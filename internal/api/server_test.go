package api

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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/meter"
)

func TestLiveRequestLogRetentionReload(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	cfgText := func(days int) string {
		return fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
request_log_days: %d
providers:
  - name: p
    base_url: %s/v1
    target: local
    models: [m1]
default_group: main
routing_groups:
  main:
    members: [p/m1]
`, dir, days, up.URL)
	}

	raw0 := cfgText(0)
	if err := os.WriteFile(cfgPath, []byte(raw0), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(raw0), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	insertOld := func(id string) {
		oldTime := time.Now().Add(-10 * 24 * time.Hour)
		if err := srv.Pipeline.Meter.LogRequest(meter.RequestLogEntry{
			RequestID: id,
			Time:      oldTime,
			Status:    200,
			Admitted:  true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	countRows := func() int {
		var n int
		if err := srv.DB.QueryRow(`SELECT COUNT(*) FROM request_log`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Start with retention=0 (disabled): old row stays intact.
	insertOld("req_old_1")
	if err := srv.PruneRequestLogNow(time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := countRows(); n != 1 {
		t.Fatalf("expected 1 row with retention=0, got %d", n)
	}

	// Hot reload 0 -> 7: background prune worker wakes up via pruneCh and deletes old row.
	if _, err := srv.Live.Apply([]byte(cfgText(7)), "test", srv.Live.Current().Cfg.Hash()); err != nil {
		t.Fatalf("apply retention=7: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countRows() == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if n := countRows(); n != 0 {
		t.Fatalf("expected old row to be pruned after 0->7 reload, got %d rows", n)
	}

	// Hot reload 7 -> 0: pruning must stop even when PruneRequestLogNow runs.
	if _, err := srv.Live.Apply([]byte(cfgText(0)), "test", srv.Live.Current().Cfg.Hash()); err != nil {
		t.Fatalf("apply retention=0: %v", err)
	}
	insertOld("req_old_2")
	if err := srv.PruneRequestLogNow(time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := countRows(); n != 1 {
		t.Fatalf("expected old row preserved after 7->0 reload, got %d rows", n)
	}
}

func TestHotReloadRequiredDetectors(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer up.Close()

	cfgWithDetectors := func(detectors string) string {
		return fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
providers:
  - name: cloud
    base_url: %s/v1
    target: external
    models: [m1]
default_group: main
routing_groups:
  main:
    members: [cloud/m1]
plugins:
  - name: ner
    kind: detector
    path: /nonexistent/ner
    enabled: false
redaction:
  mode_by_target: { local: audit, external: mask }
  detectors: %s
  fail_closed: true
`, dir, up.URL, detectors)
	}

	raw0 := cfgWithDetectors("[]")
	if err := os.WriteFile(cfgPath, []byte(raw0), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(raw0), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	tok, _, err := srv.LocalToken()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.HTTP.Handler)
	defer ts.Close()

	callChat := func() int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"cloud/m1","messages":[{"role":"user","content":"hello"}]}`)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	checkHealth := func() (string, []any) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		unavail, _ := m["detectors_unavailable"].([]any)
		return fmt.Sprint(m["status"]), unavail
	}

	// Initially optional disabled detector is not required: chat succeeds and /healthz is ok.
	if st := callChat(); st != http.StatusOK {
		t.Fatalf("expected 200 initially, got %d", st)
	}
	if hs, unavail := checkHealth(); hs != "ok" || len(unavail) != 0 {
		t.Fatalf("expected healthy initially, got status=%q unavailable=%v", hs, unavail)
	}

	// Hot reload adding disabled detector "ner" as required: must fail-closed (503) and mark degraded.
	if _, err := srv.Live.Apply([]byte(cfgWithDetectors("[ner]")), "test", srv.Live.Current().Cfg.Hash()); err != nil {
		t.Fatalf("apply detectors=[ner]: %v", err)
	}
	if st := callChat(); st != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 fail-closed when required detector missing, got %d", st)
	}
	if hs, unavail := checkHealth(); hs != "degraded" || len(unavail) != 1 || unavail[0] != "ner" {
		t.Fatalf("expected degraded [ner], got status=%q unavailable=%v", hs, unavail)
	}

	// Hot reload removing "ner" recovers cleanly.
	if _, err := srv.Live.Apply([]byte(cfgWithDetectors("[]")), "test", srv.Live.Current().Cfg.Hash()); err != nil {
		t.Fatalf("apply detectors=[]: %v", err)
	}
	if st := callChat(); st != http.StatusOK {
		t.Fatalf("expected 200 after removing required detector, got %d", st)
	}
	if hs, unavail := checkHealth(); hs != "ok" || len(unavail) != 0 {
		t.Fatalf("expected ok after removing required detector, got status=%q unavailable=%v", hs, unavail)
	}
}

type gatedBody struct {
	data    []byte
	offset  int
	started chan struct{}
	once    sync.Once
	release <-chan struct{}
}

func (g *gatedBody) Read(p []byte) (int, error) {
	g.once.Do(func() {
		close(g.started)
		<-g.release
	})
	if g.offset >= len(g.data) {
		return 0, io.EOF
	}
	n := copy(p, g.data[g.offset:])
	g.offset += n
	return n, nil
}

func (g *gatedBody) Close() error { return nil }

func TestInFlightTieredSnapshotIsolation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	var localHits, extHits atomic.Int32
	var extLastBody atomic.Value
	mkBackend := func(counter *atomic.Int32, saveBody *atomic.Value) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/get_server_info" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			b, _ := io.ReadAll(r.Body)
			if saveBody != nil {
				saveBody.Store(string(b))
			}
			counter.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
		}))
	}
	localSrv := mkBackend(&localHits, nil)
	defer localSrv.Close()
	extSrv := mkBackend(&extHits, &extLastBody)
	defer extSrv.Close()

	cfgText := func(target, backendURL string) string {
		return fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
providers:
  - name: tierp
    target: %s
    models: [m1]
    tiered:
      mode: active
      heavy_miss_threshold: 24000
      health_interval_sec: 60
      zh_think_anchor: false
      starter_cache:
        enabled: false
      backends:
        - id: b1
          url: %s
          tier: 1
          max_context_tokens: 64000
          max_safe_input: 50000
          promote_at: 64000
          speed_score: 100
default_group: main
routing_groups:
  main:
    members: [tierp/m1]
redaction:
  mode_by_target: { local: off, external: mask }
`, dir, target, backendURL)
	}

	raw0 := cfgText("local", localSrv.URL)
	if err := os.WriteFile(cfgPath, []byte(raw0), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(raw0), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	tok, _, err := srv.LocalToken()
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	inFlightReq, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", &gatedBody{
		data:    []byte(`{"model":"tierp/m1","messages":[{"role":"user","content":"call me 13800138000"}]}`),
		started: started,
		release: release,
	})
	inFlightReq.Header.Set("Authorization", "Bearer "+tok)
	inFlightReq.Header.Set("Content-Type", "application/json")
	inFlightRec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.HTTP.Handler.ServeHTTP(inFlightRec, inFlightReq)
		close(done)
	}()

	// Wait until Pipeline.Serve has bound Snapshot 1 and is reading the request body.
	<-started

	// Hot reload to Snapshot 2: external target & new backend URL on the same backend ID ("b1").
	if _, err := srv.Live.Apply([]byte(cfgText("external", extSrv.URL)), "test", srv.Live.Current().Cfg.Hash()); err != nil {
		t.Fatalf("apply snapshot 2: %v", err)
	}

	// Unblock Snapshot 1's in-flight request: it must still dispatch to localSrv, never extSrv.
	close(release)
	<-done

	if inFlightRec.Code != http.StatusOK {
		t.Fatalf("expected in-flight status 200, got %d (%s)", inFlightRec.Code, inFlightRec.Body.String())
	}
	if localHits.Load() != 1 || extHits.Load() != 0 {
		t.Fatalf("expected in-flight Snapshot 1 request to hit localSrv only, got local=%d ext=%d", localHits.Load(), extHits.Load())
	}

	// New request under Snapshot 2 hits extSrv with external masking applied.
	nextReq, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader([]byte(`{"model":"tierp/m1","messages":[{"role":"user","content":"call me 13800138000"}]}`)))
	nextReq.Header.Set("Authorization", "Bearer "+tok)
	nextReq.Header.Set("Content-Type", "application/json")
	nextRec := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(nextRec, nextReq)
	if nextRec.Code != http.StatusOK {
		t.Fatalf("expected post-reload status 200, got %d (%s)", nextRec.Code, nextRec.Body.String())
	}
	if localHits.Load() != 1 || extHits.Load() != 1 {
		t.Fatalf("expected post-reload request to hit extSrv, got local=%d ext=%d", localHits.Load(), extHits.Load())
	}
	seenBody, _ := extLastBody.Load().(string)
	if !strings.Contains(seenBody, "{{PHONE_") {
		t.Fatalf("expected post-reload external request body to be masked, got %s", seenBody)
	}
}

