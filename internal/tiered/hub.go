package tiered

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
)

// Hub owns one Router per tiered-configured provider and manages lifecycle across config reloads.
type Hub struct {
	dataDir   string
	log       *slog.Logger
	transport *http.Transport

	mu      sync.RWMutex
	routers map[string]*Router
}

// NewHub builds a Hub for managing tiered provider routers. No goroutines start until Activate.
func NewHub(dataDir string, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		dataDir:   dataDir,
		log:       log,
		transport: newDefaultTransport(),
		routers:   make(map[string]*Router),
	}
}

// Transport returns an http.RoundTripper bound lazily by provider name. Safe during config preview/validation.
func (h *Hub) Transport(provider string, upstream http.RoundTripper) http.RoundTripper {
	if upstream == nil {
		upstream = http.DefaultTransport
	}
	return &hubTransport{hub: h, provider: provider, upstream: upstream}
}

// Activate synchronizes active tiered routers with cfg (starting loops only for the live snapshot).
func (h *Hub) Activate(cfg *config.Config) {
	if cfg == nil {
		return
	}
	desired := map[string]*config.TieredConfig{}
	for i := range cfg.Providers {
		pc := &cfg.Providers[i]
		if pc.Tiered != nil {
			desired[pc.Name] = pc.Tiered
		}
	}

	h.mu.Lock()
	var toClose []*Router
	for name, r := range h.routers {
		if _, keep := desired[name]; !keep {
			toClose = append(toClose, r)
			delete(h.routers, name)
		}
	}
	existing := make(map[string]*Router, len(h.routers))
	for name, r := range h.routers {
		existing[name] = r
	}
	h.mu.Unlock()

	for _, r := range toClose {
		r.close()
	}

	for name, tc := range desired {
		if r, ok := existing[name]; ok {
			if err := r.apply(*tc); err != nil {
				h.log.Error("tiered router apply failed", "provider", name, "err", err)
			}
			continue
		}
		defPath := filepath.Join(h.dataDir, "tiered", name+"-starters.json")
		r, err := newRouter(name, *tc, defPath, h.transport, h.log)
		if err != nil {
			h.log.Error("tiered router init failed", "provider", name, "err", err)
			continue
		}
		r.ensureLoops()
		h.mu.Lock()
		// double-check race
		if old, dup := h.routers[name]; dup {
			h.mu.Unlock()
			r.close()
			_ = old.apply(*tc)
			continue
		}
		h.routers[name] = r
		h.mu.Unlock()
	}
}

// Router looks up an active tiered router by provider name.
func (h *Hub) Router(name string) (*Router, bool) {
	h.mu.RLock()
	r, ok := h.routers[name]
	h.mu.RUnlock()
	return r, ok
}

// Statuses returns status snapshots for all active tiered providers, sorted by name.
func (h *Hub) Statuses() []ProviderStatus {
	h.mu.RLock()
	names := make([]string, 0, len(h.routers))
	list := make([]*Router, 0, len(h.routers))
	for n, r := range h.routers {
		names = append(names, n)
		_ = r
	}
	sort.Strings(names)
	for _, n := range names {
		list = append(list, h.routers[n])
	}
	h.mu.RUnlock()

	out := make([]ProviderStatus, 0, len(list))
	for _, r := range list {
		out = append(out, r.Status())
	}
	return out
}

// Close shuts down background loops and flushes persisted starters to disk.
func (h *Hub) Close() error {
	h.mu.Lock()
	all := make([]*Router, 0, len(h.routers))
	for _, r := range h.routers {
		all = append(all, r)
	}
	h.routers = make(map[string]*Router)
	h.mu.Unlock()

	for _, r := range all {
		r.close()
	}
	h.transport.CloseIdleConnections()
	return nil
}

type hubTransport struct {
	hub      *Hub
	provider string
	upstream http.RoundTripper
}

func (t *hubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r, ok := t.hub.Router(t.provider)
	if !ok {
		return t.upstream.RoundTrip(req)
	}
	return r.roundTrip(req, t.upstream)
}

func readAndRestoreBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxBodyBytes))
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	return body, err
}

func targetChatURL(target *url.URL, reqURL *url.URL) string {
	u := *target
	if strings.HasSuffix(reqURL.Path, "/chat/completions") {
		base := strings.TrimRight(target.Path, "/")
		if strings.HasSuffix(base, "/v1") {
			u.Path = base + "/chat/completions"
		} else {
			u.Path = base + "/v1/chat/completions"
		}
	} else {
		base := strings.TrimRight(target.Path, "/")
		u.Path = base + reqURL.Path
	}
	u.RawQuery = reqURL.RawQuery
	return u.String()
}

