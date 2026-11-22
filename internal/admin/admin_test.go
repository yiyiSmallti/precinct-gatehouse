package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/api"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/live"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/route"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/store"
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