func TestHotReloadVaultTTL(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	phoneRe := regexp.MustCompile(`\{\{PHONE_[a-z2-7]{8}\}\}`)
	var capturedPlaceholder atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ph := phoneRe.FindString(string(b))
		if ph != "" {
			capturedPlaceholder.Store(ph)
		} else if prev, ok := capturedPlaceholder.Load().(string); ok {
			ph = prev
		}
		reply := "echo " + ph
		respJSON, _ := json.Marshal(map[string]any{
			"id":     "c1",
			"object": "chat.completion",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": reply},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(respJSON)
	}))
	defer up.Close()

	cfgWithTTL := func(ttl string) string {
		return fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
providers:
  - name: cloud
    base_url: %s/v1
    target: external
    models: [m1]
default_group: main
routing_groups:
  main:
    members: [cloud/m1]
redaction:
  mode_by_target: { local: audit, external: mask }
  vault_ttl: %s
`, dir, up.URL, ttl)
	}

	raw0 := cfgWithTTL("24h")
	if err := os.WriteFile(cfgPath, []byte(raw0), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(raw0), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	tok, _, err := srv.LocalToken()
	if err != nil {
		t.Fatal(err)
	}

	call := func(msg string) string {
		t.Helper()
		payload := fmt.Sprintf(`{"model":"cloud/m1","messages":[{"role":"user","content":%q}]}`, msg)
		req, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader([]byte(payload)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Gatehouse-Session", "s1")
		rec := httptest.NewRecorder()
		srv.HTTP.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Choices[0].Message.Content
	}

	// 1. Store secret placeholder under vault_ttl=24h and verify restore.
	if got := call("my phone is 13800138000"); got != "echo 13800138000" {
		t.Fatalf("expected restored phone initially, got %q", got)
	}
	ph, _ := capturedPlaceholder.Load().(string)
	if ph == "" {
		t.Fatal("expected upstream to observe placeholder")
	}

	// 2. Preview with a tiny TTL must NOT evict or shorten the live Vault.
	if _, err := srv.Live.Preview([]byte(cfgWithTTL("1ms"))); err != nil {
		t.Fatalf("preview 1ms: %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	if got := call("echo previous placeholder"); got != "echo 13800138000" {
		t.Fatalf("expected Preview not to affect live vault TTL, got %q", got)
	}

	// 3. Hot reload vault_ttl to 30ms after letting the bucket sit idle > 30ms.
	time.Sleep(50 * time.Millisecond)
	if _, err := srv.Live.Apply([]byte(cfgWithTTL("30ms")), "test", srv.Live.Current().Cfg.Hash()); err != nil {
		t.Fatalf("apply 30ms: %v", err)
	}
	if got := call("echo previous placeholder"); got != "echo "+ph {
		t.Fatalf("expected expired bucket not to restore after vault_ttl reload, got %q (wanted %q)", got, "echo "+ph)
	}
}

func TestStarterWarmupEnforcesConfidentialityAndRedaction(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	var lastWarmBody atomic.Value
	var reqCount atomic.Int32
	srvUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		reqCount.Add(1)
		if strings.Contains(r.Header.Get("User-Agent"), "warmer") {
			lastWarmBody.Store(string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":150,"completion_tokens":2,"total_tokens":152}}`))
	}))
	defer srvUp.Close()

	cfgText := func(target, terms, termAction string) string {
		catLine := ""
		if termAction != "" {
			catLine = fmt.Sprintf("  categories: { term: %s }\n", termAction)
		}
		return fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
providers:
  - name: tierp
    target: %s
    models: [m1]
    tiered:
      mode: active
      heavy_miss_threshold: 24000
      health_interval_sec: 60
      zh_think_anchor: false
      starter_cache:
        enabled: true
        min_tokens: 32
        max_tokens: 100000
        min_hits: 1
        check_interval_sec: 600
        startup_delay_sec: 600
      backends:
        - id: b1
          url: %s
          tier: 1
          max_context_tokens: 64000
          max_safe_input: 50000
          promote_at: 64000
          speed_score: 100
default_group: main
routing_groups:
  main:
    members: [tierp/m1]
redaction:
  mode_by_target: { local: audit, external: mask }
  terms: %s
%s`, dir, target, srvUp.URL, terms, catLine)
	}

	raw0 := cfgText("local", "[]", "")
	if err := os.WriteFile(cfgPath, []byte(raw0), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(raw0), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	tok, _, err := srv.LocalToken()
	if err != nil {
		t.Fatal(err)
	}

	sysPrompt := strings.Repeat("ProjectZeusSecret system context instruction block. ", 12)
	sendChat := func(dataClass string) int {
		payload, _ := json.Marshal(map[string]any{
			"model": "tierp/m1",
			"messages": []map[string]any{
				{"role": "system", "content": sysPrompt},
				{"role": "user", "content": "hello"},
			},
		})
		req, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		if dataClass != "" {
			req.Header.Set("X-Gatehouse-Data-Class", dataClass)
		}
		rec := httptest.NewRecorder()
		srv.HTTP.Handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// 1. Confidential request on local provider must not be captured into StarterCache.
	if code := sendChat("confidential"); code != http.StatusOK {
		t.Fatalf("expected confidential local chat 200, got %d", code)
	}
	rLocal, ok := srv.Tiered.Router("tierp")
	if !ok || len(rLocal.Starters()) != 0 {
		t.Fatalf("expected confidential request excluded from starters, got ok=%v count=%d", ok, len(rLocal.Starters()))
	}

	// 2. Switch to external provider, observe an internal prompt before ProjectZeusSecret is in dictionary.
	if _, err := srv.Live.Apply([]byte(cfgText("external", "[]", "")), "test", srv.Live.Current().Cfg.Hash()); err != nil {
		t.Fatalf("apply external: %v", err)
	}
	if code := sendChat("internal"); code != http.StatusOK {
		t.Fatalf("expected internal external chat 200, got %d", code)
	}
	rExt, ok := srv.Tiered.Router("tierp")
	if !ok || len(rExt.Starters()) != 1 {
		t.Fatalf("expected 1 external starter captured, got ok=%v count=%d", ok, len(rExt.Starters()))
	}
	hash := rExt.Starters()[0].Hash

	// 3. Hot reload adding ProjectZeusSecret as masked term: warm-up replay must redact it.
	if _, err := srv.Live.Apply([]byte(cfgText("external", "[ProjectZeusSecret]", "mask")), "test", srv.Live.Current().Cfg.Hash()); err != nil {
		t.Fatalf("apply masked term: %v", err)
	}
	warmRes, err := rExt.WarmStarter(hash, true)
	if err != nil || len(warmRes) != 1 || warmRes[0].Error != "" {
		t.Fatalf("expected masked warm-up success, got err=%v res=%+v", err, warmRes)
	}
	warmBody, _ := lastWarmBody.Load().(string)
	if warmBody == "" || strings.Contains(warmBody, "ProjectZeusSecret") || !strings.Contains(warmBody, "{{TERM_") {
		t.Fatalf("expected starter warm-up payload to mask ProjectZeusSecret, got %s", warmBody)
	}

	// 4. Hot reload making term action=block: warm-up replay must be blocked before any upstream request.
	beforeReqs := reqCount.Load()
	if _, err := srv.Live.Apply([]byte(cfgText("external", "[ProjectZeusSecret]", "block")), "test", srv.Live.Current().Cfg.Hash()); err != nil {
		t.Fatalf("apply block term: %v", err)
	}
	warmRes, err = rExt.WarmStarter(hash, true)
	if err != nil || len(warmRes) != 1 || warmRes[0].Error == "" {
		t.Fatalf("expected blocked warm-up error result, got err=%v res=%+v", err, warmRes)
	}
	if reqCount.Load() != beforeReqs {
		t.Fatalf("expected 0 upstream requests when warm-up blocked by redaction, got %d", reqCount.Load()-beforeReqs)
	}
}

func TestAnthropicCountTokensIncludesTools(t *testing.T) {
	dir := t.TempDir()
	raw := fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
providers:
  - name: p
    base_url: http://127.0.0.1:18080/v1
    models: [m1]
`, dir)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(raw), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	tok, _, err := srv.LocalToken()
	if err != nil {
		t.Fatal(err)
	}

	countFor := func(payload map[string]any) int {
		t.Helper()
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/messages/count_tokens", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", tok)
		rec := httptest.NewRecorder()
		srv.HTTP.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("count_tokens status=%d body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.InputTokens
	}

	plainTokens := countFor(map[string]any{
		"model":    "claude-3-7-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	longDesc := strings.Repeat("Detailed tool schema description for context budgeting. ", 80) // ~4.4 KB
	toolTokens := countFor(map[string]any{
		"model":    "claude-3-7-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools": []map[string]any{{
			"name":        "search_records",
			"description": longDesc,
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "search string"},
				},
				"required": []string{"query"},
			},
		}},
	})

	if toolTokens-plainTokens < 900 {
		t.Fatalf("expected tool schema (~4.4KB) to add >=900 tokens, got plain=%d withTools=%d (diff=%d)",
			plainTokens, toolTokens, toolTokens-plainTokens)
	}
}

