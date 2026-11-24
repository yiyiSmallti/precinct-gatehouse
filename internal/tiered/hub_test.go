package tiered

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestStarterWarmupUsesProviderAPIKey(t *testing.T) {
	const wantKey = "secret-tiered-backend-key"
	t.Setenv("TEST_TIERED_KEY", wantKey)

	var unauthCount, warmOK atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			unauthCount.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.Contains(r.Header.Get("User-Agent"), "warmer") {
			warmOK.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":0}}}`))
	}))
	defer up.Close()

	dir := t.TempDir()
	hub := NewHub(dir, nil)
	defer func() { _ = hub.Close() }()

	f := false
	tc := config.TieredConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      &f,
		StarterCache: &config.TieredStarterCache{
			Enabled:          true,
			MinTokens:        32,
			MaxTokens:        100000,
			MinHits:          1,
			CheckIntervalSec: 600,
			StartupDelaySec:  600,
			PersistPath:      filepath.Join(dir, "starters.json"),
		},
		Backends: []config.TieredBackend{
			{
				ID:               "b1",
				URL:              up.URL,
				Tier:             1,
				MaxContextTokens: 64000,
				MaxSafeInput:     50000,
				PromoteAt:        64000,
				SpeedScore:       100,
			},
		},
	}

	tr, err := hub.Transport("p", "local", &tc, nil)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{
		Name:   "p",
		Target: "local",
		KeyRef: "env:TEST_TIERED_KEY",
		Tiered: &tc,
	}}})

	r, ok := hub.Router("p")
	if !ok {
		t.Fatal("expected router p")
	}
	if !r.getBackends()[0].healthy.Load() {
		t.Fatalf("expected authenticated /get_server_info probe to mark backend healthy (unauth=%d)", unauthCount.Load())
	}

	sysPrompt := strings.Repeat("Authenticated starter warmup prefix instruction. ", 12)
	chatBody, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": sysPrompt},
			{"role": "user", "content": "hi"},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+wantKey)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	starters := r.Starters()
	if len(starters) != 1 {
		t.Fatalf("expected 1 starter captured, got %d", len(starters))
	}
	warmRes, err := r.WarmStarter(starters[0].Hash, true)
	if err != nil || len(warmRes) != 1 || warmRes[0].Error != "" {
		t.Fatalf("expected authenticated warmup to succeed, got err=%v res=%+v", err, warmRes)
	}
	if warmOK.Load() != 1 || unauthCount.Load() != 0 {
		t.Fatalf("expected warmOK=1 unauthCount=0, got warmOK=%d unauthCount=%d", warmOK.Load(), unauthCount.Load())
	}
}

func TestUnhealthyFallbackHonorsExclusions(t *testing.T) {
	var smallHits, largeHits atomic.Int32
	smallSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		smallHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"from-small"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
	}))
	defer smallSrv.Close()

	largeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		largeHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"busy"}`))
	}))
	defer largeSrv.Close()

	hub := NewHub(t.TempDir(), nil)
	defer func() { _ = hub.Close() }()

	f := false
	tc := config.TieredConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      &f,
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		Backends: []config.TieredBackend{
			{ID: "small", URL: smallSrv.URL, Tier: 1, MaxContextTokens: 64000, MaxSafeInput: 50000, PromoteAt: 50000, SpeedScore: 100},
			{ID: "large", URL: largeSrv.URL, Tier: 2, MaxContextTokens: 128000, MaxSafeInput: 100000, PromoteAt: 100000, SpeedScore: 50},
		},
	}

	tr, err := hub.Transport("p", "local", &tc, nil)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &tc}}})

	req, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "from-small") {
		t.Fatalf("expected failover from unhealthy large(503) to unhealthy small(200), got status=%d body=%s", resp.StatusCode, body)
	}
	if largeHits.Load() != 1 || smallHits.Load() != 1 {
		t.Fatalf("expected 1 attempt on large and 1 failover attempt on small, got large=%d small=%d", largeHits.Load(), smallHits.Load())
	}
}

func TestStarterWarmupWithV1BaseURL(t *testing.T) {
	var pathsMu sync.Mutex
	var paths []string
	var doubleV1Hits atomic.Int32
	var warmHits atomic.Int32
	var infoHits atomic.Int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		switch r.URL.Path {
		case "/get_server_info":
			infoHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		case "/v1/v1/chat/completions":
			doubleV1Hits.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		case "/v1/chat/completions":
			_, _ = io.Copy(io.Discard, r.Body)
			if strings.Contains(r.Header.Get("User-Agent"), "warmer") {
				warmHits.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":2,"total_tokens":122,"prompt_tokens_details":{"cached_tokens":100}}}`))
			return
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer up.Close()

	hub := NewHub(t.TempDir(), nil)
	defer func() { _ = hub.Close() }()

	f := false
	tc := config.TieredConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      &f,
		StarterCache: &config.TieredStarterCache{
			Enabled:          true,
			MinTokens:        16,
			MaxTokens:        100000,
			MinHits:          1,
			CheckIntervalSec: 600,
			StartupDelaySec:  600,
		},
		Backends: []config.TieredBackend{
			{ID: "b1", URL: up.URL + "/v1", Tier: 1, MaxContextTokens: 64000, MaxSafeInput: 50000, PromoteAt: 50000, SpeedScore: 100},
		},
	}

	tr, err := hub.Transport("p", "local", &tc, nil)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &tc}}})

	// Populate starter cache via a normal active chat request.
	sys := strings.Repeat("Reusable system prompt context for v1 normalization test. ", 8)
	payload, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": sys},
			{"role": "user", "content": "hi"},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	r, ok := hub.Router("p")
	if !ok {
		t.Fatal("missing router p")
	}
	starters := r.Starters()
	if len(starters) != 1 {
		t.Fatalf("expected 1 starter captured, got %d", len(starters))
	}
	warmRes, err := r.WarmStarter(starters[0].Hash, true)
	if err != nil || len(warmRes) != 1 || warmRes[0].Error != "" {
		t.Fatalf("expected starter warmup to succeed, got err=%v res=%+v", err, warmRes)
	}
	if doubleV1Hits.Load() != 0 {
		t.Fatalf("expected 0 hits to /v1/v1/chat/completions, got %d", doubleV1Hits.Load())
	}
	if infoHits.Load() < 1 || warmHits.Load() != 1 {
		pathsMu.Lock()
		seen := append([]string(nil), paths...)
		pathsMu.Unlock()
		t.Fatalf("expected /get_server_info >=1 and warmup /v1/chat/completions == 1, got info=%d warm=%d paths=%v", infoHits.Load(), warmHits.Load(), seen)
	}
}

