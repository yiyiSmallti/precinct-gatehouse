package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/api"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/live"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/store"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/updater"
)

type fakeAdminSource struct {
	remote *updater.RemoteInfo
}

func (f *fakeAdminSource) Check(_ context.Context) (*updater.RemoteInfo, error) {
	cp := *f.remote
	return &cp, nil
}

func (f *fakeAdminSource) Download(_ context.Context, _ string) (*updater.DownloadedArtifact, error) {
	return nil, nil
}

func TestAdminUpdaterEndpointsAndSecretHandling(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := SetPassword(db, "admin", "admin-pass-123"); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	initialYAML := "listen: 127.0.0.1:17788\nmode: personal\ndata_dir: " + dataDir + "\n"
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	liveMgr, err := live.New(cfg, func(c *config.Config, _ *live.Snapshot) (*live.Snapshot, error) {
		return &live.Snapshot{Cfg: c}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	updMgr := updater.NewManager(updater.Deps{
		CurrentVersion: "v0.1.1",
		ConfigPath:     cfgPath,
		Live:           liveMgr,
		DB:             db,
		ResolveSecret: func(ref string) (string, error) {
			return api.ResolveUpdaterSecret(db, ref)
		},
		NewSource: func(_ config.Updater, secret string) (updater.ArtifactSource, error) {
			if secret != "super-secret-oci-pass" {
				t.Fatalf("expected resolved secret, got %q", secret)
			}
			return &fakeAdminSource{
				remote: &updater.RemoteInfo{
					Source:  "oci",
					Version: "v0.1.2",
					Digest:  "sha256:v012",
					Tag:     "latest",
				},
			}, nil
		},
	})
	liveMgr.AddOnSwap(func(_ *live.Snapshot) {
		updMgr.NotifyConfigChange()
	})

	srv := New(Deps{
		DB:      db,
		Live:    liveMgr,
		Updater: updMgr,
		Version: "v0.1.1",
	})

	// Login to obtain admin cookie.
	loginReq, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader([]byte(`{"username":"admin","password":"admin-pass-123"}`)))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", loginRec.Code, loginRec.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == cookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("missing session cookie")
	}

	doReq := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, _ := http.NewRequest(method, path, &buf)
		req.AddCookie(sessionCookie)
		if method != http.MethodGet {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Gatehouse-Admin", "1")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// 1. GET /admin/api/config exposes app_version and has_password=false initially.
	cfgRec := doReq(http.MethodGet, "/admin/api/config", nil)
	if cfgRec.Code != http.StatusOK {
		t.Fatalf("getConfig failed: %d %s", cfgRec.Code, cfgRec.Body.String())
	}
	var cfgPayload map[string]any
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &cfgPayload); err != nil {
		t.Fatal(err)
	}
	if cfgPayload["app_version"] != "v0.1.1" {
		t.Fatalf("expected app_version=v0.1.1, got %v", cfgPayload["app_version"])
	}
	baseHash, _ := cfgPayload["hash"].(string)

	// 2. PUT /admin/api/updater/secret stores secret in DB, sets password_ref=db:password, and returns updated hash.
	secRec := doReq(http.MethodPut, "/admin/api/updater/secret", map[string]string{
		"kind":      "password",
		"value":     "super-secret-oci-pass",
		"base_hash": baseHash,
	})
	if secRec.Code != http.StatusOK {
		t.Fatalf("putUpdaterSecret failed: %d %s", secRec.Code, secRec.Body.String())
	}
	var secPayload map[string]any
	if err := json.Unmarshal(secRec.Body.Bytes(), &secPayload); err != nil {
		t.Fatal(err)
	}
	putHash, _ := secPayload["hash"].(string)
	if putHash == "" {
		t.Fatalf("expected non-empty hash from putUpdaterSecret, got %s", secRec.Body.String())
	}

	// 3. Hot-reload updater section via PUT /admin/api/config/sections/updater using returned hash directly.
	cfgRec2 := doReq(http.MethodGet, "/admin/api/config", nil)
	var cfgPayload2 map[string]any
	_ = json.Unmarshal(cfgRec2.Body.Bytes(), &cfgPayload2)
	hash2, _ := cfgPayload2["hash"].(string)
	if putHash != hash2 {
		t.Fatalf("expected putUpdaterSecret hash %q to match GET /admin/api/config hash %q", putHash, hash2)
	}

	putSecRec := doReq(http.MethodPut, "/admin/api/config/sections/updater", map[string]any{
		"base_hash": putHash,
		"data": map[string]any{
			"enabled":  true,
			"source":   "oci",
			"image":    "registry.example.com/smallti/gatehouse",
			"username": "ali",
		},
	})
	if putSecRec.Code != http.StatusOK {
		t.Fatalf("putConfigSection(updater) failed: %d %s", putSecRec.Code, putSecRec.Body.String())
	}

	// Verify secret is never leaked in GET /admin/api/config or GET /admin/api/updater.
	cfgRec3 := doReq(http.MethodGet, "/admin/api/config", nil)
	if strings.Contains(cfgRec3.Body.String(), "super-secret-oci-pass") {
		t.Fatal("plaintext updater password leaked in GET /admin/api/config")
	}
	updRec := doReq(http.MethodGet, "/admin/api/updater", nil)
	if updRec.Code != http.StatusOK {
		t.Fatalf("getUpdater failed: %d %s", updRec.Code, updRec.Body.String())
	}
	if strings.Contains(updRec.Body.String(), "super-secret-oci-pass") {
		t.Fatal("plaintext updater password leaked in GET /admin/api/updater")
	}

	// 4. POST /admin/api/updater/check exercises secret resolution + source check.
	chkRec := doReq(http.MethodPost, "/admin/api/updater/check", map[string]any{})
	if chkRec.Code != http.StatusOK {
		t.Fatalf("checkUpdater failed: %d %s", chkRec.Code, chkRec.Body.String())
	}
	var chkSt updater.Status
	if err := json.Unmarshal(chkRec.Body.Bytes(), &chkSt); err != nil {
		t.Fatal(err)
	}
	if chkSt.Available == nil || !chkSt.Available.UpdateAvailable || chkSt.Available.Version != "v0.1.2" {
		t.Fatalf("unexpected check status: %+v", chkSt.Available)
	}
	if !chkSt.Config.HasPassword {
		t.Fatal("expected has_password=true")
	}
	if chkSt.CanRestart {
		t.Fatal("expected can_restart=false when RequestRestart is nil")
	}

	// 5. Online apply & rollback return HTTP 409 restart_unavailable when unsupervised.
	applyRec := doReq(http.MethodPost, "/admin/api/updater/apply", map[string]any{})
	if applyRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 from unsupervised applyUpdater, got %d %s", applyRec.Code, applyRec.Body.String())
	}
	var applyErr map[string]any
	_ = json.Unmarshal(applyRec.Body.Bytes(), &applyErr)
	if applyErr["error"] != "restart_unavailable" {
		t.Fatalf("expected error=restart_unavailable from applyUpdater, got %+v", applyErr)
	}

	rbRec := doReq(http.MethodPost, "/admin/api/updater/rollback", map[string]any{})
	if rbRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 from unsupervised rollbackUpdater, got %d %s", rbRec.Code, rbRec.Body.String())
	}
	var rbErr map[string]any
	_ = json.Unmarshal(rbRec.Body.Bytes(), &rbErr)
	if rbErr["error"] != "restart_unavailable" {
		t.Fatalf("expected error=restart_unavailable from rollbackUpdater, got %+v", rbErr)
	}

	// 6. DELETE /admin/api/updater/secret/password clears DB secret, password_ref, and returns updated hash.
	var cfgPayload3 map[string]any
	_ = json.Unmarshal(cfgRec3.Body.Bytes(), &cfgPayload3)
	hash3, _ := cfgPayload3["hash"].(string)
	delRec := doReq(http.MethodDelete, "/admin/api/updater/secret/password", map[string]string{"base_hash": hash3})
	if delRec.Code != http.StatusOK {
		t.Fatalf("deleteUpdaterSecret failed: %d %s", delRec.Code, delRec.Body.String())
	}
	var delPayload map[string]any
	_ = json.Unmarshal(delRec.Body.Bytes(), &delPayload)
	delHash, _ := delPayload["hash"].(string)
	if delHash == "" || delHash == hash3 {
		t.Fatalf("expected new non-empty hash from deleteUpdaterSecret, got %q (prev %q)", delHash, hash3)
	}
	cfgRec4 := doReq(http.MethodGet, "/admin/api/config", nil)
	var cfgPayload4 map[string]any
	_ = json.Unmarshal(cfgRec4.Body.Bytes(), &cfgPayload4)
	if hash4, _ := cfgPayload4["hash"].(string); hash4 != delHash {
		t.Fatalf("expected deleteUpdaterSecret hash %q to match config hash %q", delHash, hash4)
	}
	if _, err := api.ResolveUpdaterSecret(db, "db:password"); err == nil {
		t.Fatal("expected db:password secret deleted")
	}
}