func TestContextHeadersPassThroughAndKeepaliveTTFT(t *testing.T) {
	dir := t.TempDir()
	var sawNoComp, sawCompCtrl atomic.Value
	var sendKeepaliveThenChunk atomic.Bool
	sendKeepaliveThenChunk.Store(true)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		sawNoComp.Store(r.Header.Get("X-No-Compress"))
		sawCompCtrl.Store(r.Header.Get("X-Context-Compress"))
		w.Header().Set("X-Router-Backend", "b1")
		w.Header().Set("X-Router-Reason", "tier1-len(50)")
		w.Header().Set("X-Context-Compressed", "stage1")
		w.Header().Set("X-Context-Original-Tokens", "620000")
		w.Header().Set("X-Context-Compressed-Tokens", "180000")
		w.Header().Set("X-Context-Saved-Tokens", "440000")
		w.Header().Set("X-Context-Checkpoint-Id", "cp123456")
		w.Header().Set("X-Context-Stage2-Status", "ready")

		if sendKeepaliveThenChunk.Load() {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			_, _ = w.Write([]byte(": keepalive\n\n"))
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(60 * time.Millisecond)
			_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"pong\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":1,\"total_tokens\":21}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}

		// Slow failure after Gatehouse Pipeline keepalive headers
		time.Sleep(55 * time.Millisecond)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`bad gateway after delay`))
	}))
	defer up.Close()

	cfgRaw := fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
