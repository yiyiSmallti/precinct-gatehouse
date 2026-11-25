package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/api"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/keys"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/limit"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/live"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/route"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/store"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/tiered"
)

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	if err := SetPassword(db, "admin", "old-password-123"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	srv := New(Deps{DB: db})

	loginWith := func(pwd string) string {
		t.Helper()
		body := []byte(`{"username":"admin","password":"` + pwd + `"}`)
		req, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login failed status=%d body=%s", rec.Code, rec.Body.String())
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == cookieName {
				return c.Value
			}
		}
		t.Fatal("missing session cookie from login")
		return ""
	}

	checkMe := func(tok string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "/admin/api/me", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}

	tokA := loginWith("old-password-123")
	tokB := loginWith("old-password-123")
	if checkMe(tokA) != http.StatusOK || checkMe(tokB) != http.StatusOK {
		t.Fatalf("expected both initial sessions authorized")
	}

	// Session B rotates the password: other sessions (tokA) and old tokB must be invalidated,
	// while a rotated cookie is issued for Session B.
	req, _ := http.NewRequest(http.MethodPost, "/admin/api/password", bytes.NewReader([]byte(`{"old":"old-password-123","new":"new-password-456"}`)))
	req.AddCookie(&http.Cookie{Name: cookieName, Value: tokB})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gatehouse-Admin", "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("changePassword status=%d body=%s", rec.Code, rec.Body.String())
	}
	var tokB2 string
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			tokB2 = c.Value
		}
	}
	if tokB2 == "" || tokB2 == tokB {
		t.Fatalf("expected fresh replacement session cookie, got %q (old %q)", tokB2, tokB)
	}

	if st := checkMe(tokA); st != http.StatusUnauthorized {
		t.Fatalf("expected session A revoked after password change, got %d", st)
	}
	if st := checkMe(tokB); st != http.StatusUnauthorized {
		t.Fatalf("expected old session B token revoked after password change, got %d", st)
	}
	if st := checkMe(tokB2); st != http.StatusOK {
		t.Fatalf("expected replacement session B2 authorized, got %d", st)
	}

	// Out-of-band password reset (e.g. CLI set-password) must also invalidate active sessions immediately.
	if err := SetPassword(db, "admin", "cli-reset-pass-789"); err != nil {
		t.Fatalf("SetPassword CLI reset: %v", err)
	}
	if st := checkMe(tokB2); st != http.StatusUnauthorized {
		t.Fatalf("expected session B2 revoked after out-of-band password change, got %d", st)
	}
}

func newTestLiveManager(t *testing.T, dir, initialYAML string, resolveKey func(string) (string, error)) (*live.Manager, string) {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(initialYAML), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := live.New(cfg, func(next *config.Config, _ *live.Snapshot) (*live.Snapshot, error) {
		reg, err := provider.NewRegistry(next, resolveKey)
		if err != nil {
			return nil, err
		}
		red, err := redact.New(next.Redaction, nil, nil)
		if err != nil {
			return nil, err
		}
		return &live.Snapshot{
			Cfg:      next,
			Reg:      reg,
			Router:   route.New(next, reg),
			Redactor: red,
		}, nil
	})
	if err != nil {
		t.Fatalf("live.New: %v", err)
	}
	return mgr, cfgPath
}

