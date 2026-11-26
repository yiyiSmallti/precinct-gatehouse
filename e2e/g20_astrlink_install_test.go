package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestG20AstrlinkInstall(t *testing.T) {
	rep := newReport(t, "G20-astrlink-install", "G20 AstrLink NER 一键安装与配置",
		"验证管理台 /admin/api/redaction/astrlink 状态查询、一键下载插件/ORT/模型并自动写入 config.yaml 的完整端到端流程。")

	// Build a minimal mock ORT tar.gz matching current OS/arch.
	var ortLibName, ortArchive string
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		ortLibName = "libonnxruntime.1.27.0.dylib"
		ortArchive = "onnxruntime-osx-arm64-1.27.0.tgz"
	case "linux/arm64":
		ortLibName = "libonnxruntime.so.1.27.0"
		ortArchive = "onnxruntime-linux-aarch64-1.27.0.tgz"
	default:
		ortLibName = "libonnxruntime.so.1.27.0"
		ortArchive = "onnxruntime-linux-x64-1.27.0.tgz"
	}

	var ortTgz bytes.Buffer
	gz := gzip.NewWriter(&ortTgz)
	tw := tar.NewWriter(gz)
	ortPayload := []byte("mock-onnxruntime-shared-library")
	_ = tw.WriteHeader(&tar.Header{
		Name: "onnxruntime/lib/" + ortLibName,
		Mode: 0o755,
		Size: int64(len(ortPayload)),
	})
	_, _ = tw.Write(ortPayload)
	_ = tw.Close()
	_ = gz.Close()

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/gh/"):
			_, _ = w.Write([]byte("#!/bin/sh\necho mock-astrlink\n"))
		case r.URL.Path == "/ort/v1.27.0/"+ortArchive:
			_, _ = w.Write(ortTgz.Bytes())
		case strings.HasPrefix(r.URL.Path, "/hf/"):
			_, _ = w.Write([]byte("mock-model-file-content"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockSrv.Close()

	fake := startFake(t, "ds")
	adminAddr := freePort(t)
	adminBase := "http://" + adminAddr
	dir := t.TempDir()

	cfg := fmt.Sprintf(`
listen: {{LISTEN}}
mode: personal
admin:
  listen: %s
  session_ttl: 1h
providers:
  - name: ds
    base_url: %s
    target: local
    models: [deepseek-chat]
default_group: main
routing_groups:
  main:
    members: [ds/deepseek-chat]
redaction:
  mode_by_target:
    local: off
    external: mask
`, adminAddr, fake.URL)

	startGatewayLogFile(t, dir, cfg, adminBase,
		"GATEHOUSE_ASTRLINK_GH_BASE="+mockSrv.URL+"/gh",
		"GATEHOUSE_ASTRLINK_ORT_BASE="+mockSrv.URL+"/ort",
		"GATEHOUSE_ASTRLINK_SKIP_SHA256=1",
	)

	cmd := exec.Command(gatehouse, "admin", "set-password", "--config", filepath.Join(dir, "config.yaml"))
	cmd.Stdin = strings.NewReader("s3cr3t-pass!\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("admin set-password: %v\n%s", err, out)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	doAdm := func(method, path string, body any) (int, map[string]any) {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, adminBase+path, rdr)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if method != http.MethodGet {
			req.Header.Set("X-Gatehouse-Admin", "1")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	code, _ := doAdm(http.MethodPost, "/admin/api/login", map[string]string{"username": "admin", "password": "s3cr3t-pass!"})
	if code != http.StatusOK {
		t.Fatalf("login = %d", code)
	}

	code, st := doAdm(http.MethodGet, "/admin/api/redaction/astrlink", nil)
	rep.Check("初始状态未安装且未配置", code == 200 && st["binary_ready"] == false && st["configured"] == false, "status=%v", st)

	code, _ = doAdm(http.MethodPost, "/admin/api/redaction/astrlink/install", map[string]any{
		"max_action": "mask",
		"hf_mirror":  mockSrv.URL + "/hf",
	})
	if code != http.StatusAccepted {
		t.Fatalf("install POST = %d, want 202", code)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		code, st = doAdm(http.MethodGet, "/admin/api/redaction/astrlink", nil)
		if code == 200 && st["installing"] == false {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	rep.Check("一键安装与配置完成",
		st["installing"] == false &&
			st["error"] == "" &&
			st["binary_ready"] == true &&
			st["ort_ready"] == true &&
			st["model_ready"] == true &&
			st["configured"] == true &&
			st["running"] == true &&
			st["restart_required"] == false,
		"step=%v err=%v", st["step"], st["error"])

	cfgBytes, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfgText := string(cfgBytes)
	rep.Check("config.yaml 已写入 plugins 与 redaction.sources.astrlink",
		strings.Contains(cfgText, "name: astrlink") &&
			strings.Contains(cfgText, "detectors:") &&
			strings.Contains(cfgText, "max_action: mask"),
		"config.yaml updated")
}