func syntheticJSONResp(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func (r *Router) roundTrip(req *http.Request, upstream http.RoundTripper) (*http.Response, error) {
	cfg := r.getConfig()
	if cfg.Mode == "active" {
		return r.roundTripActive(req)
	}
	return r.roundTripShadow(req, upstream)
}

func (r *Router) roundTripActive(req *http.Request) (*http.Response, error) {
	start := time.Now()
	body, err := readAndRestoreBody(req)
	if err != nil {
		return nil, err
	}

	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		b := r.pickHighestHealthyBackend()
		if b == nil {
			return syntheticJSONResp(req, http.StatusServiceUnavailable, `{"error":{"message":"no healthy backend available","type":"tiered_unavailable"}}`), nil
		}
		_, targetURL := b.snapshot()
		outReq, rerr := http.NewRequestWithContext(req.Context(), req.Method, targetChatURL(targetURL, req.URL), bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		outReq.Header = req.Header.Clone()
		outReq.ContentLength = int64(len(body))
		return r.transport.RoundTrip(outReq)
	}

	scCfg := r.starters.getCfg()
	toolsReordered := false
	if scCfg.CanonicalizeTools {
		if canonBody, changed, _ := canonicalizeTools(body); changed {
			body = canonBody
			toolsReordered = true
			r.starters.toolsReordered.Add(1)
		}
	}

	var starterTmpl *StarterTemplate
	if scCfg.Enabled {
		starterTmpl = extractStarterTemplate(body, req.Header.Get("User-Agent"), scCfg.MinTokens, scCfg.MaxTokens)
	}

	an, ok := r.analyzeChatBody(body)
	if !ok {
		b := r.pickHighestHealthyBackend()
		if b == nil {
			return syntheticJSONResp(req, http.StatusServiceUnavailable, `{"error":{"message":"no healthy backend available","type":"tiered_unavailable"}}`), nil
		}
		_, targetURL := b.snapshot()
		outReq, rerr := http.NewRequestWithContext(req.Context(), req.Method, targetChatURL(targetURL, req.URL), bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		outReq.Header = req.Header.Clone()
		outReq.ContentLength = int64(len(body))
		return r.transport.RoundTrip(outReq)
	}

	sendBody := body
	zhAnchored := false
	if zhAnchorEnabled(r.getConfig()) {
		if anchoredBody, changed := injectZhThinkAnchor(body); changed {
			sendBody = anchoredBody
			zhAnchored = true
		}
	}

	backends := r.getBackends()
	if len(backends) == 0 {
		return syntheticJSONResp(req, http.StatusServiceUnavailable, `{"error":{"message":"no healthy backend available","type":"tiered_unavailable"}}`), nil
	}
	highestTier := backends[len(backends)-1].config().Tier
	exclude := make(map[string]bool)

	for attempt := 0; attempt < len(backends); attempt++ {
		backend, reason, isHeavy := r.selectBackend(an, exclude)
		if backend == nil {
			return syntheticJSONResp(req, http.StatusServiceUnavailable, `{"error":{"message":"no healthy backend available","type":"tiered_unavailable"}}`), nil
		}
		bCfg, targetURL := backend.snapshot()

		backend.activeReqs.Add(1)
		backend.totalRouted.Add(1)
		var heavyReleased atomic.Bool
		if isHeavy {
			backend.activeHeavy.Add(1)
			backend.heavyRouted.Add(1)
		}
		releaseHeavy := func() {
			if isHeavy && heavyReleased.CompareAndSwap(false, true) {
				backend.activeHeavy.Add(-1)
			}
		}

		isHighestTier := bCfg.Tier >= highestTier
		attemptBody, clampedMaxOut := clampMaxTokensForBackend(sendBody, an.TotalEst, an.MaxOutTokens, bCfg.MaxContextTokens, isHighestTier)

		outReq, rerr := http.NewRequestWithContext(req.Context(), req.Method, targetChatURL(targetURL, req.URL), bytes.NewReader(attemptBody))
		if rerr != nil {
			releaseHeavy()
			backend.activeReqs.Add(-1)
			exclude[bCfg.ID] = true
			continue
		}
		outReq.Header = req.Header.Clone()
		outReq.Header.Del("Transfer-Encoding")
		outReq.ContentLength = int64(len(attemptBody))

		resp, rerr := r.transport.RoundTrip(outReq)
		if rerr != nil {
			releaseHeavy()
			backend.activeReqs.Add(-1)
			r.log.Warn("tiered backend roundtrip failed, failing over", "provider", r.name, "attempt", attempt, "backend", bCfg.ID, "err", rerr)
			exclude[bCfg.ID] = true
			continue
		}

		if resp.StatusCode == http.StatusBadRequest && bCfg.Tier < highestTier {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
			if isContextLengthError(resp.StatusCode, errBody) {
				releaseHeavy()
				backend.activeReqs.Add(-1)
				r.log.Info("backend returned context length error, retrying larger tier", "provider", r.name, "backend", bCfg.ID, "est", an.TotalEst)
				for _, b := range backends {
					otherCfg := b.config()
					if otherCfg.Tier <= bCfg.Tier {
						exclude[otherCfg.ID] = true
					}
				}
				continue
			}
			releaseHeavy()
			backend.activeReqs.Add(-1)
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
			resp.ContentLength = int64(len(errBody))
			setRouterHeaders(resp.Header, bCfg.ID, reason, an, toolsReordered, clampedMaxOut, zhAnchored)
			return resp, nil
		}

		if (resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout) && attempt+1 < len(backends) {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			releaseHeavy()
			backend.activeReqs.Add(-1)
			r.log.Warn("tiered backend bad gateway status, failing over", "provider", r.name, "backend", bCfg.ID, "status", resp.StatusCode)
			exclude[bCfg.ID] = true
			continue
		}

		setRouterHeaders(resp.Header, bCfg.ID, reason, an, toolsReordered, clampedMaxOut, zhAnchored)
		resp.Body = &trackedBody{
			inner:        resp.Body,
			releaseHeavy: releaseHeavy,
			onDone: func(completed bool, tail []byte) {
				releaseHeavy()
				backend.activeReqs.Add(-1)
				actualPrompt, actualCached := extractUsageFromTail(tail)
				if completed && resp.StatusCode >= 200 && resp.StatusCode < 300 {
					r.recordCompletion(an, bCfg.ID, bCfg.Tier, isHeavy, actualPrompt, actualCached)
					if starterTmpl != nil {
						r.starters.Observe(starterTmpl)
					}
				}
				durMs := time.Since(start).Milliseconds()
				r.log.Info("tiered route", "provider", r.name, "mode", "active", "backend", bCfg.ID, "reason", reason,
					"est", an.TotalEst, "cached_est", an.CachedEst, "miss_est", an.MissEst,
					"actual_prompt", actualPrompt, "actual_cached", actualCached, "status", resp.StatusCode, "ms", durMs)
				r.appendLog(RouteLogEntry{
					Time:         time.Now().Format("15:04:05"),
					Backend:      bCfg.ID,
					Reason:       reason,
					TotalEst:     an.TotalEst,
					CachedEst:    an.CachedEst,
					MissEst:      an.MissEst,
					ActualPrompt: actualPrompt,
					ActualCache:  actualCached,
					Status:       resp.StatusCode,
					DurationMs:   durMs,
				})
			},
		}
		return resp, nil
	}

	return syntheticJSONResp(req, http.StatusBadGateway, `{"error":{"message":"all backends failed","type":"tiered_bad_gateway"}}`), nil
}

func (r *Router) roundTripShadow(req *http.Request, upstream http.RoundTripper) (*http.Response, error) {
	start := time.Now()
	origBody, err := readAndRestoreBody(req)
	if err != nil {
		return nil, err
	}

	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return upstream.RoundTrip(req)
	}

	scCfg := r.starters.getCfg()
	workBody := append([]byte(nil), origBody...)
	if scCfg.CanonicalizeTools {
		if canonBody, changed, _ := canonicalizeTools(workBody); changed {
			workBody = canonBody
			r.starters.toolsReordered.Add(1)
		}
	}
	var starterTmpl *StarterTemplate
	if scCfg.Enabled {
		starterTmpl = extractStarterTemplate(workBody, req.Header.Get("User-Agent"), scCfg.MinTokens, scCfg.MaxTokens)
	}
	an, ok := r.analyzeChatBody(workBody)
	var predID, predReason string
	if ok {
		if pb, pr, _ := r.selectBackend(an, nil); pb != nil {
			predID = pb.config().ID
			predReason = pr
		}
	}

	// Ensure upstream receives exact ORIGINAL bytes unmodified.
	req.Body = io.NopCloser(bytes.NewReader(origBody))
	req.ContentLength = int64(len(origBody))

	resp, err := upstream.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if !ok {
		return resp, nil
	}

	actualID := resp.Header.Get("X-Router-Backend")
	actualReason := resp.Header.Get("X-Router-Reason")
	r.recordShadow(predID, predReason, actualID, actualReason, an)

	actualBackend := r.backendByID(actualID)
	actualTier := 0
	actualHeavy := strings.HasPrefix(actualReason, "heavy-miss")
	var heavyReleased atomic.Bool
	if actualBackend != nil {
		actualTier = actualBackend.config().Tier
		actualBackend.activeReqs.Add(1)
		actualBackend.totalRouted.Add(1)
		if actualHeavy {
			actualBackend.activeHeavy.Add(1)
			actualBackend.heavyRouted.Add(1)
		}
	}
	releaseHeavy := func() {
		if actualBackend != nil && actualHeavy && heavyReleased.CompareAndSwap(false, true) {
			actualBackend.activeHeavy.Add(-1)
		}
	}

	resp.Body = &trackedBody{
		inner:        resp.Body,
		releaseHeavy: releaseHeavy,
		onDone: func(completed bool, tail []byte) {
			releaseHeavy()
			if actualBackend != nil {
				actualBackend.activeReqs.Add(-1)
			}
			actualPrompt, actualCached := extractUsageFromTail(tail)
			if completed && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if actualID != "" {
					r.recordCompletion(an, actualID, actualTier, actualHeavy, actualPrompt, actualCached)
				}
				if starterTmpl != nil {
					r.starters.Observe(starterTmpl)
				}
			}
			durMs := time.Since(start).Milliseconds()
			if actualID != "" {
				r.appendLog(RouteLogEntry{
					Time:         time.Now().Format("15:04:05"),
					Backend:      actualID,
					Reason:       actualReason,
					TotalEst:     an.TotalEst,
					CachedEst:    an.CachedEst,
					MissEst:      an.MissEst,
					ActualPrompt: actualPrompt,
					ActualCache:  actualCached,
					Status:       resp.StatusCode,
					DurationMs:   durMs,
				})
			}
		},
	}
	return resp, nil
}