func TestLiveSessionTTLReload(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := SetPassword(db, "admin", "password-123"); err != nil {
		t.Fatal(err)
	}

	cfgWithTTL := func(ttl string) string {
		return fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
admin:
  session_ttl: %s
providers:
  - name: p
    base_url: http://127.0.0.1:18080/v1
    models: [m1]
`, dir, ttl)
	}

	mgr, _ := newTestLiveManager(t, dir, cfgWithTTL("12h"), nil)
	srv := New(Deps{DB: db, Live: mgr, SessionTTL: 12 * time.Hour})

	doLogin := func() *http.Cookie {
		req, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader([]byte(`{"username":"admin","password":"password-123"}`)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == cookieName {
				return c
			}
		}
		t.Fatal("missing cookie")
		return nil
	}

	c1 := doLogin()
	if c1.MaxAge != 43200 {
		t.Fatalf("expected initial MaxAge=43200, got %d", c1.MaxAge)
	}

	// Shorten admin.session_ttl to 45s -> existing cookie remains bounded? Wait: also verify cookie MaxAge & active enforcement!
	if _, err := mgr.Apply([]byte(cfgWithTTL("45s")), "test", mgr.Current().Cfg.Hash()); err != nil {
		t.Fatalf("apply 45s: %v", err)
	}
	c2 := doLogin()
	if c2.MaxAge != 45 {
		t.Fatalf("expected updated login MaxAge=45 after hot reload, got %d", c2.MaxAge)
	}

	// Shorten admin.session_ttl to 20ms -> previously issued sessions older than 20ms become unauthorized immediately.
	time.Sleep(35 * time.Millisecond)
	if _, err := mgr.Apply([]byte(cfgWithTTL("20ms")), "test", mgr.Current().Cfg.Hash()); err != nil {
		t.Fatalf("apply 20ms: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "/admin/api/me", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: c1.Value})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected shortened live session_ttl to expire existing session, got %d", rec.Code)
	}
}

func TestApplyDetectsExternalDiskEditAndSerializesProviderKey(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := SetPassword(db, "admin", "password-123"); err != nil {
		t.Fatal(err)
	}

	var seenKeys sync.Map
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKeys.Store(r.Header.Get("Authorization"), true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	cfgWithDays := func(days int) string {
		return fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
request_log_days: %d
providers:
  - name: p
    base_url: %s/v1
    models: [m1]
`, dir, days, up.URL)
	}

	mgr, cfgPath := newTestLiveManager(t, dir, cfgWithDays(90), func(ref string) (string, error) {
		return api.ResolveKey(db, ref)
	})
	srv := New(Deps{DB: db, Live: mgr})

	reqLogin, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader([]byte(`{"username":"admin","password":"password-123"}`)))
	reqLogin.Header.Set("Content-Type", "application/json")
	recLogin := httptest.NewRecorder()
	srv.ServeHTTP(recLogin, reqLogin)
	var tok string
	for _, c := range recLogin.Result().Cookies() {
		if c.Name == cookieName {
			tok = c.Value
		}
	}
	if tok == "" {
		t.Fatal("missing admin cookie")
	}

	// 1. Concurrent PUT /admin/api/providers/p/key calls with the same base_hash must serialize:
	// one wins (200) and the other gets 409 Conflict, and DB setting matches active snapshot's key.
	baseHash := mgr.Current().Cfg.Hash()
	type putRes struct {
		code int
		key  string
	}
	results := make(chan putRes, 2)
	var wg sync.WaitGroup
	for _, k := range []string{"sk-key-A", "sk-key-B"} {
		wg.Add(1)
		go func(kVal string) {
			defer wg.Done()
			payload, _ := json.Marshal(map[string]string{"key": kVal, "base_hash": baseHash})
			req, _ := http.NewRequest(http.MethodPut, "/admin/api/providers/p/key", bytes.NewReader(payload))
			req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
			req.Header.Set("X-Gatehouse-Admin", "1")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			results <- putRes{code: rec.Code, key: kVal}
		}(k)
	}
	wg.Wait()
	close(results)

	var winner string
	var conflicts int
	for r := range results {
		switch r.code {
		case http.StatusOK:
			winner = r.key
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent putProviderKey status: %d (key=%s)", r.code, r.key)
		}
	}
	if winner == "" || conflicts != 1 {
		t.Fatalf("expected 1 winner and 1 conflict, got winner=%q conflicts=%d", winner, conflicts)
	}

	dbKey, err := api.ResolveKey(db, "db:p")
	if err != nil || dbKey != winner {
		t.Fatalf("expected DB provider key %q to match winning request, got %q (err=%v)", winner, dbKey, err)
	}
	prov, _, ok := mgr.Current().Reg.Lookup("p/m1")
	if !ok {
		t.Fatal("missing provider p/m1")
	}
	if _, err := prov.Chat(t.Context(), []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("prov.Chat: %v", err)
	}
	if _, ok := seenKeys.Load("Bearer " + winner); !ok {
		t.Fatalf("expected active snapshot registry to use winning key %q", winner)
	}

	// 2. External disk edit before watcher runs must trigger 409 Conflict instead of silently overwriting disk.
	staleHash := mgr.Current().Cfg.Hash()
	externalYAML := cfgWithDays(14)
	if err := os.WriteFile(cfgPath, []byte(externalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	genBody, _ := json.Marshal(map[string]any{
		"base_hash": staleHash,
		"data":      map[string]any{"request_log_days": 30},
	})
	reqPut, _ := http.NewRequest(http.MethodPut, "/admin/api/config/sections/general", bytes.NewReader(genBody))
	reqPut.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
	reqPut.Header.Set("X-Gatehouse-Admin", "1")
	reqPut.Header.Set("Content-Type", "application/json")
	recPut := httptest.NewRecorder()
	srv.ServeHTTP(recPut, reqPut)
	if recPut.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when config.yaml changed on disk before Apply, got %d (%s)", recPut.Code, recPut.Body.String())
	}
	diskNow, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(diskNow) != externalYAML {
		t.Fatalf("expected external on-disk edit to remain intact on conflict")
	}
}

