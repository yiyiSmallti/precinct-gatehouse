package updater

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// NewHTTPClient creates an HTTP client that:
//  1. Falls back to resolving hostnames over UDP via fallbackDNS if OS DNS fails.
//  2. Strips the Authorization header on cross-origin redirects (e.g. Aliyun OSS / Tencent COS).
func NewHTTPClient(timeout time.Duration, fallbackDNS []string) *http.Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	baseDialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	var dnsServers []string
	for _, srv := range fallbackDNS {
		s := strings.TrimSpace(srv)
		if s == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			s = net.JoinHostPort(s, "53")
		}
		dnsServers = append(dnsServers, s)
	}

	var cacheMu sync.Mutex
	dnsCache := map[string][]string{}

	lookupFallback := func(ctx context.Context, host string) ([]string, error) {
		key := strings.ToLower(strings.TrimSuffix(host, "."))
		cacheMu.Lock()
		cached := dnsCache[key]
		cacheMu.Unlock()
		if len(cached) > 0 {
			return cached, nil
		}

		var lastErr error
		for _, srv := range dnsServers {
			serverAddr := srv
			res := &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: 3 * time.Second}
					return d.DialContext(ctx, "udp", serverAddr)
				},
			}
			lctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			addrs, err := res.LookupHost(lctx, host)
			cancel()
			if err == nil && len(addrs) > 0 {
				cacheMu.Lock()
				dnsCache[key] = addrs
				cacheMu.Unlock()
				return addrs, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = errors.New("no fallback DNS servers configured")
		}
		return nil, fmt.Errorf("fallback DNS lookup for %s failed: %w", host, lastErr)
	}

	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := baseDialer.DialContext(ctx, network, addr)
			if err == nil || len(dnsServers) == 0 {
				return conn, err
			}
			host, port, splitErr := net.SplitHostPort(addr)
			if splitErr != nil || net.ParseIP(host) != nil {
				return nil, err
			}
			var dnsErr *net.DNSError
			if !errors.As(err, &dnsErr) {
				return nil, err
			}
			ips, fbErr := lookupFallback(ctx, host)
			if fbErr != nil {
				return nil, err
			}
			var dialErr error
			for _, ip := range ips {
				conn, dialErr = baseDialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
				if dialErr == nil {
					return conn, nil
				}
			}
			if dialErr != nil {
				return nil, dialErr
			}
			return nil, err
		},
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if len(via) > 0 && !sameOrigin(via[0].URL, req.URL) {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}
