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

	// 2. PUT /admin/api/updater/secret stores secret in DB and sets password_ref=db:password.
	secRec := doReq(http.MethodPut, "/admin/api/updater/secret", map[string]string{
		"kind":      "password",
		"value":     "super-secret-oci-pass",
		"base_hash": baseHash,
	})
	if secRec.Code != http.StatusOK {
		t.Fatalf("putUpdaterSecret failed: %d %s", secRec.Code, secRec.Body.String())
	}

	// 3. Hot-reload updater section via PUT /admin/api/config/sections/updater without losing password_ref.
	cfgRec2 := doReq(http.MethodGet, "/admin/api/config", nil)
	var cfgPayload2 map[string]any
	_ = json.Unmarshal(cfgRec2.Body.Bytes(), &cfgPayload2)
	hash2, _ := cfgPayload2["hash"].(string)

	putSecRec := doReq(http.MethodPut, "/admin/api/config/sections/updater", map[string]any{
		"base_hash": hash2,
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

	// 5. DELETE /admin/api/updater/secret/password clears DB secret and password_ref.
	var cfgPayload3 map[string]any
	_ = json.Unmarshal(cfgRec3.Body.Bytes(), &cfgPayload3)
	hash3, _ := cfgPayload3["hash"].(string)
	delRec := doReq(http.MethodDelete, "/admin/api/updater/secret/password", map[string]string{"base_hash": hash3})
	if delRec.Code != http.StatusOK {
		t.Fatalf("deleteUpdaterSecret failed: %d %s", delRec.Code, delRec.Body.String())
	}
	if _, err := api.ResolveUpdaterSecret(db, "db:password"); err == nil {
		t.Fatal("expected db:password secret deleted")
	}
}