providers:
  - name: p
    base_url: %s/v1
    target: local
    models: [m1]
`, dir, up.URL)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(cfgRaw), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	tok, _, err := srv.LocalToken()
	if err != nil {
		t.Fatal(err)
	}

	// 1. SSE chat completions with : keepalive comment before first data chunk.
	req, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader([]byte(`{"model":"p/m1","stream":true,"messages":[{"role":"user","content":"ping"}]}`)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-No-Compress", "1")
	req.Header.Set("X-Context-Compress", "off")
	rec := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat stream status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Context-Compressed") != "stage1" || rec.Header().Get("X-Context-Saved-Tokens") != "440000" {
		t.Fatalf("expected X-Context-* headers passed through, got %v", rec.Header())
	}
	if gotNo, _ := sawNoComp.Load().(string); gotNo != "1" {
		t.Fatalf("expected X-No-Compress=1 forwarded upstream, got %q", gotNo)
	}
	if gotCtrl, _ := sawCompCtrl.Load().(string); gotCtrl != "off" {
		t.Fatalf("expected X-Context-Compress=off forwarded upstream, got %q", gotCtrl)
	}
	if !strings.Contains(rec.Body.String(), ": keepalive\n\n") || !strings.Contains(rec.Body.String(), `"content":"pong"`) {
		t.Fatalf("expected keepalive + pong chunk in stream body, got %q", rec.Body.String())
	}
	var gotStatus int
	var gotTTFT int64
	if err := srv.DB.QueryRow(`SELECT status, ttft_ms FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&gotStatus, &gotTTFT); err != nil {
		t.Fatalf("query request_log failed: %v", err)
	}
	if gotStatus != http.StatusOK || gotTTFT < 45 {
		t.Fatalf("expected status=200 and TTFTMS >= 45ms (must ignore leading : keepalive), got status=%d ttft=%dms", gotStatus, gotTTFT)
	}

	// 2. Anthropic /v1/messages SSE compat preserves : keepalive comments too.
	antReq, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/messages", bytes.NewReader([]byte(`{"model":"p/m1","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"ping"}]}`)))
	antReq.Header.Set("x-api-key", tok)
	antReq.Header.Set("Content-Type", "application/json")
	antRec := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(antRec, antReq)
	if antRec.Code != http.StatusOK {
		t.Fatalf("anthropic stream status=%d body=%s", antRec.Code, antRec.Body.String())
	}
	if !strings.Contains(antRec.Body.String(), ": keepalive\n\n") || !strings.Contains(antRec.Body.String(), `"pong"`) {
		t.Fatalf("expected keepalive + translated content_block_delta, got %q", antRec.Body.String())
	}

	// 3. Pipeline keepalive safety net fires on delayed failure and logs status 502.
	sendKeepaliveThenChunk.Store(false)
	srv.Pipeline.SSEKeepaliveInitDelay = 20 * time.Millisecond
	srv.Pipeline.SSEKeepaliveInterval = 15 * time.Millisecond
	slowReq, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader([]byte(`{"model":"p/m1","stream":true,"messages":[{"role":"user","content":"slow err"}]}`)))
	slowReq.Header.Set("Authorization", "Bearer "+tok)
	slowReq.Header.Set("Content-Type", "application/json")
	slowRec := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(slowRec, slowReq)
	if !strings.Contains(slowRec.Body.String(), ": keepalive\n\n") || !strings.Contains(slowRec.Body.String(), `"upstream_error"`) {
		t.Fatalf("expected Pipeline keepalive + upstream_error SSE event, got %q", slowRec.Body.String())
	}
	var slowStatus int
	if err := srv.DB.QueryRow(`SELECT status FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&slowStatus); err != nil {
		t.Fatalf("query request_log 2 failed: %v", err)
	}
	if slowStatus != http.StatusBadGateway {
		t.Fatalf("expected latest request_log status 502 after post-keepalive failure, got %d", slowStatus)
	}
}

type closeSpyBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *closeSpyBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestKeepaliveCodexFindingsRegression(t *testing.T) {
	dir := t.TempDir()
	var mode atomic.Int32 // 0 = sse-error-chunk, 1 = post-keepalive-500, 2 = slow-500-for-disabled

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		switch mode.Load() {
		case 0:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"SECRET_PROMPT_ECHO\"}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case 1:
			time.Sleep(45 * time.Millisecond)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("SECRET_PROMPT_ECHO"))
		case 2:
			time.Sleep(45 * time.Millisecond)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("SECRET_PROMPT_ECHO"))
		}
	}))
	defer up.Close()

	cfgRaw := fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
providers:
  - name: p
    base_url: %s/v1
    target: local
    models: [m1]
  - name: pdisabled
    target: local
    models: [m2]
    tiered:
      mode: active
      sse_keepalive_sec: -1
      health_interval_sec: 60
      starter_cache:
        enabled: false
      backends:
        - id: b1
          url: %s
          tier: 1
          max_context_tokens: 32000
          max_safe_input: 16000
          promote_at: 16000
          speed_score: 100
default_group: main
routing_groups:
  main:
    type: order
    members: [p/m1]
`, dir, up.URL, up.URL)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(cfgRaw), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	tok, _, err := srv.LocalToken()
	if err != nil {
		t.Fatal(err)
	}

	srv.Pipeline.SSEKeepaliveInitDelay = 15 * time.Millisecond
	srv.Pipeline.SSEKeepaliveInterval = 15 * time.Millisecond

	// 1. SSE error event containing sensitive text must forward SSE to client,
	//    log status=502, and NEVER record SECRET_PROMPT_ECHO into audit logs.
	mode.Store(0)
	req1, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader([]byte(`{"model":"p/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	req1.Header.Set("Authorization", "Bearer "+tok)
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec1, req1)
	if !strings.Contains(rec1.Body.String(), "SECRET_PROMPT_ECHO") {
		t.Fatalf("expected client to still receive upstream SSE error event, got %q", rec1.Body.String())
	}

	// 2. Post-keepalive HTTP 500 error body on regular provider must not leak to audit.
	mode.Store(1)
	req2, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader([]byte(`{"model":"p/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec2, req2)
	if !strings.Contains(rec2.Body.String(), ": keepalive\n\n") || !strings.Contains(rec2.Body.String(), `"upstream_error"`) {
		t.Fatalf("expected keepalive + upstream_error, got %q", rec2.Body.String())
	}

	// 3. Tiered provider with sse_keepalive_sec=-1 disables Pipeline keepalive wrapper too.
	mode.Store(2)
	req3, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader([]byte(`{"model":"pdisabled/m2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	req3.Header.Set("Authorization", "Bearer "+tok)
	req3.Header.Set("Content-Type", "application/json")
	rec3 := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusBadGateway || strings.Contains(rec3.Body.String(), ": keepalive") {
		t.Fatalf("expected 502 JSON with no keepalive when sse_keepalive_sec=-1, got code=%d body=%q", rec3.Code, rec3.Body.String())
	}

	// Verify audit files exist on disk, record upstream_error, and NEVER contain SECRET_PROMPT_ECHO.
	var auditBytes strings.Builder
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr == nil && !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			b, err := os.ReadFile(path)
			if err == nil {
				auditBytes.Write(b)
			}
		}
		return nil
	})
	auditStr := auditBytes.String()
	if !strings.Contains(auditStr, "upstream_error") {
		t.Fatalf("expected audit log to contain upstream_error records, got %q", auditStr)
	}
	if strings.Contains(auditStr, "SECRET_PROMPT_ECHO") {
		t.Fatalf("audit log leaked upstream error body: %s", auditStr)
	}

	// 4. Cancel racing with prov.Chat return must close late response body (tested under -race).
	srv.Pipeline.SSEKeepaliveInitDelay = 200 * time.Millisecond
	var bodiesMu sync.Mutex
	var bodies []*closeSpyBody
	srv.Live.Current().Reg.SetPlugin("p", func(ctx context.Context, body []byte) (*http.Response, error) {
		time.Sleep(10 * time.Millisecond)
		sb := &closeSpyBody{Reader: strings.NewReader(`{"id":"c1"}`)}
		bodiesMu.Lock()
		bodies = append(bodies, sb)
		bodiesMu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       sb,
		}, nil
	})
	for i := 0; i < 15; i++ {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		r, _ := http.NewRequestWithContext(cctx, http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader([]byte(`{"model":"p/m1","stream":true,"messages":[{"role":"user","content":"race"}]}`)))
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.HTTP.Handler.ServeHTTP(rec, r)
		cancel()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		allClosed := true
		bodiesMu.Lock()
		for _, b := range bodies {
			if !b.closed.Load() {
				allClosed = false
				break
			}
		}
		bodiesMu.Unlock()
		if allClosed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("canceled late response body was not closed")
}

