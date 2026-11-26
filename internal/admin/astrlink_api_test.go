package admin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/live"
)

func TestAstrlinkStatusAndApplyConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	initial := []byte("listen: 127.0.0.1:0\nmode: personal\nproviders:\n  - name: local\n    base_url: http://127.0.0.1:11434/v1\n    target: local\n    models: [m]\n")
	if err := os.WriteFile(cfgPath, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	bootCfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := live.New(bootCfg, func(next *config.Config, _ *live.Snapshot) (*live.Snapshot, error) {
		return &live.Snapshot{Cfg: next}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Deps{
		Live:    mgr,
		Version: "v0.2.10",
	})

	// 1. Initial status: nothing installed or configured
	req := httptest.NewRequest(http.MethodGet, "/admin/api/redaction/astrlink", nil)
	rec := httptest.NewRecorder()
	s.getAstrlinkStatus(rec, req, "admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["binary_ready"] != false || st["ort_ready"] != false || st["model_ready"] != false || st["configured"] != false {
		t.Fatalf("unexpected initial status: %+v", st)
	}

	// 2. Populate files and apply config
	platform := runtime.GOOS + "/" + runtime.GOARCH
	spec, ok := astrlinkPlatforms[platform]
	if !ok {
		spec = astrlinkPlatforms["linux/amd64"]
	}
	paths := resolveAstrlinkPaths(mgr.Status().Snapshot.Cfg, spec)
	for _, p := range []string{paths.BinPath, paths.ORTPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("stub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(paths.ModelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, mf := range astrlinkModelFiles {
		if err := os.WriteFile(filepath.Join(paths.ModelDir, mf.Name), []byte("stub"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.applyAstrlinkConfig(paths, "mask"); err != nil {
		t.Fatalf("applyAstrlinkConfig: %v", err)
	}

	updated := mgr.Status().Snapshot.Cfg
	if !slices.Contains(updated.Redaction.Detectors, "astrlink") {
		t.Fatalf("expected astrlink in redaction.detectors, got %v", updated.Redaction.Detectors)
	}
	if got := updated.Redaction.Sources["astrlink"].MaxAction; got != "mask" {
		t.Fatalf("max_action = %q, want mask", got)
	}

	// 3. Status now reports ready + configured + running (hot-reloaded)
	rec2 := httptest.NewRecorder()
	s.getAstrlinkStatus(rec2, req, "admin")
	var st2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &st2); err != nil {
		t.Fatal(err)
	}
	if st2["binary_ready"] != true || st2["ort_ready"] != true || st2["model_ready"] != true || st2["configured"] != true || st2["running"] != true || st2["restart_required"] != false {
		t.Fatalf("unexpected configured status: %+v", st2)
	}
}

func TestDownloadAndExtractHelpers(t *testing.T) {
	payload := []byte("fake-ort-shared-lib")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "onnxruntime-linux-x64-1.27.0/lib/libonnxruntime.so.1.27.0",
		Mode: 0o755,
		Size: int64(len(payload)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()

	sum := sha256.Sum256(buf.Bytes())
	wantSHA := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dir := t.TempDir()
	tgzPath := filepath.Join(dir, "ort.tgz")
	if err := downloadFileVerified(context.Background(), srv.Client(), srv.URL, tgzPath, wantSHA, 0o644); err != nil {
		t.Fatalf("downloadFileVerified: %v", err)
	}
	libPath := filepath.Join(dir, "libonnxruntime.so.1.27.0")
	if err := extractORTFromTarGz(tgzPath, "libonnxruntime.so.1.27.0", libPath); err != nil {
		t.Fatalf("extractORTFromTarGz: %v", err)
	}
	got, err := os.ReadFile(libPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("extracted = %q, want %q", got, payload)
	}
}

func TestGHMirrorCandidatesAndFallback(t *testing.T) {
	gotDefault := ghMirrorCandidates("")
	wantDefault := []string{"", "https://gh-proxy.com/", "https://ghfast.top/"}
	if !slices.Equal(gotDefault, wantDefault) {
		t.Fatalf("ghMirrorCandidates(\"\") = %v, want %v", gotDefault, wantDefault)
	}

	gotCustom := ghMirrorCandidates("https://gh-proxy.com")
	wantCustom := []string{"https://gh-proxy.com", "", "https://ghfast.top/"}
	if !slices.Equal(gotCustom, wantCustom) {
		t.Fatalf("ghMirrorCandidates(custom) = %v, want %v", gotCustom, wantCustom)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "bin")
	if err := downloadGitHubFile(context.Background(), srv.Client(), srv.URL+"/asset", "", dst, "", 0o755); err != nil {
		t.Fatalf("downloadGitHubFile(non-github): %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ok" {
		t.Fatalf("downloaded = %q, want ok", data)
	}
}

func TestPlatformSpecsAndWheelExtraction(t *testing.T) {
	for plat, spec := range astrlinkPlatforms {
		if spec.ORTWheelPath == "" {
			t.Fatalf("platform %s missing ORTWheelPath", plat)
		}
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("onnxruntime/capi/libonnxruntime.so.1.27.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("wheel-ort-so")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	whlPath := filepath.Join(dir, "ort.whl")
	if err := os.WriteFile(whlPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "libonnxruntime.so.1.27.0")
	if err := extractORTArchive(whlPath, ".whl", "libonnxruntime.so.1.27.0", dst); err != nil {
		t.Fatalf("extractORTArchive(whl): %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "wheel-ort-so" {
		t.Fatalf("extracted = %q, want wheel-ort-so", got)
	}
}
