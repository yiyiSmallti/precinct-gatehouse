package tiered

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
)

func TestSnapshotTransportIsolation(t *testing.T) {
	var localHits, extHits atomic.Int32
	mkServer := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/get_server_info" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			counter.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
		}))
	}
	localSrv := mkServer(&localHits)
	defer localSrv.Close()
	extSrv := mkServer(&extHits)
	defer extSrv.Close()

	hub := NewHub(t.TempDir(), nil)
	defer func() { _ = hub.Close() }()

	makeCfg := func(rawURL string) config.TieredConfig {
		f := false
		return config.TieredConfig{
			Mode:               "active",
			HeavyMissThreshold: 24000,
			HealthIntervalSec:  60,
			ZhThinkAnchor:      &f,
			StarterCache:       &config.TieredStarterCache{Enabled: false},
			Backends: []config.TieredBackend{
				{
					ID:               "b1",
					URL:              rawURL,
					Tier:             1,
					MaxContextTokens: 64000,
					MaxSafeInput:     50000,
					PromoteAt:        64000,
					SpeedScore:       100,
				},
			},
		}
	}

	cfg1 := makeCfg(localSrv.URL)
	tr1, err := hub.Transport("p", &cfg1, nil)
	if err != nil {
		t.Fatalf("Transport(cfg1): %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Tiered: &cfg1}}})

	// Hot reload changes the same backend ID ("b1") to extSrv.
	cfg2 := makeCfg(extSrv.URL)
	tr2, err := hub.Transport("p", &cfg2, nil)
	if err != nil {
		t.Fatalf("Transport(cfg2): %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Tiered: &cfg2}}})

	call := func(tr http.RoundTripper) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected status: %d", resp.StatusCode)
		}
	}

	call(tr1)
	if localHits.Load() != 1 || extHits.Load() != 0 {
		t.Fatalf("expected tr1 to dispatch only to localSrv after reload, got local=%d ext=%d", localHits.Load(), extHits.Load())
	}

	call(tr2)
	if localHits.Load() != 1 || extHits.Load() != 1 {
		t.Fatalf("expected tr2 to dispatch to extSrv, got local=%d ext=%d", localHits.Load(), extHits.Load())
	}

	// Removing tiered from live config must not break an in-flight request bound to Snapshot 1.
	hub.Activate(&config.Config{})
	call(tr1)
	if localHits.Load() != 2 || extHits.Load() != 1 {
		t.Fatalf("expected tr1 to continue using Snapshot 1 after provider removal, got local=%d ext=%d", localHits.Load(), extHits.Load())
	}
}