func TestBatchConfigSectionsInterdependentRename(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := SetPassword(db, "admin", "password-123"); err != nil {
		t.Fatal(err)
	}

	initialYAML := fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
providers:
  - name: p
    base_url: http://127.0.0.1:18080/v1
    models: [m1]
default_group: main
routing_groups:
  main:
    members: [p/m1]
model_aliases:
  - match: friendly
    use: main
`, dir)

	mgr, _ := newTestLiveManager(t, dir, initialYAML, nil)
	srv := New(Deps{DB: db, Live: mgr})

	reqLogin, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader([]byte(`{"username":"admin","password":"password-123"}`)))
	reqLogin.Header.Set("Content-Type", "application/json")
	recLogin := httptest.NewRecorder()
	srv.ServeHTTP(recLogin, reqLogin)
	var tok string
	for _, c := range recLogin.Result().Cookies() {
		if c.Name == cookieName {
			tok = c.Value
		}
	}
	if tok == "" {
		t.Fatal("missing admin cookie")
	}

	baseHash := mgr.Current().Cfg.Hash()
	sections := map[string]any{
		"routing": map[string]any{
			"default_group": "primary",
			"routing_groups": map[string]any{
				"primary": map[string]any{
					"members": []string{"p/m1"},
				},
			},
		},
		"aliases": []map[string]any{
			{
				"match": "friendly",
				"use":   "primary",
			},
		},
	}

	valBody, _ := json.Marshal(map[string]any{"sections": sections})
	reqVal, _ := http.NewRequest(http.MethodPost, "/admin/api/config/validate", bytes.NewReader(valBody))
	reqVal.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
	reqVal.Header.Set("X-Gatehouse-Admin", "1")
	reqVal.Header.Set("Content-Type", "application/json")
	recVal := httptest.NewRecorder()
	srv.ServeHTTP(recVal, reqVal)
	if recVal.Code != http.StatusOK {
		t.Fatalf("expected validate sections 200 OK, got %d (%s)", recVal.Code, recVal.Body.String())
	}

	putBody, _ := json.Marshal(map[string]any{
		"base_hash": baseHash,
		"sections":  sections,
	})
	reqPut, _ := http.NewRequest(http.MethodPut, "/admin/api/config/sections", bytes.NewReader(putBody))
	reqPut.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
	reqPut.Header.Set("X-Gatehouse-Admin", "1")
	reqPut.Header.Set("Content-Type", "application/json")
	recPut := httptest.NewRecorder()
	srv.ServeHTTP(recPut, reqPut)
	if recPut.Code != http.StatusOK {
		t.Fatalf("expected PUT /admin/api/config/sections 200 OK, got %d (%s)", recPut.Code, recPut.Body.String())
	}

	cur := mgr.Current().Cfg
	if cur.DefaultGroup != "primary" || cur.RoutingGroups["primary"] == nil || cur.RoutingGroups["main"] != nil {
		t.Fatalf("expected routing group renamed main -> primary, got default=%q groups=%v", cur.DefaultGroup, cur.RoutingGroups)
	}
	if len(cur.ModelAliases) != 1 || cur.ModelAliases[0].Match != "friendly" || cur.ModelAliases[0].Use != "primary" {
		t.Fatalf("expected alias friendly -> primary, got %+v", cur.ModelAliases)
	}
}

func TestContextCompressSecretRedactionAndCheckpointsAPI(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := SetPassword(db, "admin", "password-123"); err != nil {
		t.Fatal(err)
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	initialYAML := fmt.Sprintf(`
listen: 127.0.0.1:0
mode: personal
data_dir: %s
providers:
  - name: p
    target: local
    models: [m1]
    tiered:
      mode: active
      health_interval_sec: 1
      zh_think_anchor: false
      starter_cache:
        enabled: false
      context_compress:
        enabled: true
        stage2_enabled: false
        trigger_tokens: 500
        stage1_target_tokens: 200
        keep_recent_groups: 2
        keep_recent_min_tokens: 20
        keep_recent_max_tokens: 120
        assistant_max_chars: 120
        user_paste_max_chars: 120
        summarizer_url: %s
        summarizer_api_key: sk-inline-secret
      backends:
        - id: b1
          url: %s
          tier: 1
          max_context_tokens: 32000
          max_safe_input: 16000
          promote_at: 8000
          speed_score: 100
default_group: main
routing_groups:
  main:
    members: [p/m1]
`, dir, up.URL, up.URL)

	mgr, cfgPath := newTestLiveManager(t, dir, initialYAML, nil)
	hub := tiered.NewHub(dir, nil)
	defer func() { _ = hub.Close() }()
	hub.Activate(mgr.Current().Cfg)

	srv := New(Deps{DB: db, Live: mgr, Tiered: hub})

	reqLogin, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader([]byte(`{"username":"admin","password":"password-123"}`)))
	reqLogin.Header.Set("Content-Type", "application/json")
	recLogin := httptest.NewRecorder()
	srv.ServeHTTP(recLogin, reqLogin)
	var tok string
	for _, c := range recLogin.Result().Cookies() {
		if c.Name == cookieName {
			tok = c.Value
		}
	}
	if tok == "" {
		t.Fatal("missing admin cookie")
	}

	admDo := func(method, path string, body any) (int, map[string]any, string) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		var rdr *bytes.Reader
		if body != nil {
			rdr = bytes.NewReader(buf.Bytes())
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, path, rdr)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
		req.Header.Set("X-Gatehouse-Admin", "1")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Body.String()
	}

	// 1. GET /admin/api/config must redact summarizer_api_key and expose has_summarizer_key=true.
	codeCfg, cfgResp, rawCfg := admDo(http.MethodGet, "/admin/api/config", nil)
	if codeCfg != http.StatusOK || bytes.Contains([]byte(rawCfg), []byte("sk-inline-secret")) {
		t.Fatalf("GET /admin/api/config status=%d leaked secret or failed: %s", codeCfg, rawCfg)
	}
	rawMap, _ := cfgResp["raw"].(map[string]any)
	provs, _ := rawMap["providers"].([]any)
	p0, _ := provs[0].(map[string]any)
	t0, _ := p0["tiered"].(map[string]any)
	cc0, _ := t0["context_compress"].(map[string]any)
	if cc0 == nil || cc0["has_summarizer_key"] != true {
		t.Fatalf("expected has_summarizer_key=true in redacted config, got %+v", cc0)
	}
	if v, ok := cc0["summarizer_api_key"].(string); ok && v != "" {
		t.Fatalf("expected empty/omitted summarizer_api_key, got %q", v)
	}

	// 2. PUT /admin/api/config/sections/providers without summarizer_api_key/key_ref must preserve inline key on disk and in live snapshot.
	baseHash, _ := cfgResp["hash"].(string)
	codePut, _, rawPut := admDo(http.MethodPut, "/admin/api/config/sections/providers", map[string]any{
		"base_hash": baseHash,
		"data":      provs,
	})
	if codePut != http.StatusOK {
		t.Fatalf("PUT /admin/api/config/sections/providers status=%d body=%s", codePut, rawPut)
	}
	liveCC := mgr.Current().Cfg.Providers[0].Tiered.ContextCompress
	if liveCC == nil || liveCC.SummarizerAPIKey != "sk-inline-secret" || liveCC.HasSummarizerKey {
		t.Fatalf("expected inline summarizer_api_key preserved without has_summarizer_key persisted, got %+v", liveCC)
	}
	diskBytes, _ := os.ReadFile(cfgPath)
	if !bytes.Contains(diskBytes, []byte("sk-inline-secret")) || bytes.Contains(diskBytes, []byte("has_summarizer_key")) {
		t.Fatalf("expected config.yaml to retain sk-inline-secret without has_summarizer_key, got:\n%s", diskBytes)
	}

	// 3. Exercise Checkpoints Admin API (list, 404 on unknown, delete by id, clear all).
	deadline := time.Now().Add(4 * time.Second)
	healthy := false
	for time.Now().Before(deadline) {
		_, stMap, _ := admDo(http.MethodGet, "/admin/api/tiered", nil)
		if ps, _ := stMap["providers"].([]any); len(ps) > 0 {
			if st0, _ := ps[0].(map[string]any); st0 != nil {
				if bs, _ := st0["backends"].([]any); len(bs) > 0 {
					if b0, _ := bs[0].(map[string]any); b0 != nil && b0["healthy"] == true {
						healthy = true
						break
					}
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("backend b1 did not become healthy in time")
	}

	tr, err := hub.Transport("p", "local", mgr.Current().Cfg.Providers[0].Tiered, nil)
	if err != nil {
		t.Fatalf("hub.Transport: %v", err)
	}
	triggerCP := func(marker string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"model": "m1",
			"messages": []map[string]string{
				{"role": "user", "content": "Rule " + marker},
				{"role": "assistant", "content": string(bytes.Repeat([]byte("Assistant long analysis block "), 55))},
				{"role": "user", "content": string(bytes.Repeat([]byte("User long detail block "), 55))},
				{"role": "assistant", "content": string(bytes.Repeat([]byte("Second assistant block "), 55))},
				{"role": "user", "content": "Recent follow up."},
				{"role": "assistant", "content": "Short ok."},
				{"role": "user", "content": "Final question."},
			},
		})
		req, _ := http.NewRequest(http.MethodPost, "http://tiered.invalid/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("tr.RoundTrip: %v", err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("X-Context-Compressed"); got != "stage1" {
			t.Fatalf("expected X-Context-Compressed=stage1, got %q", got)
		}
	}

	triggerCP("A")
	cList, cpResp, rawCP := admDo(http.MethodGet, "/admin/api/tiered/p/checkpoints", nil)
	cpList, _ := cpResp["checkpoints"].([]any)
	if cList != http.StatusOK || len(cpList) != 1 {
		t.Fatalf("expected 1 checkpoint on GET, got status=%d body=%s", cList, rawCP)
	}
	cpID := fmt.Sprint(cpList[0].(map[string]any)["id"])

	cMiss, _, _ := admDo(http.MethodDelete, "/admin/api/tiered/p/checkpoints/nonexistent-id", nil)
	if cMiss != http.StatusNotFound {
		t.Fatalf("expected 404 deleting unknown checkpoint, got %d", cMiss)
	}

	cDel, _, rawDel := admDo(http.MethodDelete, "/admin/api/tiered/p/checkpoints/"+cpID, nil)
	if cDel != http.StatusOK {
		t.Fatalf("expected 200 deleting checkpoint %s, got %d (%s)", cpID, cDel, rawDel)
	}

	triggerCP("B")
	cClr, clrResp, rawClr := admDo(http.MethodDelete, "/admin/api/tiered/p/checkpoints?all=1", nil)
	if cClr != http.StatusOK || clrResp["cleared"] != float64(1) {
		t.Fatalf("expected cleared=1 on DELETE ?all=1, got %d (%s)", cClr, rawClr)
	}
	_, afterResp, rawAfter := admDo(http.MethodGet, "/admin/api/tiered/p/checkpoints", nil)
	if arr, _ := afterResp["checkpoints"].([]any); len(arr) != 0 {
		t.Fatalf("expected 0 checkpoints after clear, got %s", rawAfter)
	}
}

func TestAdminLogsExposesBodyReadMetricsAndMigratesLegacyDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "gatehouse.db")

	// Seed a legacy gatehouse.db without body_read_ms / request_bytes on request_log.
	legacyDB, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open seed: %v", err)
	}
	legacyDB.Close()

	// Recreate request_log explicitly without body_read_ms and request_bytes to test migration.
	reopenRaw, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open raw: %v", err)
	}
	_, err = reopenRaw.Exec(`DROP TABLE request_log;
CREATE TABLE request_log (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id        TEXT NOT NULL,
  ts                INTEGER NOT NULL,
  day               TEXT NOT NULL,
  key_id            TEXT NOT NULL DEFAULT '',
  key_name          TEXT NOT NULL DEFAULT '',
  protocol          TEXT NOT NULL DEFAULT '',
  client_ip         TEXT NOT NULL DEFAULT '',
  requested_model   TEXT NOT NULL DEFAULT '',
  model             TEXT NOT NULL DEFAULT '',
  status            INTEGER NOT NULL,
  error_code        TEXT NOT NULL DEFAULT '',
  stream            INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  latency_ms        INTEGER NOT NULL DEFAULT 0,
  ttft_ms           INTEGER NOT NULL DEFAULT 0,
  admitted          INTEGER NOT NULL DEFAULT 0,
  redaction_mode    TEXT NOT NULL DEFAULT '',
  redaction_count   INTEGER NOT NULL DEFAULT 0,
  route_backend     TEXT NOT NULL DEFAULT '',
  route_reason      TEXT NOT NULL DEFAULT ''
);
INSERT INTO request_log(request_id,ts,day,key_name,protocol,model,status,stream,prompt_tokens,completion_tokens,cached_tokens,latency_ms,ttft_ms,admitted)
VALUES('req_legacy',1795520000,'2026-11-24','legacy-key','chat','ds/m1',200,1,1000,100,800,5000,1200,1);`)
	if err != nil {
		reopenRaw.Close()
		t.Fatalf("seed legacy schema: %v", err)
	}
	reopenRaw.Close()

	// Re-open with store.Open to exercise ensureColumn migration on existing gatehouse.db.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected db file %s: %v", dbPath, err)
	}
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open migrate: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`INSERT INTO request_log(request_id,ts,day,key_name,protocol,model,status,stream,prompt_tokens,completion_tokens,cached_tokens,latency_ms,ttft_ms,body_read_ms,request_bytes,admitted)
VALUES('req_slow_upload',1795523495,'2026-11-24','prod-key','chat','ds/m1',200,1,268133,1686,266752,36000,7250,112000,94000000,1);`)
	if err != nil {
		t.Fatalf("insert new row: %v", err)
	}

	if err := SetPassword(db, "admin", "password-123"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	srv := New(Deps{DB: db})

	loginReq, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader([]byte(`{"username":"admin","password":"password-123"}`)))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", loginRec.Code, loginRec.Body.String())
	}
	var tok string
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == cookieName {
			tok = c.Value
		}
	}

	req, _ := http.NewRequest(http.MethodGet, "/admin/api/logs?limit=10", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/logs status=%d body=%s", rec.Code, rec.Body.String())
	}

	var parsed struct {
		Logs []struct {
			RequestID    string `json:"request_id"`
			LatencyMS    int64  `json:"latency_ms"`
			TTFTMS       int64  `json:"ttft_ms"`
			BodyReadMS   int64  `json:"body_read_ms"`
			RequestBytes int64  `json:"request_bytes"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode logs response: %v", err)
	}
	if len(parsed.Logs) != 2 {
		t.Fatalf("expected 2 log rows, got %d (%s)", len(parsed.Logs), rec.Body.String())
	}
	if parsed.Logs[0].RequestID != "req_slow_upload" || parsed.Logs[0].BodyReadMS != 112000 || parsed.Logs[0].RequestBytes != 94000000 || parsed.Logs[0].TTFTMS != 7250 {
		t.Fatalf("unexpected migrated new row: %+v", parsed.Logs[0])
	}
	if parsed.Logs[1].RequestID != "req_legacy" || parsed.Logs[1].BodyReadMS != 0 || parsed.Logs[1].RequestBytes != 0 || parsed.Logs[1].TTFTMS != 1200 {
		t.Fatalf("unexpected migrated legacy row: %+v", parsed.Logs[1])
	}
}

func TestAdminSessionPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	db1, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open 1: %v", err)
	}
	if err := SetPassword(db1, "admin", "password-123"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	srv1 := New(Deps{DB: db1, SessionTTL: 12 * time.Hour, IdleTimeout: time.Hour})
	loginReq, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader([]byte(`{"username":"admin","password":"password-123"}`)))
	loginReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv1.ServeHTTP(rec, loginReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatal("missing session cookie")
	}

	// Verify only hashed token is stored in admin_sessions (raw token must not appear).
	var rawCount, hashCount int
	_ = db1.QueryRow(`SELECT COUNT(*) FROM admin_sessions WHERE token_hash = ?`, sessionCookie.Value).Scan(&rawCount)
	sum := sha256.Sum256([]byte(sessionCookie.Value))
	tokHash := hex.EncodeToString(sum[:])
	_ = db1.QueryRow(`SELECT COUNT(*) FROM admin_sessions WHERE token_hash = ?`, tokHash).Scan(&hashCount)
	if rawCount != 0 || hashCount != 1 {
		t.Fatalf("expected hashed token only (rawCount=%d, hashCount=%d)", rawCount, hashCount)
	}

	// Simulate process restart by closing DB 1 and opening fresh DB 2 + Server 2.
	_ = db1.Close()
	db2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open 2: %v", err)
	}
	defer db2.Close()

	srv2 := New(Deps{DB: db2, SessionTTL: 12 * time.Hour, IdleTimeout: time.Hour})
	meReq, _ := http.NewRequest(http.MethodGet, "/admin/api/me", nil)
	meReq.AddCookie(&http.Cookie{Name: cookieName, Value: sessionCookie.Value})
	meRec := httptest.NewRecorder()
	srv2.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("expected persisted session to stay valid across restart, got %d (%s)", meRec.Code, meRec.Body.String())
	}

	// Logout on restarted server removes the session from SQLite.
	outReq, _ := http.NewRequest(http.MethodPost, "/admin/api/logout", nil)
	outReq.Header.Set("X-Gatehouse-Admin", "1")
	outReq.AddCookie(&http.Cookie{Name: cookieName, Value: sessionCookie.Value})
	outRec := httptest.NewRecorder()
	srv2.ServeHTTP(outRec, outReq)
	if outRec.Code != http.StatusOK {
		t.Fatalf("logout failed: %d", outRec.Code)
	}

	meReq2, _ := http.NewRequest(http.MethodGet, "/admin/api/me", nil)
	meReq2.AddCookie(&http.Cookie{Name: cookieName, Value: sessionCookie.Value})
	meRec2 := httptest.NewRecorder()
	srv2.ServeHTTP(meRec2, meReq2)
	if meRec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", meRec2.Code)
	}
}

