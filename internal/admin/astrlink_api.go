package admin

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/updater"
)

const (
	astrlinkPluginName = "astrlink"
	astrlinkORTVersion = "1.27.0"
	astrlinkModelRepo  = "QuantumNous/astrlink-guard"
	astrlinkModelRev   = "7216b6a3b897ef903d4b1ba367ae6fb95b0fa5e4"
	astrlinkGHRepo     = "yiyiSmallti/precinct-gatehouse"
	astrlinkDefaultHF  = "https://hf-mirror.com"
)

type astrlinkPlatformSpec struct {
	BinName      string
	ReleaseAsset string
	ORTArchive   string
	ORTWheelPath string
	ORTLibName   string
}

var astrlinkPlatforms = map[string]astrlinkPlatformSpec{
	"linux/amd64": {
		BinName:      "astrlink-detector",
		ReleaseAsset: "astrlink-detector-linux-amd64",
		ORTArchive:   "onnxruntime-linux-x64-" + astrlinkORTVersion + ".tgz",
		ORTWheelPath: "/packages/26/81/24dd9b31b0fb912ee19ca53ac1c9764bfd79d58a2ccef564eb693be831a5/onnxruntime-1.27.0-cp312-cp312-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl",
		ORTLibName:   "libonnxruntime.so." + astrlinkORTVersion,
	},
	"linux/arm64": {
		BinName:      "astrlink-detector",
		ReleaseAsset: "astrlink-detector-linux-arm64",
		ORTArchive:   "onnxruntime-linux-aarch64-" + astrlinkORTVersion + ".tgz",
		ORTWheelPath: "/packages/84/86/c3b6b17745a1997d784dadc9bd88d713d2e6721139a5a0e885b28cfb79b1/onnxruntime-1.27.0-cp312-cp312-manylinux_2_27_aarch64.manylinux_2_28_aarch64.whl",
		ORTLibName:   "libonnxruntime.so." + astrlinkORTVersion,
	},
	"darwin/arm64": {
		BinName:      "astrlink-detector",
		ReleaseAsset: "astrlink-detector-darwin-arm64",
		ORTArchive:   "onnxruntime-osx-arm64-" + astrlinkORTVersion + ".tgz",
		ORTWheelPath: "/packages/c3/b7/dd3a524ed93a820dff1af902d0412957ab12499953333e9daa01af5bc480/onnxruntime-1.27.0-cp312-cp312-macosx_14_0_arm64.whl",
		ORTLibName:   "libonnxruntime." + astrlinkORTVersion + ".dylib",
	},
	"windows/amd64": {
		BinName:      "astrlink-detector.exe",
		ReleaseAsset: "astrlink-detector-windows-amd64.exe",
		ORTArchive:   "onnxruntime-win-x64-" + astrlinkORTVersion + ".zip",
		ORTWheelPath: "/packages/4f/88/8ec9db1a4d126bb8b758992beb40d1249df171917d75f44a327eb5f20dda/onnxruntime-1.27.0-cp312-cp312-win_amd64.whl",
		ORTLibName:   "onnxruntime.dll",
	},
}

var astrlinkModelFiles = []struct {
	Name   string
	SHA256 string
}{
	{Name: "model_int8.onnx", SHA256: "0a070d3113615e33ba3b84d6bd486f663a377c04c4b056e7381732179f51832a"},
	{Name: "model_int8.onnx_data", SHA256: "0ba34216ceead01e0ae2dfd8e46d52b67e7aee80b04f722d9b758613a91c0067"},
	{Name: "tokenizer.json", SHA256: "0cf3b235ef125658015a6c8026a5fe6f28088bf825101a990ec18a10200feef4"},
	{Name: "config.json", SHA256: "2c20003590d642e273c03c4c7c82d9849fa75f64bf29a71ec0a0c9a20ba4a150"},
}

type astrlinkState struct {
	mu         sync.Mutex
	installing bool
	step       string
	message    string
	err        string
}

func (a *astrlinkState) set(installing bool, step, msg, errStr string) {
	a.mu.Lock()
	a.installing = installing
	a.step = step
	a.message = msg
	a.err = errStr
	a.mu.Unlock()
}

func (a *astrlinkState) snapshot() (installing bool, step, msg, errStr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.installing, a.step, a.message, a.err
}

type astrlinkPaths struct {
	BinPath  string
	ORTPath  string
	ModelDir string
}