func TestProbeBackendsTimeoutToleranceByLoad(t *testing.T) {
	var return503 atomic.Bool
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if return503.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		select {
		case <-time.After(150 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer slowSrv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	timeoutClient := &http.Client{Timeout: 25 * time.Millisecond}

	newState := func(maxCtx int) *BackendState {
		bs := &BackendState{cfg: BackendConfig{ID: "b1", URL: slowSrv.URL, Tier: 1, MaxContextTokens: maxCtx}}
		bs.healthy.Store(true)
		return bs
	}

	// 1. Idle normal-context active backend tolerates 2 timeouts, evicted on 3rd.
	b := newState(200000)
	probeBackends([]*BackendState{b}, "active", 0, "", timeoutClient, logger, "p")
	probeBackends([]*BackendState{b}, "active", 0, "", timeoutClient, logger, "p")
	if !b.healthy.Load() {
		t.Fatal("expected idle backend to remain healthy through 2 probe timeouts")
	}
	probeBackends([]*BackendState{b}, "active", 0, "", timeoutClient, logger, "p")
	if b.healthy.Load() {
		t.Fatal("expected idle backend to become unhealthy on 3rd timeout")
	}

	// 2. activeReqs > 0 raises tolerance to 12; activeHeavy > 0 raises to 24.
	b = newState(200000)
	b.activeReqs.Store(1)
	for i := 0; i < 11; i++ {
		probeBackends([]*BackendState{b}, "active", 0, "", timeoutClient, logger, "p")
	}
	if !b.healthy.Load() {
		t.Fatal("expected activeReqs>0 backend to stay healthy across 11 probe timeouts")
	}
	probeBackends([]*BackendState{b}, "active", 0, "", timeoutClient, logger, "p")
	if b.healthy.Load() {
		t.Fatal("expected activeReqs>0 backend to become unhealthy on 12th timeout")
	}

	b = newState(200000)
	b.activeHeavy.Store(1)
	for i := 0; i < 23; i++ {
		probeBackends([]*BackendState{b}, "active", 0, "", timeoutClient, logger, "p")
	}
	if !b.healthy.Load() {
		t.Fatal("expected activeHeavy>0 backend to stay healthy across 23 probe timeouts")
	}
	probeBackends([]*BackendState{b}, "active", 0, "", timeoutClient, logger, "p")
	if b.healthy.Load() {
		t.Fatal("expected activeHeavy>0 backend to become unhealthy on 24th timeout")
	}

	// 3. Shadow mode / >=512K context tolerates 12 timeouts even when counters are 0.
	b = newState(1048576)
	for i := 0; i < 6; i++ {
		probeBackends([]*BackendState{b}, "shadow", 0, "", timeoutClient, logger, "p")
	}
	if !b.healthy.Load() {
		t.Fatal("expected shadow/large-context backend to tolerate 6 probe timeouts")
	}

	// 4. Immediate eviction on HTTP 503 or connection error even with activeHeavy > 0.
	b = newState(1048576)
	b.activeHeavy.Store(1)
	return503.Store(true)
	probeBackends([]*BackendState{b}, "active", 0, "", timeoutClient, logger, "p")
	if b.healthy.Load() {
		t.Fatal("expected non-2xx HTTP 503 to mark backend unhealthy immediately")
	}
}

func TestClientCancel499SkipsFailover(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var t2Hits atomic.Int32

	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(release)
		srv1.Close()
	}()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		t2Hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c2","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv2.Close()

	hub := NewHub(t.TempDir(), nil)
	defer func() { _ = hub.Close() }()

	f := false
	tc := config.TieredConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      &f,
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		Backends: []config.TieredBackend{
			{ID: "t1", URL: srv1.URL, Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 185000, PromoteAt: 150000, SpeedScore: 55},
			{ID: "t2", URL: srv2.URL, Tier: 2, MaxContextTokens: 1048576, MaxSafeInput: 1048576, PromoteAt: 1048576, SpeedScore: 110},
		},
	}

	tr, err := hub.Transport("p", "local", &tc, nil)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &tc}}})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")

	go func() {
		<-started
		cancel()
	}()

	_, err = tr.RoundTrip(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if t2Hits.Load() != 0 {
		t.Fatalf("expected 0 failover requests to t2 on client 499 cancel, got %d", t2Hits.Load())
	}
	r, _ := hub.Router("p")
	for _, b := range r.getBackends() {
		if b.activeReqs.Load() != 0 || b.activeHeavy.Load() != 0 {
			t.Fatalf("expected counters released on cancel for %s, got reqs=%d heavy=%d", b.config().ID, b.activeReqs.Load(), b.activeHeavy.Load())
		}
	}
}

