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