func resolveAstrlinkPaths(cfg *config.Config, spec astrlinkPlatformSpec) astrlinkPaths {
	base := filepath.Join(cfg.DataDir, "plugins", "astrlink")
	p := astrlinkPaths{
		BinPath:  filepath.Join(base, "bin", spec.BinName),
		ORTPath:  filepath.Join(base, "lib", spec.ORTLibName),
		ModelDir: filepath.Join(base, "models", "astrlink-int8"),
	}
	for _, pl := range cfg.Plugins {
		if pl.Name != astrlinkPluginName {
			continue
		}
		if pl.Path != "" {
			p.BinPath = config.ExpandHome(pl.Path)
		}
		for i := 0; i+1 < len(pl.Args); i++ {
			switch pl.Args[i] {
			case "-model":
				p.ModelDir = config.ExpandHome(pl.Args[i+1])
			case "-ort":
				p.ORTPath = config.ExpandHome(pl.Args[i+1])
			}
		}
		break
	}
	return p
}

func fileReady(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && !st.IsDir() && st.Size() > 0
}

func modelDirReady(dir string) bool {
	if dir == "" {
		return false
	}
	for _, mf := range astrlinkModelFiles {
		if !fileReady(filepath.Join(dir, mf.Name)) {
			return false
		}
	}
	return true
}

func (s *Server) astrlinkStatusMap() map[string]any {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	spec, supported := astrlinkPlatforms[platform]
	if !supported {
		spec = astrlinkPlatforms["linux/amd64"]
	}

	st := s.deps.Live.Status()
	cfg := st.Snapshot.Cfg
	paths := resolveAstrlinkPaths(cfg, spec)

	binReady := fileReady(paths.BinPath)
	ortReady := fileReady(paths.ORTPath)
	modReady := modelDirReady(paths.ModelDir)

	hasPlugin := false
	for _, pl := range cfg.Plugins {
		if pl.Name == astrlinkPluginName && pl.Kind == "detector" && (pl.Enabled == nil || *pl.Enabled) {
			hasPlugin = true
			break
		}
	}
	hasDetector := slices.Contains(cfg.Redaction.Detectors, astrlinkPluginName)
	configured := hasPlugin && hasDetector

	var missing []string
	if st.Snapshot.Redactor != nil {
		missing = st.Snapshot.Redactor.Missing()
	} else {
		missing = s.deps.MissingDetectors
	}
	pluginsNeedRestart := slices.Contains(st.RestartRequired, "plugins")
	running := configured && !pluginsNeedRestart && !slices.Contains(missing, astrlinkPluginName)

	maxAction := cfg.Redaction.Sources[astrlinkPluginName].MaxAction
	if maxAction == "" {
		maxAction = "audit"
	}

	installing, step, msg, errStr := s.astrlink.snapshot()
	restartRequired := pluginsNeedRestart || (configured && !running && binReady && ortReady && modReady)

	return map[string]any{
		"platform":         platform,
		"supported":        supported,
		"binary_path":      paths.BinPath,
		"binary_ready":     binReady,
		"ort_path":         paths.ORTPath,
		"ort_ready":        ortReady,
		"model_dir":        paths.ModelDir,
		"model_ready":      modReady,
		"configured":       configured,
		"running":          running,
		"restart_required": restartRequired,
		"max_action":       maxAction,
		"installing":       installing,
		"step":             step,
		"message":          msg,
		"error":            errStr,
	}
}

func (s *Server) getAstrlinkStatus(w http.ResponseWriter, _ *http.Request, _ string) {
	writeJSON(w, http.StatusOK, s.astrlinkStatusMap())
}

type astrlinkInstallReq struct {
	MaxAction string `json:"max_action"`
	HFMirror  string `json:"hf_mirror"`
	GHMirror  string `json:"gh_mirror"`
	Force     bool   `json:"force"`
}

func (s *Server) installAstrlink(w http.ResponseWriter, r *http.Request, _ string) {
	var req astrlinkInstallReq
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
	}
	req.MaxAction = strings.TrimSpace(req.MaxAction)
	if req.MaxAction == "" {
		req.MaxAction = "audit"
	}
	if req.MaxAction != "audit" && req.MaxAction != "mask" && req.MaxAction != "warn" && req.MaxAction != "block" {
		writeJSONError(w, http.StatusBadRequest, "max_action must be audit, warn, mask or block")
		return
	}
	req.HFMirror = strings.TrimRight(strings.TrimSpace(req.HFMirror), "/")
	if req.HFMirror == "" {
		req.HFMirror = astrlinkDefaultHF
	}
	req.GHMirror = strings.TrimSpace(req.GHMirror)

	platform := runtime.GOOS + "/" + runtime.GOARCH
	spec, supported := astrlinkPlatforms[platform]
	if !supported {
		writeJSONError(w, http.StatusBadRequest, "unsupported platform for one-click install: "+platform)
		return
	}

	s.astrlink.mu.Lock()
	if s.astrlink.installing {
		s.astrlink.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "install_in_progress",
			"status": s.astrlinkStatusMap(),
		})
		return
	}
	s.astrlink.installing = true
	s.astrlink.step = "prepare"
	s.astrlink.message = "正在准备安装目录..."
	s.astrlink.err = ""
	s.astrlink.mu.Unlock()

	go s.runAstrlinkInstall(spec, req)
	writeJSON(w, http.StatusAccepted, s.astrlinkStatusMap())
}