func TestHeavyWriteStickinessWindow(t *testing.T) {
	mkBackend := func(id string, promptToks, cachedToks int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/get_server_info" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			resp, _ := json.Marshal(map[string]any{
				"id":     "c-" + id,
				"object": "chat.completion",
				"choices": []map[string]any{{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": "from-" + id},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{
					"prompt_tokens":         promptToks,
					"completion_tokens":     2,
					"total_tokens":          promptToks + 2,
					"prompt_tokens_details": map[string]any{"cached_tokens": cachedToks},
				},
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(resp)
		}))
	}
	t1Srv := mkBackend("t1", 200, 180)
	defer t1Srv.Close()
	t2Srv := mkBackend("t2", 200, 0)
	defer t2Srv.Close()

	hub := NewHub(t.TempDir(), nil)
	defer func() { _ = hub.Close() }()

	f := false
	tc := config.TieredConfig{
		Mode:               "active",
		HeavyMissThreshold: 120,
		HeavyStickySec:     12,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      &f,
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		Backends: []config.TieredBackend{
			{ID: "t1", URL: t1Srv.URL, Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 185000, PromoteAt: 150000, SpeedScore: 55},
			{ID: "t2", URL: t2Srv.URL, Tier: 2, MaxContextTokens: 1048576, MaxSafeInput: 1048576, PromoteAt: 1048576, SpeedScore: 110},
		},
	}

	tr, err := hub.Transport("p", "local", &tc, nil)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &tc}}})

	// Turn 1: heavy miss (>120 tokens) goes to fastest writer t2.
	turn1Body, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": "Static system block"},
			{"role": "user", "content": strings.Repeat("heavy miss initial prompt text ", 40)},
		},
	})
	req1, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader(turn1Body))
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := tr.RoundTrip(req1)
	if err != nil {
		t.Fatalf("turn1 RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	if got := resp1.Header.Get("X-Router-Backend"); got != "t2" {
		t.Fatalf("expected turn 1 heavy write on t2, got backend=%q reason=%q", got, resp1.Header.Get("X-Router-Reason"))
	}

	// Turn 2 (short append within heavy_sticky_sec): stays sticky on t2 while async Mooncake flush completes.
	turn2Body, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": "Static system block"},
			{"role": "user", "content": strings.Repeat("heavy miss initial prompt text ", 40)},
			{"role": "assistant", "content": "from-t2"},
			{"role": "user", "content": "short follow-up"},
		},
	})
	req2, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader(turn2Body))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := tr.RoundTrip(req2)
	if err != nil {
		t.Fatalf("turn2 RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if got, reason := resp2.Header.Get("X-Router-Backend"), resp2.Header.Get("X-Router-Reason"); got != "t2" || reason != "heavy-write-sticky(t2)" {
		t.Fatalf("expected heavy-write-sticky(t2) within window, got backend=%q reason=%q", got, reason)
	}

	// Age HeavyWriteAt past heavy_sticky_sec -> short turn naturally returns to t1.
	r, _ := hub.Router("p")
	an2, _ := r.analyzeChatBody(turn2Body)
	r.mu.Lock()
	if sess := r.sessions[an2.SessionKey]; sess != nil {
		sess.HeavyWriteAt = time.Now().Add(-20 * time.Second)
	}
	r.mu.Unlock()

	turn3Body, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": "Static system block"},
			{"role": "user", "content": strings.Repeat("heavy miss initial prompt text ", 40)},
			{"role": "assistant", "content": "from-t2"},
			{"role": "user", "content": "short follow-up"},
			{"role": "assistant", "content": "from-t2"},
			{"role": "user", "content": "another short follow-up"},
		},
	})
	req3, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader(turn3Body))
	req3.Header.Set("Content-Type", "application/json")
	resp3, err := tr.RoundTrip(req3)
	if err != nil {
		t.Fatalf("turn3 RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp3.Body)
	resp3.Body.Close()
	if got := resp3.Header.Get("X-Router-Backend"); got != "t1" {
		t.Fatalf("expected short turn after sticky window to migrate back to t1, got %q (reason=%q)", got, resp3.Header.Get("X-Router-Reason"))
	}
}

func TestTieredLongPrefillSSEKeepaliveAndUpstreamError(t *testing.T) {
	var failAfterDelay atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(60 * time.Millisecond)
		if failAfterDelay.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`upstream exploded`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	hub := NewHub(t.TempDir(), nil)
	defer func() { _ = hub.Close() }()

	f := false
	tc := config.TieredConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		HealthIntervalSec:  60,
		SSEKeepaliveSec:    1,
		ZhThinkAnchor:      &f,
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		Backends: []config.TieredBackend{
			{ID: "b1", URL: up.URL, Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 185000, PromoteAt: 150000, SpeedScore: 55},
		},
	}

	tr, err := hub.Transport("p", "local", &tc, nil)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &tc}}})
	r, _ := hub.Router("p")
	r.sseKeepaliveInitDelay.Store(int64(20 * time.Millisecond))

	// 1. Slow prefill followed by normal SSE stream.
	req, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	bodyStr := string(bodyBytes)
	if resp.Header.Get("X-Router-SSE-Keepalive") != "1" {
		t.Fatalf("expected X-Router-SSE-Keepalive=1 header, got %v", resp.Header)
	}
	if !strings.HasPrefix(bodyStr, ": keepalive\n\n") || !strings.Contains(bodyStr, `"content":"hello"`) {
		t.Fatalf("expected SSE keepalive followed by completion chunk, got %q", bodyStr)
	}

	// 2. Slow prefill followed by HTTP 500 after keepalive headers were sent -> emits SSE error + [DONE].
	failAfterDelay.Store(true)
	req2, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := tr.RoundTrip(req2)
	if err != nil {
		t.Fatalf("RoundTrip error case: %v", err)
	}
	errBytes, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	errStr := string(errBytes)
	if !strings.Contains(errStr, ": keepalive\n\n") || !strings.Contains(errStr, `"upstream_error"`) || !strings.Contains(errStr, "data: [DONE]") {
		t.Fatalf("expected keepalive + SSE error + [DONE], got %q", errStr)
	}
	for _, b := range r.getBackends() {
		if b.activeReqs.Load() != 0 || b.activeHeavy.Load() != 0 {
			t.Fatalf("expected counters released after keepalive stream error, got reqs=%d heavy=%d", b.activeReqs.Load(), b.activeHeavy.Load())
		}
	}
}