func TestTieredKeepaliveFailoverRequestLogBackend(t *testing.T) {
	dir := t.TempDir()
	tb1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		time.Sleep(45 * time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`unavailable`))
	}))
	defer tb1.Close()

	tb2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"from-tb2\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer tb2.Close()

	cfgRaw := fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
providers:
  - name: ptiered
    target: local
    models: [m1]
    tiered:
      mode: active
      sse_keepalive_sec: 1
      health_interval_sec: 1
      starter_cache:
        enabled: false
      backends:
        - id: tb1
          url: %s
          tier: 1
          max_context_tokens: 32000
          max_safe_input: 16000
          promote_at: 16000
          speed_score: 100
        - id: tb2
          url: %s
          tier: 2
          max_context_tokens: 64000
          max_safe_input: 32000
          promote_at: 32000
          speed_score: 50
default_group: main
routing_groups:
  main:
    type: order
    members: [ptiered/m1]
`, dir, tb1.URL, tb2.URL)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(cfgRaw), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	rt, ok := srv.Tiered.Router("ptiered")
	if !ok {
		t.Fatal("missing tiered router ptiered")
	}
	rt.SetSSEKeepaliveInitDelay(15 * time.Millisecond)

	// Wait for health probe to mark both backends healthy so tb1 gets tried first cleanly.
	deadline := time.Now().Add(4 * time.Second)
	bothHealthy := false
	for time.Now().Before(deadline) {
		st := rt.Status()
		if len(st.Backends) == 2 && st.Backends[0].Healthy && st.Backends[1].Healthy {
			bothHealthy = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !bothHealthy {
		t.Fatalf("backends did not become healthy in time: %+v", rt.Status().Backends)
	}

	tok, _, err := srv.LocalToken()
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader([]byte(`{"model":"ptiered/m1","stream":true,"messages":[{"role":"user","content":"failover"}]}`)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"from-tb2"`) || !strings.Contains(rec.Body.String(), ": keepalive\n\n") {
		t.Fatalf("expected keepalive + failover chunk from tb2, got code=%d body=%q", rec.Code, rec.Body.String())
	}

	var routeBackend string
	if err := srv.DB.QueryRow(`SELECT route_backend FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&routeBackend); err != nil {
		t.Fatalf("query request_log failed: %v", err)
	}
	if routeBackend != "tb2" {
		t.Fatalf("expected request_log route_backend to record winning backend tb2, got %q", routeBackend)
	}
}

func TestPipelineKeepalivePreservesCandidateFailover(t *testing.T) {
	dir := t.TempDir()
	var p1Fail, p2Fail atomic.Bool
	p1Fail.Store(true)

	p1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if p1Fail.Load() {
			time.Sleep(40 * time.Millisecond)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`p1 overloaded`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"from-p1\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer p1.Close()

	p2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if p2Fail.Load() {
			time.Sleep(40 * time.Millisecond)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`p2 overloaded`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"2\",\"object\":\"chat.completion.chunk\",\"model\":\"m2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"from-p2\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"2\",\"object\":\"chat.completion.chunk\",\"model\":\"m2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer p2.Close()

	cfgRaw := fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
providers:
  - name: p1
    base_url: %s/v1
    target: local
    models: [m1]
  - name: p2
    base_url: %s/v1
    target: local
    models: [m2]
default_group: main
routing_groups:
  main:
    type: order
    members: [p1/m1, p2/m2]
`, dir, p1.URL, p2.URL)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(cfgRaw), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	srv.Pipeline.SSEKeepaliveInitDelay = 15 * time.Millisecond
	srv.Pipeline.SSEKeepaliveInterval = 15 * time.Millisecond

	tok, _, err := srv.LocalToken()
	if err != nil {
		t.Fatal(err)
	}

	// 1. Candidate 1 fails with 503 after keepalive delay -> fails over to Candidate 2 and marks Candidate 1 failed.
	req1, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader([]byte(`{"model":"main","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	req1.Header.Set("Authorization", "Bearer "+tok)
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK || !strings.Contains(rec1.Body.String(), ": keepalive\n\n") || !strings.Contains(rec1.Body.String(), `"content":"from-p2"`) {
		t.Fatalf("expected keepalive + failover stream from p2, got code=%d body=%q", rec1.Code, rec1.Body.String())
	}
	if srv.Live.Current().Reg.Healthy("p1/m1") {
		t.Fatalf("expected failed candidate p1/m1 to be marked unhealthy for cooldown")
	}
	var gotStatus int
	var gotModel string
	if err := srv.DB.QueryRow(`SELECT status, model FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&gotStatus, &gotModel); err != nil {
		t.Fatalf("query request_log 1 failed: %v", err)
	}
	if gotStatus != http.StatusOK || gotModel != "p2/m2" {
		t.Fatalf("expected request_log status=200 model=p2/m2, got status=%d model=%q", gotStatus, gotModel)
	}

	// 2. When all candidates fail after keepalives start -> emits terminal SSE error + [DONE] and logs status 502.
	p2Fail.Store(true)
	req2, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader([]byte(`{"model":"main","stream":true,"messages":[{"role":"user","content":"hi2"}]}`)))
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec2, req2)
	body2 := rec2.Body.String()
	if !strings.Contains(body2, ": keepalive\n\n") || !strings.Contains(body2, `"upstream_error"`) || !strings.Contains(body2, "data: [DONE]\n\n") {
		t.Fatalf("expected keepalive + terminal SSE error + [DONE], got %q", body2)
	}
	if err := srv.DB.QueryRow(`SELECT status FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&gotStatus); err != nil {
		t.Fatalf("query request_log 2 failed: %v", err)
	}
	if gotStatus != http.StatusBadGateway {
		t.Fatalf("expected request_log status=502 when all candidates fail, got %d", gotStatus)
	}
}

type failingReadCloser struct {
	err error
}

func (f failingReadCloser) Read([]byte) (int, error) { return 0, f.err }
func (f failingReadCloser) Close() error             { return nil }

func newMidStreamTestServer(t *testing.T, upstreamURL string, maxBodyMB int) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfgRaw := fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
max_body_mb: %d
providers:
  - name: p
    base_url: %s/v1
    target: local
    models: [m1]
default_group: main
routing_groups:
  main:
    type: order
    members: [p/m1]
`, dir, maxBodyMB, upstreamURL)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(cfgRaw), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	tok, _, err := srv.LocalToken()
	if err != nil {
		t.Fatal(err)
	}
	return srv, tok
}