func (s *Server) runAstrlinkInstall(spec astrlinkPlatformSpec, req astrlinkInstallReq) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	st := s.deps.Live.Status()
	paths := resolveAstrlinkPaths(st.Snapshot.Cfg, spec)
	fallbackDNS := []string{"223.5.5.5", "119.29.29.29"}
	if st.Snapshot.Cfg.Updater.FallbackDNS != nil && len(*st.Snapshot.Cfg.Updater.FallbackDNS) > 0 {
		fallbackDNS = *st.Snapshot.Cfg.Updater.FallbackDNS
	}
	httpClient := updater.NewHTTPClient(5*time.Minute, fallbackDNS)

	fail := func(step, msg string, err error) {
		s.astrlink.set(false, step, msg, err.Error())
	}

	for _, dir := range []string{filepath.Dir(paths.BinPath), filepath.Dir(paths.ORTPath), paths.ModelDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail("prepare", "创建目录失败", err)
			return
		}
	}

	// 1. Install astrlink-detector binary
	if req.Force || !fileReady(paths.BinPath) {
		s.astrlink.set(true, "binary", "正在安装 astrlink-detector 插件程序...", "")
		if err := ensureAstrlinkBinary(ctx, httpClient, paths.BinPath, spec, s.deps.Version, req.GHMirror); err != nil {
			fail("binary", "插件程序安装失败", err)
			return
		}
	}

	// 2. Install ONNX Runtime shared library
	if req.Force || !fileReady(paths.ORTPath) {
		s.astrlink.set(true, "ort", fmt.Sprintf("正在下载 ONNX Runtime %s...", astrlinkORTVersion), "")
		if err := ensureAstrlinkORT(ctx, httpClient, paths.ORTPath, spec, req.GHMirror); err != nil {
			fail("ort", "ONNX Runtime 下载安装失败", err)
			return
		}
	}

	// 3. Install AstrLink Guard int8 model files
	skipSHA := os.Getenv("GATEHOUSE_ASTRLINK_SKIP_SHA256") == "1"
	if req.Force || !modelDirReady(paths.ModelDir) {
		for idx, mf := range astrlinkModelFiles {
			dst := filepath.Join(paths.ModelDir, mf.Name)
			wantSHA := mf.SHA256
			if skipSHA {
				wantSHA = ""
			}
			if !req.Force && wantSHA != "" && verifyFileSHA256(dst, wantSHA) == nil {
				continue
			}
			s.astrlink.set(true, "model", fmt.Sprintf("正在下载模型文件 (%d/%d): %s...", idx+1, len(astrlinkModelFiles), mf.Name), "")
			url := fmt.Sprintf("%s/%s/resolve/%s/int8/%s", req.HFMirror, astrlinkModelRepo, astrlinkModelRev, mf.Name)
			if err := downloadFileVerified(ctx, httpClient, url, dst, wantSHA, 0o644); err != nil {
				fail("model", "模型文件下载或校验失败: "+mf.Name, err)
				return
			}
		}
	}

	// 4. Write config.yaml atomically via Live.Apply
	s.astrlink.set(true, "config", "正在写入网关配置...", "")
	if err := s.applyAstrlinkConfig(paths, req.MaxAction); err != nil {
		fail("config", "写入配置文件失败", err)
		return
	}

	s.astrlink.set(false, "done", "安装与配置已完成，重启网关进程后即可生效", "")
}

