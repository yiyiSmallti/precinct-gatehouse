package api

import (
	"bytes"
	"context"
	"encoding/json"
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