func TestAdminSessionIdleTimeoutAndBackgroundPoll(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	if err := SetPassword(db, "admin", "password-123"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	srv := New(Deps{DB: db, SessionTTL: 12 * time.Hour, IdleTimeout: 300 * time.Millisecond})
	loginReq, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader([]byte(`{"username":"admin","password":"password-123"}`)))
	loginReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, loginReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d", rec.Code)
	}
	var tok string
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			tok = c.Value
		}
	}

	callMe := func(background bool) int {
		req, _ := http.NewRequest(http.MethodGet, "/admin/api/me", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
		if background {
			req.Header.Set("X-Gatehouse-Background", "1")
		}
		r := httptest.NewRecorder()
		srv.ServeHTTP(r, req)
		return r.Code
	}

	// Active request after 150ms (> touchThrottle=75ms) extends the idle window.
	time.Sleep(150 * time.Millisecond)
	if code := callMe(false); code != http.StatusOK {
		t.Fatalf("expected 200 at 150ms, got %d", code)
	}

	// 200ms after active request, session is still valid; background poll does NOT refresh idle timer.
	time.Sleep(200 * time.Millisecond)
	if code := callMe(true); code != http.StatusOK {
		t.Fatalf("expected 200 for background poll at 200ms since active touch, got %d", code)
	}

	// Another 160ms (now 360ms since last active request > 300ms idle timeout): must expire!
	time.Sleep(160 * time.Millisecond)
	if code := callMe(true); code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after idle expiry despite background polls, got %d", code)
	}

	// Negative IdleTimeout disables idle expiry completely.
	srvNoIdle := New(Deps{DB: db, SessionTTL: 12 * time.Hour, IdleTimeout: -1 * time.Second})
	loginReq2, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader([]byte(`{"username":"admin","password":"password-123"}`)))
	loginReq2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	srvNoIdle.ServeHTTP(rec2, loginReq2)
	var tok2 string
	for _, c := range rec2.Result().Cookies() {
		if c.Name == cookieName {
			tok2 = c.Value
		}
	}
	time.Sleep(50 * time.Millisecond)
	req2, _ := http.NewRequest(http.MethodGet, "/admin/api/me", nil)
	req2.AddCookie(&http.Cookie{Name: cookieName, Value: tok2})
	r2 := httptest.NewRecorder()
	srvNoIdle.ServeHTTP(r2, req2)
	if r2.Code != http.StatusOK {
		t.Fatalf("expected 200 with negative idle timeout, got %d", r2.Code)
	}
}