func (s *Server) applyAstrlinkConfig(paths astrlinkPaths, maxAction string) error {
	st := s.deps.Live.Status()
	raw := st.Snapshot.Cfg.RawCopy()

	enabled := true
	pluginEntry := config.Plugin{
		Name:    astrlinkPluginName,
		Kind:    "detector",
		Path:    paths.BinPath,
		Args:    []string{"-model", paths.ModelDir, "-ort", paths.ORTPath},
		Enabled: &enabled,
	}
	foundPlugin := false
	for i := range raw.Plugins {
		if raw.Plugins[i].Name == astrlinkPluginName {
			raw.Plugins[i] = pluginEntry
			foundPlugin = true
			break
		}
	}
	if !foundPlugin {
		raw.Plugins = append(raw.Plugins, pluginEntry)
	}

	if !slices.Contains(raw.Redaction.Detectors, astrlinkPluginName) {
		raw.Redaction.Detectors = append(raw.Redaction.Detectors, astrlinkPluginName)
	}
	if raw.Redaction.Sources == nil {
		raw.Redaction.Sources = map[string]config.RedactionSource{}
	}
	src := raw.Redaction.Sources[astrlinkPluginName]
	if len(src.MinConfidence) == 0 {
		src.MinConfidence = map[string]float64{
			"default": 0.8,
			"person":  0.9,
			"date":    0.9,
			"account": 0.7,
		}
	}
	src.MaxAction = maxAction
	if src.Timeout == 0 {
		src.Timeout = config.Duration(10 * time.Second)
	}
	if src.OnError == "" {
		src.OnError = "rules_only"
	}
	raw.Redaction.Sources[astrlinkPluginName] = src

	yamlBytes, err := config.MarshalRaw(raw)
	if err != nil {
		return err
	}
	_, err = s.deps.Live.Apply(yamlBytes, "web:astrlink-install", st.Hash)
	return err
}

var defaultGHMirrors = []string{
	"https://gh-proxy.com/",
	"https://ghfast.top/",
}

func applyGHMirror(rawURL, ghMirror string) string {
	if ghMirror == "" {
		return rawURL
	}
	if strings.HasSuffix(ghMirror, "/") {
		return ghMirror + rawURL
	}
	return ghMirror + "/" + rawURL
}

func ghMirrorCandidates(ghMirror string) []string {
	var out []string
	seen := make(map[string]struct{})
	add := func(m string) {
		m = strings.TrimSpace(m)
		key := strings.TrimRight(m, "/")
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, m)
	}

	if strings.TrimSpace(ghMirror) != "" {
		add(ghMirror)
	}
	add("") // direct GitHub first when ghMirror is empty, or fallback when specified mirror fails
	for _, m := range defaultGHMirrors {
		add(m)
	}
	return out
}

var defaultPyPIBases = []string{
	"https://pypi.tuna.tsinghua.edu.cn",
	"https://mirrors.aliyun.com/pypi",
	"https://files.pythonhosted.org",
}

type httpStatusError struct {
	code int
	url  string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d from %s", e.code, e.url)
}

func downloadGitHubFile(ctx context.Context, client *http.Client, rawURL, ghMirror, dst, wantSHA string, mode os.FileMode) error {
	if !strings.HasPrefix(rawURL, "https://github.com/") {
		return downloadFileVerified(ctx, client, applyGHMirror(rawURL, ghMirror), dst, wantSHA, mode)
	}
	var lastErr error
	for _, m := range ghMirrorCandidates(ghMirror) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := downloadFileVerified(ctx, client, applyGHMirror(rawURL, m), dst, wantSHA, mode)
		if err == nil {
			return nil
		}
		lastErr = err
		var se *httpStatusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			// Upstream GitHub returned 404; skip remaining mirrors to avoid unnecessary timeouts.
			return err
		}
	}
	return lastErr
}