type hubCloseSpy struct {
	io.Reader
	closed atomic.Bool
}

func (b *hubCloseSpy) Close() error {
	b.closed.Store(true)
	return nil
}

func TestTieredKeepaliveStreamPreservesBackendFailover(t *testing.T) {
	var b1Mode atomic.Int32 // 0=503, 1=400 ctx len, 2=500 with sensitive body
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(45 * time.Millisecond)
		switch b1Mode.Load() {
		case 0:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`unavailable`))
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens"}}`))
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`SECRET_PROMPT_ECHO`))
		}
	}))
	defer b1.Close()

	var b2Down atomic.Bool
	var b2DelayAfterHeaders atomic.Int64
	b2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			if b2Down.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if b2Down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		if d := time.Duration(b2DelayAfterHeaders.Load()); d > 0 {
			time.Sleep(d)
		}
		_, _ = w.Write([]byte("data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"from-b2\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer b2.Close()

	hub := NewHub(t.TempDir(), nil)
	defer func() { _ = hub.Close() }()

	f := false
	tc := config.TieredConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		HealthIntervalSec:  60,
		SSEKeepaliveSec:    1,
		ZhThinkAnchor:      &f,
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		Backends: []config.TieredBackend{
			{ID: "b1", URL: b1.URL, Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 185000, PromoteAt: 150000, SpeedScore: 55},
			{ID: "b2", URL: b2.URL, Tier: 2, MaxContextTokens: 204800, MaxSafeInput: 185000, SpeedScore: 50},
		},
	}

	tr, err := hub.Transport("p", "local", &tc, nil)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &tc}}})
	r, _ := hub.Router("p")
	r.sseKeepaliveInitDelay.Store(int64(15 * time.Millisecond))

	// 1. Slow b1 503 after keepalive starts must fail over to b2 seamlessly and update RouteMeta.
	b1Mode.Store(0)
	meta1 := &RouteMeta{}
	req1, _ := http.NewRequestWithContext(WithRouteMeta(context.Background(), meta1), http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := tr.RoundTrip(req1)
	if err != nil {
		t.Fatalf("RoundTrip 1: %v", err)
	}
	body1Bytes, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	body1 := string(body1Bytes)
	if !strings.HasPrefix(body1, ": keepalive\n\n") || !strings.Contains(body1, `"content":"from-b2"`) {
		t.Fatalf("expected keepalive + failover content from b2 on 503, got %q", body1)
	}
	if gotB, _ := meta1.Get(); gotB != "b2" {
		t.Fatalf("expected RouteMeta backend updated to winning backend b2, got %q", gotB)
	}

	// 2. Slow b1 400 context-length error after keepalive starts must fail over to higher-tier b2.
	b1Mode.Store(1)
	meta2 := &RouteMeta{}
	req2, _ := http.NewRequestWithContext(WithRouteMeta(context.Background(), meta2), http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"ctx"}]}`)))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := tr.RoundTrip(req2)
	if err != nil {
		t.Fatalf("RoundTrip 2: %v", err)
	}
	body2Bytes, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	body2 := string(body2Bytes)
	if !strings.HasPrefix(body2, ": keepalive\n\n") || !strings.Contains(body2, `"content":"from-b2"`) {
		t.Fatalf("expected keepalive + failover content from b2 on 400 context-length, got %q", body2)
	}
	if gotB, _ := meta2.Get(); gotB != "b2" {
		t.Fatalf("expected RouteMeta backend updated to b2 on ctx failover, got %q", gotB)
	}

	// 3. Keepalives continue after 200 headers flush while waiting for the first upstream body chunk.
	r.sseKeepaliveIntervalOverride.Store(int64(20 * time.Millisecond))
	b2DelayAfterHeaders.Store(int64(90 * time.Millisecond))
	b1Mode.Store(0)
	reqPostHdr, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"posthdr"}]}`)))
	reqPostHdr.Header.Set("Content-Type", "application/json")
	respPostHdr, err := tr.RoundTrip(reqPostHdr)
	if err != nil {
		t.Fatalf("RoundTrip post-headers keepalive: %v", err)
	}
	postHdrBytes, _ := io.ReadAll(respPostHdr.Body)
	respPostHdr.Body.Close()
	postHdrBody := string(postHdrBytes)
	if strings.Count(postHdrBody, ": keepalive\n\n") < 4 || !strings.Contains(postHdrBody, `"content":"from-b2"`) {
		t.Fatalf("expected >=4 keepalive comments continuing after 200 headers until first chunk, got count=%d body=%q",
			strings.Count(postHdrBody, ": keepalive\n\n"), postHdrBody)
	}
	b2DelayAfterHeaders.Store(0)
	r.sseKeepaliveIntervalOverride.Store(0)

	// 3. Post-keepalive HTTP 500 error body must NOT be embedded in SSE error chunk.
	b1Mode.Store(2)
	b2Down.Store(true)
	req3, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"err"}]}`)))
	req3.Header.Set("Content-Type", "application/json")
	resp3, err := tr.RoundTrip(req3)
	if err != nil {
		t.Fatalf("RoundTrip 3: %v", err)
	}
	body3Bytes, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	body3 := string(body3Bytes)
	if !strings.Contains(body3, "upstream status 500") || strings.Contains(body3, "SECRET_PROMPT_ECHO") {
		t.Fatalf("expected sanitized status 500 without body leak, got %q", body3)
	}

	for _, b := range r.getBackends() {
		if b.activeReqs.Load() != 0 || b.activeHeavy.Load() != 0 {
			t.Fatalf("expected zero active counters on %s, got reqs=%d heavy=%d", b.config().ID, b.activeReqs.Load(), b.activeHeavy.Load())
		}
	}

	// 4. rtCall concurrent abort vs deliver closes late response body cleanly under -race.
	for i := 0; i < 40; i++ {
		call := &rtCall{ch: make(chan activeRTRes, 1)}
		spy := &hubCloseSpy{Reader: strings.NewReader("ok")}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			rr := &http.Response{StatusCode: http.StatusOK, Body: spy}
			call.mu.Lock()
			if call.canceled {
				call.mu.Unlock()
				_ = rr.Body.Close()
				return
			}
			call.ch <- activeRTRes{resp: rr}
			call.mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			call.abort()
		}()
		wg.Wait()
		if !spy.closed.Load() {
			t.Fatalf("iteration %d: aborted rtCall leaked open response body", i)
		}
	}
}

func TestTieredMidStreamKeepalive(t *testing.T) {
	var mode atomic.Int32 // 0=clean-pause-after-chunk, 1=split-line-pause
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		fl, _ := w.(http.Flusher)
		// Delay response headers past sseKeepaliveInitDelay (10ms) so startKeepaliveStream handles the stream.
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if mode.Load() == 0 {
			_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part1\"}}]}\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(85 * time.Millisecond)
			_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part2\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		} else {
			_, _ = io.WriteString(w, "data: {\"id\":\"1")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(75 * time.Millisecond)
			_, _ = io.WriteString(w, "23\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
		if fl != nil {
			fl.Flush()
		}
	}))
	defer up.Close()

	hub := NewHub(t.TempDir(), nil)
	defer hub.Close()
	f := false
	tc := config.TieredConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		SSEKeepaliveSec:    1,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      &f,
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		Backends: []config.TieredBackend{
			{ID: "b1", URL: up.URL, Tier: 1, MaxContextTokens: 32000, MaxSafeInput: 16000, PromoteAt: 16000, SpeedScore: 100},
		},
	}
	tr, err := hub.Transport("p", "local", &tc, nil)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &tc}}})
	r, _ := hub.Router("p")
	r.sseKeepaliveInitDelay.Store(int64(10 * time.Millisecond))
	r.sseKeepaliveIntervalOverride.Store(int64(20 * time.Millisecond))

	// 1. Mid-stream pause after SSE chunk triggers >=2 keepalives between part1 and part2.
	mode.Store(0)
	req1, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := tr.RoundTrip(req1)
	if err != nil {
		t.Fatalf("RoundTrip 1: %v", err)
	}
	raw1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	body1 := string(raw1)
	p1Idx := strings.Index(body1, `"content":"part1"`)
	p2Idx := strings.Index(body1, `"content":"part2"`)
	if p1Idx < 0 || p2Idx < 0 || p2Idx <= p1Idx {
		t.Fatalf("missing expected parts in %q", body1)
	}
	between := body1[p1Idx:p2Idx]
	if strings.Count(between, ": keepalive\n\n") < 2 {
		t.Fatalf("expected >=2 mid-stream keepalives between part1 and part2, got %q", between)
	}

	// 2. Pause inside an incomplete SSE line must NOT inject keepalive comments into the line.
	mode.Store(1)
	req2, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"split"}]}`)))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := tr.RoundTrip(req2)
	if err != nil {
		t.Fatalf("RoundTrip 2: %v", err)
	}
	raw2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	body2 := string(raw2)
	if !strings.Contains(body2, `data: {"id":"123",`) {
		t.Fatalf("expected split SSE line uncorrupted, got %q", body2)
	}
}

