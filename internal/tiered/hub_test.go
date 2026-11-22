package tiered

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	tr1, err := hub.Transport("p", "local", &cfg1, nil)
	if err != nil {
		t.Fatalf("Transport(cfg1): %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &cfg1}}})

	// Hot reload changes the same backend ID ("b1") to extSrv.
	cfg2 := makeCfg(extSrv.URL)
	tr2, err := hub.Transport("p", "external", &cfg2, nil)
	if err != nil {
		t.Fatalf("Transport(cfg2): %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "external", Tiered: &cfg2}}})

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

func TestStarterCacheTrustBoundaryOnReload(t *testing.T) {
	var localHits, extHits atomic.Int32
	mkServer := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/get_server_info" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			counter.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":2,"total_tokens":122}}`))
		}))
	}
	localSrv := mkServer(&localHits)
	defer localSrv.Close()
	extSrv := mkServer(&extHits)
	defer extSrv.Close()

	dir := t.TempDir()
	persistPath := filepath.Join(dir, "starters.json")
	hub := NewHub(dir, nil)
	defer func() { _ = hub.Close() }()

	makeCfg := func(rawURL string) config.TieredConfig {
		f := false
		return config.TieredConfig{
			Mode:               "active",
			HeavyMissThreshold: 24000,
			HealthIntervalSec:  60,
			ZhThinkAnchor:      &f,
			StarterCache: &config.TieredStarterCache{
				Enabled:          true,
				MinTokens:        32,
				MaxTokens:        100000,
				MinHits:          1,
				KeepAliveSec:     240,
				CheckIntervalSec: 600,
				StartupDelaySec:  600,
				PersistPath:      persistPath,
			},
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

	cfgLocal := makeCfg(localSrv.URL)
	trLocal, err := hub.Transport("p", "local", &cfgLocal, nil)
	if err != nil {
		t.Fatalf("Transport(local): %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &cfgLocal}}})

	sysPrompt := strings.Repeat("Confidential internal system prompt sk-live-secret-9988776655. ", 12)
	chatBody, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": sysPrompt},
			{"role": "user", "content": "hi"},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := trLocal.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip local: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	rLocal, ok := hub.Router("p")
	if !ok || len(rLocal.Starters()) != 1 {
		t.Fatalf("expected 1 local starter captured, got ok=%v len=%d", ok, len(rLocal.Starters()))
	}
	rLocal.starters.saveToDisk()

	// Switch provider target to external: unmasked local starters must be purged and never warmed to extSrv.
	cfgExt := makeCfg(extSrv.URL)
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "external", Tiered: &cfgExt}}})

	rExt, ok := hub.Router("p")
	if !ok {
		t.Fatal("expected router p after external reload")
	}
	if got := len(rExt.Starters()); got != 0 {
		t.Fatalf("expected non-external starter entries to be purged on external reload, got %d", got)
	}
	if res := rExt.starters.WarmDue(true, ""); len(res) != 0 {
		t.Fatalf("expected 0 warm results after purge, got %+v", res)
	}
	if extHits.Load() != 0 {
		t.Fatalf("expected 0 requests to extSrv from local starter cache, got %d", extHits.Load())
	}

	// Simulate legacy persisted file without origin_target loaded into an external router: must also be dropped.
	legacy, _ := json.Marshal([]map[string]any{{
		"hash":        "legacyhash1234",
		"model":       "m1",
		"system_msgs": []json.RawMessage{json.RawMessage(`{"role":"system","content":"unmasked legacy internal secret"}`)},
		"est_tokens":  128,
		"hits":        3,
	}})
	if err := os.WriteFile(persistPath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	hub2 := NewHub(dir, nil)
	defer func() { _ = hub2.Close() }()
	hub2.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "external", Tiered: &cfgExt}}})
	rExt2, ok := hub2.Router("p")
	if !ok || len(rExt2.Starters()) != 0 {
		t.Fatalf("expected legacy starter without origin_target dropped by external router, got ok=%v len=%d", ok, len(rExt2.Starters()))
	}
	if res := rExt2.starters.WarmDue(true, ""); len(res) != 0 || extHits.Load() != 0 {
		t.Fatalf("expected no warmup to extSrv from legacy starter, got res=%+v extHits=%d", res, extHits.Load())
	}
}