func ensureAstrlinkBinary(ctx context.Context, client *http.Client, dst string, spec astrlinkPlatformSpec, version, ghMirror string) error {
	ghBase := strings.TrimRight(os.Getenv("GATEHOUSE_ASTRLINK_GH_BASE"), "/")
	if ghBase == "" {
		// 1. Copy if placed beside the gatehouse executable.
		if exe, err := os.Executable(); err == nil {
			exeDir := filepath.Dir(exe)
			for _, cand := range []string{spec.BinName, spec.ReleaseAsset} {
				p := filepath.Join(exeDir, cand)
				if fileReady(p) {
					return copyFileAtomic(p, dst, 0o755)
				}
			}
		}

		// 2. Build from local source checkout if available and Go toolchain exists.
		if srcDir := findLocalAstrlinkSource(); srcDir != "" {
			if _, err := exec.LookPath("go"); err == nil {
				tmp := dst + ".tmp"
				cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", "-s -w", "-o", tmp, ".")
				cmd.Dir = srcDir
				cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
				if out, err := cmd.CombinedOutput(); err == nil {
					if err := os.Chmod(tmp, 0o755); err != nil {
						_ = os.Remove(tmp)
						return err
					}
					return os.Rename(tmp, dst)
				} else {
					_ = os.Remove(tmp)
					_ = out
				}
			}
		}
		ghBase = "https://github.com/" + astrlinkGHRepo + "/releases"
	}

	// 3. Download from GitHub Release.
	var urls []string
	ver := strings.TrimSpace(version)
	if ver != "" && ver != "dev" && !strings.HasPrefix(ver, "manual-") {
		if !strings.HasPrefix(ver, "v") {
			ver = "v" + ver
		}
		urls = append(urls, fmt.Sprintf("%s/download/%s/%s", ghBase, ver, spec.ReleaseAsset))
	}
	urls = append(urls, fmt.Sprintf("%s/latest/download/%s", ghBase, spec.ReleaseAsset))

	var lastErr error
	for _, u := range urls {
		if err := downloadGitHubFile(ctx, client, u, ghMirror, dst, "", 0o755); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("无法获取 %s（请确认 Release 已包含该资产或手动放置到 %s）: %w", spec.ReleaseAsset, dst, lastErr)
}

func findLocalAstrlinkSource() string {
	var bases []string
	if wd, err := os.Getwd(); err == nil {
		bases = append(bases, wd)
	}
	if exe, err := os.Executable(); err == nil {
		bases = append(bases, filepath.Dir(exe))
	}
	for _, b := range bases {
		cand := filepath.Join(b, "plugins", "astrlink-detector")
		if fileReady(filepath.Join(cand, "go.mod")) {
			return cand
		}
	}
	return ""
}

func extractORTArchive(archivePath, archiveName, libName, dst string) error {
	if strings.HasSuffix(archiveName, ".zip") || strings.HasSuffix(archiveName, ".whl") {
		return extractORTFromZip(archivePath, libName, dst)
	}
	return extractORTFromTarGz(archivePath, libName, dst)
}

func ensureAstrlinkORT(ctx context.Context, client *http.Client, dst string, spec astrlinkPlatformSpec, ghMirror string) error {
	ortBase := strings.TrimRight(os.Getenv("GATEHOUSE_ASTRLINK_ORT_BASE"), "/")
	if ortBase != "" {
		rawURL := fmt.Sprintf("%s/v%s/%s", ortBase, astrlinkORTVersion, spec.ORTArchive)
		tmpArchive := dst + ".archive.tmp"
		defer os.Remove(tmpArchive)
		if err := downloadGitHubFile(ctx, client, rawURL, ghMirror, tmpArchive, "", 0o644); err != nil {
			return err
		}
		return extractORTArchive(tmpArchive, spec.ORTArchive, spec.ORTLibName, dst)
	}

	rawURL := fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/%s", astrlinkORTVersion, spec.ORTArchive)
	tmpArchive := dst + ".archive.tmp"
	defer os.Remove(tmpArchive)
	ghErr := downloadGitHubFile(ctx, client, rawURL, ghMirror, tmpArchive, "", 0o644)
	if ghErr == nil {
		return extractORTArchive(tmpArchive, spec.ORTArchive, spec.ORTLibName, dst)
	}

	if spec.ORTWheelPath == "" {
		return ghErr
	}
	tmpWheel := dst + ".whl.tmp"
	defer os.Remove(tmpWheel)
	lastErr := ghErr
	for _, base := range defaultPyPIBases {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wheelURL := strings.TrimRight(base, "/") + spec.ORTWheelPath
		if err := downloadFileVerified(ctx, client, wheelURL, tmpWheel, "", 0o644); err == nil {
			return extractORTArchive(tmpWheel, ".whl", spec.ORTLibName, dst)
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func extractORTFromTarGz(archivePath, targetBase, dst string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) == targetBase {
			return writeStreamAtomic(tr, dst, "", 0o755)
		}
	}
	return fmt.Errorf("archive does not contain %s", targetBase)
}

func extractORTFromZip(archivePath, targetBase, dst string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if filepath.Base(f.Name) == targetBase {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = writeStreamAtomic(rc, dst, "", 0o755)
			_ = rc.Close()
			return err
		}
	}
	return fmt.Errorf("archive does not contain %s", targetBase)
}

func downloadFileVerified(ctx context.Context, client *http.Client, url, dst, wantSHA string, mode os.FileMode) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "precinct-gatehouse-installer/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{code: resp.StatusCode, url: url}
	}
	return writeStreamAtomic(resp.Body, dst, wantSHA, mode)
}

func writeStreamAtomic(r io.Reader, dst, wantSHA string, mode os.FileMode) error {
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), r)
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if wantSHA != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, wantSHA) {
			_ = os.Remove(tmp)
			return fmt.Errorf("sha256 mismatch: got %s, want %s", got, wantSHA)
		}
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func copyFileAtomic(src, dst string, mode os.FileMode) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeStreamAtomic(f, dst, "", mode)
}

func verifyFileSHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}