func makeMockTierBackend(id string, hook func(w http.ResponseWriter, r *http.Request) bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if hook != nil && hook(w, r) {
			return
		}
		resp, _ := json.Marshal(map[string]any{
			"id":     "c-" + id,
			"object": "chat.completion",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "from-" + id},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":         50,
				"completion_tokens":     2,
				"total_tokens":          52,
				"prompt_tokens_details": map[string]any{"cached_tokens": 40},
			},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
}

func postChat(t *testing.T, tr http.RoundTripper, payload []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	return resp
}

func TestMultiNodeSameTierLoadBalanceAndAffinity(t *testing.T) {
	var holdT1A sync.WaitGroup
	var blockT1A atomic.Bool
	var failT1AStatus atomic.Int32
	var t1aHits, t1bHits, t2Hits atomic.Int32

	releaseT1A := make(chan struct{})
	var t1aEntered atomic.Bool
	t1aEnteredCh := make(chan struct{}, 4)

	t1aSrv := makeMockTierBackend("t1a", func(w http.ResponseWriter, r *http.Request) bool {
		t1aHits.Add(1)
		if blockT1A.Load() {
			t1aEntered.Store(true)
			t1aEnteredCh <- struct{}{}
			<-releaseT1A
			return false
		}
		switch failT1AStatus.Load() {
		case 503:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"busy"}`))
			return true
		case 400:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 204800 tokens"}}`))
			return true
		}
		return false
	})
	defer t1aSrv.Close()

	t1bSrv := makeMockTierBackend("t1b", func(w http.ResponseWriter, r *http.Request) bool {
		t1bHits.Add(1)
		return false
	})
	defer t1bSrv.Close()

	t2Srv := makeMockTierBackend("t2", func(w http.ResponseWriter, r *http.Request) bool {
		t2Hits.Add(1)
		return false
	})
	defer t2Srv.Close()

	hub := NewHub(t.TempDir(), nil)
	defer func() { _ = hub.Close() }()

	f := false
	tc := config.TieredConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		HeavyStickySec:     12,
		SpilloverThreshold: 4,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      &f,
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		Backends: []config.TieredBackend{
			{ID: "t1a", URL: t1aSrv.URL, Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 185000, PromoteAt: 150000, SpeedScore: 55},
			{ID: "t1b", URL: t1bSrv.URL, Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 185000, PromoteAt: 150000, SpeedScore: 55},
			{ID: "t2", URL: t2Srv.URL, Tier: 2, MaxContextTokens: 524288, MaxSafeInput: 495000, PromoteAt: 450000, DemoteBelow: 100000, SpeedScore: 55},
		},
	}

	tr, err := hub.Transport("p", "local", &tc, nil)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &tc}}})

	mkPrompt := func(sys, usr string) []byte {
		b, _ := json.Marshal(map[string]any{
			"model": "m1",
			"messages": []map[string]any{
				{"role": "system", "content": sys},
				{"role": "user", "content": usr},
			},
		})
		return b
	}

	// 1. Independent sessions spread across t1a and t1b via totalRouted balancing.
	rA1 := postChat(t, tr, mkPrompt("sys-A", "u1"))
	_, _ = io.Copy(io.Discard, rA1.Body)
	rA1.Body.Close()
	if got := rA1.Header.Get("X-Router-Backend"); got != "t1a" {
		t.Fatalf("Session A turn 1 expected t1a, got %q", got)
	}

	rB1 := postChat(t, tr, mkPrompt("sys-B", "u1"))
	_, _ = io.Copy(io.Discard, rB1.Body)
	rB1.Body.Close()
	if got := rB1.Header.Get("X-Router-Backend"); got != "t1b" {
		t.Fatalf("Session B turn 1 expected t1b (load-balanced), got %q", got)
	}

	// Session C goes to t1a (both t1a and t1b have totalRouted=1), giving t1a higher totalRouted (2 vs 1).
	rC1 := postChat(t, tr, mkPrompt("sys-C", "u1"))
	_, _ = io.Copy(io.Discard, rC1.Body)
	rC1.Body.Close()
	if got := rC1.Header.Get("X-Router-Backend"); got != "t1a" {
		t.Fatalf("Session C turn 1 expected t1a, got %q", got)
	}

	// 2. Session A turn 2 stays on LastBackend (t1a) when both t1a & t1b are idle,
	// even though t1a has more totalRouted than t1b (L1 VRAM affinity).
	rA2Body, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": "sys-A"},
			{"role": "user", "content": "u1"},
			{"role": "assistant", "content": "from-t1a"},
			{"role": "user", "content": "u2"},
		},
	})
	rA2 := postChat(t, tr, rA2Body)
	_, _ = io.Copy(io.Discard, rA2.Body)
	rA2.Body.Close()
	if got := rA2.Header.Get("X-Router-Backend"); got != "t1a" {
		t.Fatalf("Session A turn 2 expected L1 VRAM affinity to t1a, got %q", got)
	}

	// 3a. Busy shift: while t1a has an active request in-flight, traffic shifts to sibling t1b (not t2).
	blockT1A.Store(true)
	holdT1A.Add(1)
	go func() {
		defer holdT1A.Done()
		resp := postChat(t, tr, rA2Body)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	<-t1aEnteredCh

	rA3 := postChat(t, tr, rA2Body)
	_, _ = io.Copy(io.Discard, rA3.Body)
	rA3.Body.Close()
	if got := rA3.Header.Get("X-Router-Backend"); got != "t1b" {
		t.Fatalf("expected busy shift from t1a to sibling t1b, got %q", got)
	}
	blockT1A.Store(false)
	close(releaseT1A)
	holdT1A.Wait()
	if !t1aEntered.Load() || t2Hits.Load() != 0 {
		t.Fatalf("expected t1a blocker ran and t2 untouched so far, got t2Hits=%d", t2Hits.Load())
	}

	// 3b. 503 failover: give Session A affinity back to t1a via Session A on t1a (currently Session A's LastBackend is t1b from rA3,
	// so send Session C turn 2 whose LastBackend is t1a). Set t1a to 503 -> should fail over to sibling t1b before t2.
	failT1AStatus.Store(503)
	rC2Body, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": "sys-C"},
			{"role": "user", "content": "u1"},
			{"role": "assistant", "content": "from-t1a"},
			{"role": "user", "content": "u2"},
		},
	})
	t1bBefore503 := t1bHits.Load()
	rC2 := postChat(t, tr, rC2Body)
	_, _ = io.Copy(io.Discard, rC2.Body)
	rC2.Body.Close()
	if rC2.StatusCode != http.StatusOK || rC2.Header.Get("X-Router-Backend") != "t1b" {
		t.Fatalf("expected 503 failover from t1a to sibling t1b, got status=%d backend=%q", rC2.StatusCode, rC2.Header.Get("X-Router-Backend"))
	}
	if t1bHits.Load() != t1bBefore503+1 || t2Hits.Load() != 0 {
		t.Fatalf("expected failover to t1b without touching t2, got t1bDelta=%d t2Hits=%d", t1bHits.Load()-t1bBefore503, t2Hits.Load())
	}

	// 4. 400 context length error on t1a excludes BOTH t1a and t1b and retries directly on Tier 2 (t2).
	failT1AStatus.Store(0)
	var t1aSys string
	for i := 0; i < 8; i++ {
		sys := fmt.Sprintf("sys-ctx-%d", i)
		rx := postChat(t, tr, mkPrompt(sys, "u1"))
		_, _ = io.Copy(io.Discard, rx.Body)
		rx.Body.Close()
		if rx.Header.Get("X-Router-Backend") == "t1a" {
			t1aSys = sys
			break
		}
	}
	if t1aSys == "" {
		t.Fatal("failed to pin a session onto t1a before testing 400 failover")
	}
	failT1AStatus.Store(400)
	t1aBefore400 := t1aHits.Load()
	t1bBefore400 := t1bHits.Load()
	ctxFollowBody, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": t1aSys},
			{"role": "user", "content": "u1"},
			{"role": "assistant", "content": "from-t1a"},
			{"role": "user", "content": "trigger context overflow on t1a"},
		},
	})
	r400 := postChat(t, tr, ctxFollowBody)
	_, _ = io.Copy(io.Discard, r400.Body)
	r400.Body.Close()
	if r400.StatusCode != http.StatusOK || r400.Header.Get("X-Router-Backend") != "t2" {
		t.Fatalf("expected 400 context error on t1a to skip t1b and retry on t2, got status=%d backend=%q", r400.StatusCode, r400.Header.Get("X-Router-Backend"))
	}
	if t1aHits.Load() != t1aBefore400+1 || t1bHits.Load() != t1bBefore400 || t2Hits.Load() != 1 {
		t.Fatalf("expected t1a+1 t1b+0 t2=1 on 400 context failover, got t1aDelta=%d t1bDelta=%d t2=%d",
			t1aHits.Load()-t1aBefore400, t1bHits.Load()-t1bBefore400, t2Hits.Load())
	}
}