func setRouterHeaders(h http.Header, backendID, reason string, an *RequestAnalysis, toolsReordered bool, clampedMaxOut int, zhAnchored bool) {
	h.Set("X-Router-Backend", backendID)
	h.Set("X-Router-Reason", reason)
	h.Set("X-Router-Est-Tokens", fmt.Sprintf("%d", an.TotalEst))
	h.Set("X-Router-Miss-Tokens", fmt.Sprintf("%d", an.MissEst))
	if toolsReordered {
		h.Set("X-Router-Tools-Canonicalized", "1")
	}
	if clampedMaxOut > 0 {
		h.Set("X-Router-Max-Tokens-Clamped", fmt.Sprintf("%d->%d", an.MaxOutTokens, clampedMaxOut))
	}
	if zhAnchored {
		h.Set("X-Router-Zh-Anchor", "1")
	}
}

const maxBodyTail = 16384

type trackedBody struct {
	inner        io.ReadCloser
	releaseHeavy func()
	onDone       func(completed bool, tail []byte)

	mu        sync.Mutex
	tail      []byte
	firstSeen bool
	doneOnce  sync.Once
}

func (tb *trackedBody) Read(p []byte) (int, error) {
	n, err := tb.inner.Read(p)
	if n > 0 {
		tb.mu.Lock()
		if !tb.firstSeen {
			tb.firstSeen = true
			if tb.releaseHeavy != nil {
				tb.releaseHeavy()
			}
		}
		if n >= maxBodyTail {
			tb.tail = append(tb.tail[:0], p[n-maxBodyTail:n]...)
		} else {
			tb.tail = append(tb.tail, p[:n]...)
			if len(tb.tail) > maxBodyTail {
				tb.tail = append([]byte(nil), tb.tail[len(tb.tail)-maxBodyTail:]...)
			}
		}
		tb.mu.Unlock()
	}
	if err == io.EOF {
		tb.finish(true)
	} else if err != nil {
		tb.finish(false)
	}
	return n, err
}

func (tb *trackedBody) Close() error {
	err := tb.inner.Close()
	tb.finish(false)
	return err
}

func (tb *trackedBody) finish(completed bool) {
	tb.doneOnce.Do(func() {
		tb.mu.Lock()
		tailCopy := append([]byte(nil), tb.tail...)
		tb.mu.Unlock()
		if tb.onDone != nil {
			tb.onDone(completed, tailCopy)
		}
	})
}