func TestBrandingEndpointsAndKeySecretAPI(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	if err := SetPassword(db, "admin", "password-123"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	ks := keys.NewStore(db)
	defer ks.Close()
	// Create one legacy key before SetSecretKey, then enable encryption.
	_, legacyKey, err := ks.Create(keys.CreateOpts{Name: "legacy"})
	if err != nil {
		t.Fatalf("Create legacy: %v", err)
	}
	ks.SetSecretKey([]byte("test-user-key-32-bytes-0123456789"))

	srv := New(Deps{DB: db, Keys: ks, Limiter: limit.New(db)})

	// 1. Public branding works without authentication and returns defaults.
	pubReq, _ := http.NewRequest(http.MethodGet, "/admin/api/branding", nil)
	pubRec := httptest.NewRecorder()
	srv.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/branding status=%d body=%s", pubRec.Code, pubRec.Body.String())
	}
	var pubMap map[string]any
	if err := json.Unmarshal(pubRec.Body.Bytes(), &pubMap); err != nil {
		t.Fatal(err)
	}
	if pubMap["has_logo"] != false || pubMap["display_name"] != "" {
		t.Fatalf("unexpected default branding: %v", pubMap)
	}

	// Default logo endpoint returns 404 when no custom logo is uploaded.
	logo404Req, _ := http.NewRequest(http.MethodGet, "/admin/api/branding/logo", nil)
	logo404Rec := httptest.NewRecorder()
	srv.ServeHTTP(logo404Rec, logo404Req)
	if logo404Rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 before logo upload, got %d", logo404Rec.Code)
	}

	// Protected branding settings endpoint requires auth.
	unauthReq, _ := http.NewRequest(http.MethodGet, "/admin/api/branding/settings", nil)
	unauthRec := httptest.NewRecorder()
	srv.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated branding settings, got %d", unauthRec.Code)
	}

	// Login to get session cookie.
	loginReq, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader([]byte(`{"username":"admin","password":"password-123"}`)))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login failed: %d", loginRec.Code)
	}
	var tok string
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == cookieName {
			tok = c.Value
		}
	}

	doAuth := func(method, path string, body any, csrf bool) *httptest.ResponseRecorder {
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, path, rdr)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if csrf {
			req.Header.Set("X-Gatehouse-Admin", "1")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// 2. Partial settings update & validation.
	if r := doAuth(http.MethodPut, "/admin/api/branding/settings", map[string]any{"display_name": "ACME"}, false); r.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF header, got %d", r.Code)
	}
	if r := doAuth(http.MethodPut, "/admin/api/branding/settings", map[string]any{"public_url": "ftp://bad.example.com"}, true); r.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid public_url scheme, got %d", r.Code)
	}
	if r := doAuth(http.MethodPut, "/admin/api/branding/settings", map[string]any{"display_name": "Acme AI Gateway"}, true); r.Code != http.StatusOK {
		t.Fatalf("put display_name status=%d body=%s", r.Code, r.Body.String())
	}
	rTpl := doAuth(http.MethodPut, "/admin/api/branding/settings", map[string]any{
		"public_url":    "https://llm.acme.internal/",
		"dist_template": "KEY={{API_KEY}} URL={{BASE_URL}}",
	}, true)
	if rTpl.Code != http.StatusOK {
		t.Fatalf("put template status=%d body=%s", rTpl.Code, rTpl.Body.String())
	}
	var stResp map[string]any
	_ = json.Unmarshal(rTpl.Body.Bytes(), &stResp)
	if stResp["display_name"] != "Acme AI Gateway" || stResp["public_url"] != "https://llm.acme.internal" || stResp["dist_template"] != "KEY={{API_KEY}} URL={{BASE_URL}}" {
		t.Fatalf("unexpected settings after partial updates: %v", stResp)
	}

	// Oversized template in combined payload must reject without modifying public_url.
	oversized := strings.Repeat("x", maxDistTemplateBytes+1)
	if r := doAuth(http.MethodPut, "/admin/api/branding/settings", map[string]any{
		"public_url":    "https://should-not-persist.example.com",
		"dist_template": oversized,
	}, true); r.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized dist_template, got %d", r.Code)
	}
	getSt := doAuth(http.MethodGet, "/admin/api/branding/settings", nil, false)
	var stAfter map[string]any
	_ = json.Unmarshal(getSt.Body.Bytes(), &stAfter)
	if stAfter["public_url"] != "https://llm.acme.internal" {
		t.Fatalf("expected public_url unchanged after rejected save, got %v", stAfter)
	}

	// Public endpoint exposes display_name only (not public_url or dist_template).
	pubRec2 := httptest.NewRecorder()
	srv.ServeHTTP(pubRec2, pubReq)
	var pubMap2 map[string]any
	_ = json.Unmarshal(pubRec2.Body.Bytes(), &pubMap2)
	if pubMap2["display_name"] != "Acme AI Gateway" {
		t.Fatalf("expected public display_name updated, got %v", pubMap2)
	}
	if _, hasURL := pubMap2["public_url"]; hasURL {
		t.Fatalf("public endpoint must not expose public_url: %v", pubMap2)
	}

	// 3. Logo upload validation (reject non-image & active SVG; accept valid PNG).
	badText := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not an image"))
	if r := doAuth(http.MethodPut, "/admin/api/branding/logo", map[string]any{"data_url": badText}, true); r.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for fake PNG payload, got %d", r.Code)
	}
	badSVG := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	if r := doAuth(http.MethodPut, "/admin/api/branding/logo", map[string]any{"data_url": badSVG}, true); r.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for SVG with script, got %d", r.Code)
	}

	// Valid 1x1 PNG.
	pngBytes, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	goodPNG := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	upRec := doAuth(http.MethodPut, "/admin/api/branding/logo", map[string]any{"data_url": goodPNG}, true)
	if upRec.Code != http.StatusOK {
		t.Fatalf("upload PNG status=%d body=%s", upRec.Code, upRec.Body.String())
	}
	var upBody map[string]any
	_ = json.Unmarshal(upRec.Body.Bytes(), &upBody)
	logoURL, _ := upBody["logo_url"].(string)
	if upBody["has_logo"] != true || !strings.HasPrefix(logoURL, "/admin/api/branding/logo?v=") {
		t.Fatalf("unexpected upload response: %v", upBody)
	}

	// Fetch public logo without auth.
	getLogoReq, _ := http.NewRequest(http.MethodGet, logoURL, nil)
	getLogoRec := httptest.NewRecorder()
	srv.ServeHTTP(getLogoRec, getLogoReq)
	if getLogoRec.Code != http.StatusOK || !bytes.Equal(getLogoRec.Body.Bytes(), pngBytes) {
		t.Fatalf("GET logo status=%d len=%d", getLogoRec.Code, getLogoRec.Body.Len())
	}
	if getLogoRec.Header().Get("X-Content-Type-Options") != "nosniff" || getLogoRec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("missing security headers on logo response: %v", getLogoRec.Header())
	}

	// Delete logo reverts to 404.
	if r := doAuth(http.MethodDelete, "/admin/api/branding/logo", nil, true); r.Code != http.StatusOK {
		t.Fatalf("delete logo status=%d", r.Code)
	}
	getLogoRec2 := httptest.NewRecorder()
	srv.ServeHTTP(getLogoRec2, logo404Req)
	if getLogoRec2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete logo, got %d", getLogoRec2.Code)
	}

	// 4. Key secret retrieval endpoint.
	createRec := doAuth(http.MethodPost, "/admin/api/keys", map[string]any{"name": "dist-key"}, true)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create key status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var createdPayload struct {
		Key    string   `json:"key"`
		APIKey keys.Key `json:"api_key"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &createdPayload); err != nil {
		t.Fatal(err)
	}

	secRec := doAuth(http.MethodGet, "/admin/api/keys/"+createdPayload.APIKey.ID+"/secret", nil, false)
	if secRec.Code != http.StatusOK {
		t.Fatalf("getKeySecret status=%d body=%s", secRec.Code, secRec.Body.String())
	}
	var secBody map[string]any
	_ = json.Unmarshal(secRec.Body.Bytes(), &secBody)
	if secBody["has_secret"] != true || secBody["key"] != createdPayload.Key {
		t.Fatalf("unexpected key secret response: %v (want %q)", secBody, createdPayload.Key)
	}

	legRec := doAuth(http.MethodGet, "/admin/api/keys/"+legacyKey.ID+"/secret", nil, false)
	if legRec.Code != http.StatusOK {
		t.Fatalf("getKeySecret legacy status=%d", legRec.Code)
	}
	var legBody map[string]any
	_ = json.Unmarshal(legRec.Body.Bytes(), &legBody)
	if legBody["has_secret"] != false || legBody["key"] != "" {
		t.Fatalf("expected empty secret for legacy key, got %v", legBody)
	}

	missRec := doAuth(http.MethodGet, "/admin/api/keys/key_does_not_exist/secret", nil, false)
	if missRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown key id, got %d", missRec.Code)
	}
}