func TestSameTierSpilloverAndNoPermanentPromotion(t *testing.T) {
	var holdMode atomic.Bool
	releaseCh := make(chan struct{})
	var t1aInFlight, t1bInFlight, t2Hits, t3Hits atomic.Int32
	t1BusyReady := make(chan struct{}, 8)

	mkHoldable := func(id string, inflight *atomic.Int32) *httptest.Server {
		return makeMockTierBackend(id, func(w http.ResponseWriter, r *http.Request) bool {
			if holdMode.Load() {
				inflight.Add(1)
				t1BusyReady <- struct{}{}
				<-releaseCh
				inflight.Add(-1)
			}
			return false
		})
	}
	t1aSrv := mkHoldable("t1a", &t1aInFlight)
	defer t1aSrv.Close()
	t1bSrv := mkHoldable("t1b", &t1bInFlight)
	defer t1bSrv.Close()
	t2Srv := makeMockTierBackend("t2", func(w http.ResponseWriter, r *http.Request) bool {
		t2Hits.Add(1)
		return false
	})
	defer t2Srv.Close()
	t3Srv := makeMockTierBackend("t3", func(w http.ResponseWriter, r *http.Request) bool {
		t3Hits.Add(1)
		return false
	})
	defer t3Srv.Close()

	hub := NewHub(t.TempDir(), nil)
	defer func() { _ = hub.Close() }()

	f := false
	tc := config.TieredConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		HeavyStickySec:     12,
		SpilloverThreshold: 2,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      &f,
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		Backends: []config.TieredBackend{
			{ID: "t1a", URL: t1aSrv.URL, Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 185000, PromoteAt: 150000, SpeedScore: 55},
			{ID: "t1b", URL: t1bSrv.URL, Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 185000, PromoteAt: 150000, SpeedScore: 55},
			{ID: "t2", URL: t2Srv.URL, Tier: 2, MaxContextTokens: 524288, MaxSafeInput: 495000, PromoteAt: 450000, DemoteBelow: 20, SpeedScore: 55},
			{ID: "t3", URL: t3Srv.URL, Tier: 3, MaxContextTokens: 1048576, MaxSafeInput: 1048576, PromoteAt: 1048576, DemoteBelow: 350000, SpeedScore: 110},
		},
	}

	tr, err := hub.Transport("p", "local", &tc, nil)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &tc}}})

	// Saturate both t1a and t1b up to SpilloverThreshold=2 (4 concurrent Tier-1 requests).
	holdMode.Store(true)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{
				"model": "m1",
				"messages": []map[string]any{
					{"role": "system", "content": fmt.Sprintf("sat-sys-%d", idx)},
					{"role": "user", "content": "hold"},
				},
			})
			resp := postChat(t, tr, body)
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}(i)
		<-t1BusyReady
	}
	if t1aInFlight.Load() < 2 || t1bInFlight.Load() < 2 {
		t.Fatalf("expected both Tier 1 backends at >=2 activeReqs, got t1a=%d t1b=%d", t1aInFlight.Load(), t1bInFlight.Load())
	}

	// Send Session S while Tier 1 is saturated: must spill over to Tier 2 (protecting Tier 3).
	spillBody, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": "session-S-system-prompt-above-demote-floor"},
			{"role": "user", "content": "hello spillover request with enough tokens to exceed demote_below 20"},
		},
	})
	spillResp := postChat(t, tr, spillBody)
	_, _ = io.Copy(io.Discard, spillResp.Body)
	spillResp.Body.Close()
	if got, reason := spillResp.Header.Get("X-Router-Backend"), spillResp.Header.Get("X-Router-Reason"); got != "t2" || !strings.Contains(reason, "-spillover(t1_busy=") {
		t.Fatalf("expected spillover to t2 with -spillover(t1_busy=...) reason, got backend=%q reason=%q", got, reason)
	}
	if t3Hits.Load() != 0 {
		t.Fatalf("expected t3 protected during spillover, got t3Hits=%d", t3Hits.Load())
	}

	// Release Tier-1 saturation.
	holdMode.Store(false)
	close(releaseCh)
	wg.Wait()

	// Next turn of Session S has prompt >= t2.DemoteBelow (20), yet because spillover is temporary
	// (sess.Tier stayed 1), it must return naturally to Tier 1 rather than sticking on Tier 2!
	followBody, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": "session-S-system-prompt-above-demote-floor"},
			{"role": "user", "content": "hello spillover request with enough tokens to exceed demote_below 20"},
			{"role": "assistant", "content": "from-t2"},
			{"role": "user", "content": "follow up after spillover"},
		},
	})
	t2BeforeFollow := t2Hits.Load()
	followResp := postChat(t, tr, followBody)
	_, _ = io.Copy(io.Discard, followResp.Body)
	followResp.Body.Close()
	gotFollow := followResp.Header.Get("X-Router-Backend")
	if gotFollow != "t1a" && gotFollow != "t1b" {
		t.Fatalf("expected follow-up after spillover to return to Tier 1, got backend=%q reason=%q", gotFollow, followResp.Header.Get("X-Router-Reason"))
	}
	if t2Hits.Load() != t2BeforeFollow {
		t.Fatalf("expected t2 not hit after spillover subsided, delta=%d", t2Hits.Load()-t2BeforeFollow)
	}
}