func TestMidStreamKeepaliveAllProtocols(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello \"}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(75 * time.Millisecond)
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer up.Close()

	srv, tok := newMidStreamTestServer(t, up.URL, 0)
	srv.Pipeline.SSEKeepaliveInitDelay = 500 * time.Millisecond
	srv.Pipeline.SSEKeepaliveInterval = 20 * time.Millisecond

	cases := []struct {
		name     string
		path     string
		body     string
		wantText string
	}{
		{
			name:     "chat",
			path:     "/v1/chat/completions",
			body:     `{"model":"p/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			wantText: `"content":"world"`,
		},
		{
			name:     "messages",
			path:     "/v1/messages",
			body:     `{"model":"p/m1","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			wantText: `event: message_stop`,
		},
		{
			name:     "responses",
			path:     "/v1/responses",
			body:     `{"model":"p/m1","stream":true,"input":"hi"}`,
			wantText: `event: response.completed`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local"+tc.path, bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Authorization", "Bearer "+tok)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.HTTP.Handler.ServeHTTP(rec, req)
			got := rec.Body.String()
			if rec.Code != http.StatusOK {
				t.Fatalf("%s status=%d body=%q", tc.name, rec.Code, got)
			}
			if strings.Count(got, ": keepalive\n\n") < 2 {
				t.Fatalf("%s expected >=2 mid-stream keepalives, got %q", tc.name, got)
			}
			if !strings.Contains(got, tc.wantText) {
				t.Fatalf("%s missing %q in %q", tc.name, tc.wantText, got)
			}
			var logRow meter.RequestLogEntry
			if err := srv.DB.QueryRow(`SELECT status, ttft_ms FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&logRow.Status, &logRow.TTFTMS); err != nil {
				t.Fatalf("query request_log: %v", err)
			}
			if logRow.Status != http.StatusOK || logRow.TTFTMS >= 65 {
				t.Fatalf("%s unexpected log status=%d ttft=%dms", tc.name, logRow.Status, logRow.TTFTMS)
			}
		})
	}
}

func TestResponsesCustomToolBufferingEmitsKeepalive(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Initial tool call announcement triggers ResponsesTranslator.announceCall (bytes written > 0).
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"fc_1\",\"type\":\"function\",\"function\":{\"name\":\"apply_patch\",\"arguments\":\"{\\\"input\\\":\\\"\"}}]}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		// Upstream trickles chunks every 10ms (< 25ms keepalive interval), while ResponsesTranslator
		// buffers custom tool arguments in memory without writing any client bytes.
		for i := 0; i < 8; i++ {
			time.Sleep(10 * time.Millisecond)
			_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"line\\n\"}}]}}]}\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":30,\"total_tokens\":50}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer up.Close()

	srv, tok := newMidStreamTestServer(t, up.URL, 0)
	srv.Pipeline.SSEKeepaliveInitDelay = 500 * time.Millisecond
	srv.Pipeline.SSEKeepaliveInterval = 25 * time.Millisecond

	body := `{"model":"p/m1","stream":true,"input":"edit","tools":[{"type":"custom","name":"apply_patch","description":"patch files"}]}`
	req, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/responses", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec, req)
	got := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, got)
	}
	if n := strings.Count(got, ": keepalive\n\n"); n < 2 {
		t.Fatalf("expected >=2 keepalives while buffering custom tool arguments, got %d in %q", n, got)
	}
	if !strings.Contains(got, `"type":"custom_tool_call"`) || !strings.Contains(got, `event: response.completed`) {
		t.Fatalf("expected complete custom_tool_call output, got %q", got)
	}
}

func TestReadRequestBodyErrorClassification(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	srv, tok := newMidStreamTestServer(t, up.URL, 1)

	// 1. Exceeds max_body_mb (1 MiB) -> 413 body_too_large.
	huge := append([]byte(`{"model":"p/m1","messages":[{"role":"user","content":"`), bytes.Repeat([]byte("a"), (1<<20)+1024)...)
	huge = append(huge, []byte(`"}]}`)...)
	req1, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", bytes.NewReader(huge))
	req1.Header.Set("Authorization", "Bearer "+tok)
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec1.Body.String(), `"body_too_large"`) {
		t.Fatalf("expected 413 body_too_large, got code=%d body=%q", rec1.Code, rec1.Body.String())
	}

	// 2. Truncated upload (unexpected EOF) -> 408 request_body_read_error (NOT 413).
	req2, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/chat/completions", failingReadCloser{err: io.ErrUnexpectedEOF})
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusRequestTimeout || !strings.Contains(rec2.Body.String(), `"request_body_read_error"`) {
		t.Fatalf("expected 408 request_body_read_error, got code=%d body=%q", rec2.Code, rec2.Body.String())
	}

	// 3. Canceled context -> 499 client_disconnected.
	ctx3, cancel3 := context.WithCancel(context.Background())
	cancel3()
	req3, _ := http.NewRequestWithContext(ctx3, http.MethodPost, "http://gatehouse.local/v1/chat/completions", failingReadCloser{err: errors.New("read aborted")})
	req3.Header.Set("Authorization", "Bearer "+tok)
	req3.Header.Set("Content-Type", "application/json")
	rec3 := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec3, req3)
	if rec3.Code != 499 || !strings.Contains(rec3.Body.String(), `"client_disconnected"`) {
		t.Fatalf("expected 499 client_disconnected, got code=%d body=%q", rec3.Code, rec3.Body.String())
	}

	// 4. Anthropic count_tokens truncated upload -> 408 (NOT 413).
	req4, _ := http.NewRequest(http.MethodPost, "http://gatehouse.local/v1/messages/count_tokens", failingReadCloser{err: io.ErrUnexpectedEOF})
	req4.Header.Set("Authorization", "Bearer "+tok)
	req4.Header.Set("Content-Type", "application/json")
	rec4 := httptest.NewRecorder()
	srv.HTTP.Handler.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusRequestTimeout {
		t.Fatalf("expected 408 for truncated count_tokens body, got code=%d body=%q", rec4.Code, rec4.Body.String())
	}
}