func TestAdminPreservesEmptyFallbackDNSAcrossSaves(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := SetPassword(db, "admin", "admin-pass-123"); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	initialYAML := "listen: 127.0.0.1:17788\nmode: personal\ndata_dir: " + dataDir + "\nupdater:\n  source: oci\n  image: registry.example.com/smallti/gatehouse\n  fallback_dns: []\n"
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	liveMgr, err := live.New(cfg, func(c *config.Config, _ *live.Snapshot) (*live.Snapshot, error) {
		return &live.Snapshot{Cfg: c}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	updMgr := updater.NewManager(updater.Deps{
		CurrentVersion: "v0.1.1",
		ConfigPath:     cfgPath,
		Live:           liveMgr,
		DB:             db,
	})
	srv := New(Deps{DB: db, Live: liveMgr, Updater: updMgr, Version: "v0.1.1"})

	loginReq, _ := http.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader([]byte(`{"username":"admin","password":"admin-pass-123"}`)))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, loginReq)
	var sessionCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == cookieName {
			sessionCookie = c
		}
	}
	doReq := func(method, path string, body any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, _ := http.NewRequest(method, path, &buf)
		req.AddCookie(sessionCookie)
		if method != http.MethodGet {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Gatehouse-Admin", "1")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// 1. Save unrelated general section while updater.fallback_dns is [].
	var cfg1 map[string]any
	_ = json.Unmarshal(doReq(http.MethodGet, "/admin/api/config", nil).Body.Bytes(), &cfg1)
	h1, _ := cfg1["hash"].(string)
	genRec := doReq(http.MethodPut, "/admin/api/config/sections/general", map[string]any{
		"base_hash": h1,
		"data": map[string]any{
			"max_body_mb":      16,
			"request_log_days": 30,
		},
	})
	if genRec.Code != http.StatusOK {
		t.Fatalf("put general failed: %d %s", genRec.Code, genRec.Body.String())
	}
	var genRes map[string]any
	_ = json.Unmarshal(genRec.Body.Bytes(), &genRes)
	h2, _ := genRes["hash"].(string)

	// 2. Save updater secret (triggers MarshalRaw serialization).
	secRec := doReq(http.MethodPut, "/admin/api/updater/secret", map[string]string{
		"kind":      "password",
		"value":     "secret-pass",
		"base_hash": h2,
	})
	if secRec.Code != http.StatusOK {
		t.Fatalf("put secret failed: %d %s", secRec.Code, secRec.Body.String())
	}

	// 3. Verify disk YAML, GET /admin/api/config, and GET /admin/api/updater all keep fallback_dns: [].
	diskBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diskBytes), "fallback_dns: []") {
		t.Fatalf("expected disk config.yaml to preserve 'fallback_dns: []', got:\n%s", string(diskBytes))
	}
	var cfg2 struct {
		Raw struct {
			Updater struct {
				FallbackDNS *[]string `json:"fallback_dns"`
			} `json:"updater"`
		} `json:"raw"`
	}
	_ = json.Unmarshal(doReq(http.MethodGet, "/admin/api/config", nil).Body.Bytes(), &cfg2)
	if cfg2.Raw.Updater.FallbackDNS == nil || len(*cfg2.Raw.Updater.FallbackDNS) != 0 {
		t.Fatalf("expected raw.updater.fallback_dns=[] in GET /admin/api/config, got %+v", cfg2.Raw.Updater.FallbackDNS)
	}
	var st updater.Status
	_ = json.Unmarshal(doReq(http.MethodGet, "/admin/api/updater", nil).Body.Bytes(), &st)
	if st.Config.FallbackDNS == nil || len(st.Config.FallbackDNS) != 0 {
		t.Fatalf("expected empty non-nil fallback_dns in GET /admin/api/updater, got %#v", st.Config.FallbackDNS)
	}
}