func TestShadowMultiNodeTracking(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Router-Backend", "t2b")
		w.Header().Set("X-Router-Reason", "tier2-len(120)")
		_, _ = w.Write([]byte(`{"id":"c-up","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":2,"total_tokens":122,"prompt_tokens_details":{"cached_tokens":100}}}`))
	}))
	defer upstream.Close()

	mockHealth := makeMockTierBackend("health", nil)
	defer mockHealth.Close()

	hub := NewHub(t.TempDir(), nil)
	defer func() { _ = hub.Close() }()

	f := false
	tc := config.TieredConfig{
		Mode:               "shadow",
		HeavyMissThreshold: 500,
		SpilloverThreshold: 4,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      &f,
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		Backends: []config.TieredBackend{
			{ID: "t1a", URL: mockHealth.URL, Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 1000, PromoteAt: 80, SpeedScore: 55},
			{ID: "t1b", URL: mockHealth.URL, Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 1000, PromoteAt: 80, SpeedScore: 55},
			{ID: "t2a", URL: mockHealth.URL, Tier: 2, MaxContextTokens: 524288, MaxSafeInput: 5000, PromoteAt: 500, DemoteBelow: 40, SpeedScore: 55},
			{ID: "t2b", URL: mockHealth.URL, Tier: 2, MaxContextTokens: 524288, MaxSafeInput: 5000, PromoteAt: 500, DemoteBelow: 40, SpeedScore: 55},
			{ID: "t3", URL: mockHealth.URL, Tier: 3, MaxContextTokens: 1048576, MaxSafeInput: 10000, PromoteAt: 10000, DemoteBelow: 300, SpeedScore: 110},
		},
	}

	u, _ := url.Parse(upstream.URL)
	tr, err := hub.Transport("p", "local", &tc, &http.Transport{
		Proxy: nil,
	})
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", BaseURL: upstream.URL, Target: "local", Tiered: &tc}}})

	// Send >=80 estimated tokens (< 500 heavy miss threshold) so targetTier == 2.
	turn1Body, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": strings.Repeat("shadow routing system prompt prefix ", 12)},
			{"role": "user", "content": strings.Repeat("initial turn context ", 8)},
		},
	})
	req1, _ := http.NewRequest(http.MethodPost, u.String()+"/v1/chat/completions", bytes.NewReader(turn1Body))
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := tr.RoundTrip(req1)
	if err != nil {
		t.Fatalf("shadow turn1 RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()

	// Verify t2b recorded total_routed=1 and session recorded LastBackend="t2b".
	r, _ := hub.Router("p")
	st := r.Status()
	var t2bStatus *BackendStatus
	for i := range st.Backends {
		if st.Backends[i].ID == "t2b" {
			t2bStatus = &st.Backends[i]
		}
	}
	if t2bStatus == nil || t2bStatus.TotalRouted != 1 {
		t.Fatalf("expected t2b TotalRouted=1 in shadow mode, got %+v", t2bStatus)
	}
	if st.Shadow.Compared != 1 || st.Shadow.ByActual["t2b"].Total != 1 {
		t.Fatalf("unexpected shadow stats after turn1: %+v", st.Shadow)
	}

	// Turn 2 on same session: L1 VRAM affinity keeps shadow prediction on t2b (matching upstream).
	turn2Body, _ := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]any{
			{"role": "system", "content": strings.Repeat("shadow routing system prompt prefix ", 12)},
			{"role": "user", "content": strings.Repeat("initial turn context ", 8)},
			{"role": "assistant", "content": "ok"},
			{"role": "user", "content": "follow up"},
		},
	})
	req2, _ := http.NewRequest(http.MethodPost, u.String()+"/v1/chat/completions", bytes.NewReader(turn2Body))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := tr.RoundTrip(req2)
	if err != nil {
		t.Fatalf("shadow turn2 RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	if gotPred := resp2.Header.Get("X-Router-Shadow-Backend"); gotPred != "t2b" {
		t.Fatalf("expected shadow turn2 prediction to match t2b via L1 VRAM affinity, got %q (reason=%q)", gotPred, resp2.Header.Get("X-Router-Shadow-Reason"))
	}
	st2 := r.Status()
	if st2.Shadow.Compared != 2 || st2.Shadow.Matched < 1 || st2.Shadow.ByActual["t2b"].Matched < 1 {
		t.Fatalf("expected t2b shadow match recorded on turn2, got %+v", st2.Shadow)
	}
}
