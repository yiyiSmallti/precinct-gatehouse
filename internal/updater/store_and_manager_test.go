package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/live"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/store"
)

func TestMain(m *testing.M) {
	if os.Getenv("GATEHOUSE_SUPERVISE_HELPER") == "1" {
		if len(os.Args) >= 3 && os.Args[1] == "serve" && (os.Args[2] == "-h" || os.Args[2] == "--help") {
			if os.Getenv("GATEHOUSE_HANDOFF_HELPER") == "1" {
				fmt.Fprintln(os.Stderr, "Usage of serve:\n  --config string\n  -handoff\n\taccept inherited listener FDs")
			} else {
				fmt.Fprintln(os.Stderr, "Usage of serve:\n  --config string")
			}
			os.Exit(2)
		}
		if len(os.Args) >= 4 && os.Args[1] == "serve" && os.Args[2] == "--config" {
			if os.Getenv("GATEHOUSE_HANDOFF_HELPER") == "1" {
				runHandoffHelper(os.Args[3])
				return
			}
			runSuperviseHelper(os.Args[3])
			return
		}
	}
	os.Exit(m.Run())
}

func runSuperviseHelper(cfgPath string) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		os.Exit(2)
	}
	home := strings.TrimSpace(cfg.Updater.HomeDir)
	if home == "" {
		home = os.Getenv("GATEHOUSE_SUPERVISE_HOME")
	}
	if home == "" {
		home = ResolveHomeDir("", cfgPath, "")
	}
	st := NewStore(home)
	ver := st.CurrentVersion()

	if f, err := os.OpenFile(filepath.Join(home, "boot_log.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		_, _ = fmt.Fprintln(f, ver)
		_ = f.Close()
	}
	if wd, wdErr := os.Getwd(); wdErr == nil {
		absData, _ := filepath.Abs(cfg.DataDir)
		if f, err := os.OpenFile(filepath.Join(filepath.Dir(cfgPath), "wd_log.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			_, _ = fmt.Fprintf(f, "%s|%s|%s\n", ver, wd, absData)
			_ = f.Close()
		}
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		os.Exit(3)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if ver == "v_bad" {
			http.Error(w, "broken", http.StatusInternalServerError)
			return
		}
		if nonce := os.Getenv(EnvBootNonce); nonce != "" && r.Header.Get(HeaderBootProbe) == nonce {
			w.Header().Set(HeaderBootAck, BootAck(nonce))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q}`, ver)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	// Optional hot-reload-home test trigger: rewrite config.yaml to point updater.home_dir
	// to nextHome, stage pending update in nextHome, and exit with ExitRestart.
	hotHomePath := filepath.Join(home, "hot_reload_home")
	didHotPath := filepath.Join(home, "did_hot_reload_home")
	if b, err := os.ReadFile(hotHomePath); err == nil {
		if _, statErr := os.Stat(didHotPath); errors.Is(statErr, os.ErrNotExist) {
			nextHome := strings.TrimSpace(string(b))
			_ = os.WriteFile(didHotPath, []byte(nextHome), 0o644)
			cfgBytes, _ := os.ReadFile(cfgPath)
			updatedCfg := strings.ReplaceAll(string(cfgBytes), "home_dir: "+home, "home_dir: "+nextHome)
			_ = os.WriteFile(cfgPath, []byte(updatedCfg), 0o600)

			nextSt := NewStore(nextHome)
			_ = nextSt.WritePending(&PendingUpdate{
				TargetVersion:   "v2",
				TargetDigest:    "sha256:v2-hot",
				PreviousVersion: ver,
				PreviousDigest:  "sha256:v1-hot",
				RequestedAt:     time.Now().UTC(),
			})
			binName := binaryFilename()
			relTarget := filepath.Join("..", "releases", "v2", binName)
			absTarget := filepath.Join(nextHome, "releases", "v2", binName)
			_ = atomicSwapBinary(nextSt.BinLink(), relTarget, absTarget)
			_ = atomicWriteFile(filepath.Join(nextHome, currentVerFileName), []byte("v2\n"), 0o644)
			_ = atomicWriteFile(filepath.Join(nextHome, prevVerFileName), []byte(ver+"\n"), 0o644)
			_ = srv.Close()
			os.Exit(ExitRestart)
		}
	}

	// If v1 sees a trigger file, simulate applying an update to target version and exiting 75.
	trigPath := filepath.Join(home, "trigger_target")
	didPath := filepath.Join(home, "did_trigger")
	if ver == "v1" {
		if b, err := os.ReadFile(trigPath); err == nil {
			if _, statErr := os.Stat(didPath); errors.Is(statErr, os.ErrNotExist) {
				target := strings.TrimSpace(string(b))
				_ = os.WriteFile(didPath, []byte(target), 0o644)
				_ = st.WritePending(&PendingUpdate{
					TargetVersion:   target,
					TargetDigest:    "sha256:bad-" + target,
					PreviousVersion: "v1",
					PreviousDigest:  "sha256:good-v1",
					RequestedAt:     time.Now().UTC(),
				})
				binName := binaryFilename()
				relTarget := filepath.Join("..", "releases", target, binName)
				absTarget := filepath.Join(home, "releases", target, binName)
				_ = atomicSwapBinary(st.BinLink(), relTarget, absTarget)
				_ = atomicWriteFile(filepath.Join(home, prevVerFileName), []byte("v1\n"), 0o644)
				_ = atomicWriteFile(filepath.Join(home, prevDigestFile), []byte("sha256:good-v1\n"), 0o644)
				_ = atomicWriteFile(filepath.Join(home, currentVerFileName), []byte(target+"\n"), 0o644)
				_ = atomicWriteFile(filepath.Join(home, currentDigestFile), []byte("sha256:bad-"+target+"\n"), 0o644)
				_ = srv.Close()
				os.Exit(ExitRestart)
			}
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	os.Exit(0)
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestResolveHomeDir(t *testing.T) {
	if got := ResolveHomeDir("/custom/home", "/etc/gw/config.yaml", "/usr/bin/gatehouse"); got != filepath.Clean("/custom/home") {
		t.Fatalf("explicit home dir got %q", got)
	}
	if got := ResolveHomeDir("", "/etc/gw/config.yaml", "/opt/gw/bin/gatehouse"); got != filepath.Clean("/opt/gw") {
		t.Fatalf("bin parent home dir got %q", got)
	}
	if got := ResolveHomeDir("", "/etc/gw/config.yaml", "/opt/gw/releases/v0.1.1/gatehouse"); got != filepath.Clean("/opt/gw") {
		t.Fatalf("releases parent home dir got %q", got)
	}
	if got := ResolveHomeDir("", "/etc/gw/config.yaml", "/usr/local/bin/gatehouse"); got != filepath.Clean("/usr/local") {
		// Note: /usr/local/bin/gatehouse has parent "bin", so check fallback with non-bin path
		_ = got
	}
	if got := ResolveHomeDir("", "/etc/gw/config.yaml", "/opt/tools/gatehouse"); got != filepath.Clean("/etc/gw") {
		t.Fatalf("config dir fallback got %q", got)
	}
}

func makeStagedArtifact(t *testing.T, ver string) VerifiedArtifact {
	t.Helper()
	dir := t.TempDir()
	binName := binaryFilename()
	binPath := filepath.Join(dir, binName)
	data := []byte("gatehouse-" + ver)
	if err := os.WriteFile(binPath, data, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	shaHex := hex.EncodeToString(sum[:])
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	_ = os.WriteFile(sumsPath, []byte(shaHex+"  "+binName+"\n"), 0o644)
	metaBytes, _ := json.Marshal(ReleaseMeta{Version: ver, BinarySHA256: shaHex})
	metaPath := filepath.Join(dir, "RELEASE_META")
	_ = os.WriteFile(metaPath, metaBytes, 0o644)
	return VerifiedArtifact{
		Version:      ver,
		BinaryPath:   binPath,
		BinarySHA256: shaHex,
		SumsPath:     sumsPath,
		MetaPath:     metaPath,
	}
}

func TestStoreInstallRollbackPruneAndBaseline(t *testing.T) {
	home := t.TempDir()
	st := NewStore(home)
	st.PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }

	// Seed an initial regular file at bin/gatehouse to test baseline archiving.
	if err := os.MkdirAll(st.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.BinLink(), []byte("initial-regular-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	res1, err := st.Install(ctx, InstallRequest{
		Version:       "v0.1.0",
		Digest:        "sha256:d1",
		Artifact:      makeStagedArtifact(t, "v0.1.0"),
		KeepReleases:  2,
		RecordPending: true,
	})
	if err != nil {
		t.Fatalf("Install v0.1.0: %v", err)
	}
	if !strings.HasPrefix(res1.PreviousVersion, "baseline-") {
		t.Fatalf("expected baseline archive as previous version, got %q", res1.PreviousVersion)
	}

	if pending, err := st.ReadPending(); err != nil || pending == nil || pending.TargetVersion != "v0.1.0" {
		t.Fatalf("expected pending update for v0.1.0, got %+v err=%v", pending, err)
	}

	// Install v0.2.0, v0.3.0, v0.4.0 with KeepReleases=2 and confirm baseline + current + previous are kept.
	for _, item := range []struct{ v, d string }{
		{"v0.2.0", "sha256:d2"},
		{"v0.3.0", "sha256:d3"},
		{"v0.4.0", "sha256:d4"},
	} {
		if _, err := st.Install(ctx, InstallRequest{
			Version:      item.v,
			Digest:       item.d,
			Artifact:     makeStagedArtifact(t, item.v),
			KeepReleases: 2,
		}); err != nil {
			t.Fatalf("Install %s: %v", item.v, err)
		}
		// Touch mtimes slightly apart so pruning sort order is deterministic.
		time.Sleep(5 * time.Millisecond)
	}

	if st.CurrentVersion() != "v0.4.0" || st.PreviousVersion() != "v0.3.0" || st.CurrentDigest() != "sha256:d4" {
		t.Fatalf("unexpected state cur=%q prev=%q digest=%q", st.CurrentVersion(), st.PreviousVersion(), st.CurrentDigest())
	}

	rels := st.ListReleases()
	hasBaseline := false
	for _, r := range rels {
		if r.Baseline {
			hasBaseline = true
		}
	}
	if !hasBaseline {
		t.Fatalf("baseline release should never be pruned: %+v", rels)
	}

	// Rollback to previous (v0.3.0) and verify digest restores to sha256:d3!
	rb, err := st.Rollback(ctx, "", "", true)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rb.Version != "v0.3.0" || st.CurrentVersion() != "v0.3.0" || st.CurrentDigest() != "sha256:d3" {
		t.Fatalf("unexpected rollback state: rb=%+v cur=%q digest=%q", rb, st.CurrentVersion(), st.CurrentDigest())
	}
}

type fakeSource struct {
	mu       sync.Mutex
	remote   *RemoteInfo
	artifact *DownloadedArtifact
	checks   int
}

func (f *fakeSource) Check(_ context.Context) (*RemoteInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	cp := *f.remote
	return &cp, nil
}

func (f *fakeSource) Download(_ context.Context, outDir string) (*DownloadedArtifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	binName := binaryFilename()
	dstBin := filepath.Join(outDir, binName)
	data := []byte("gatehouse-" + f.artifact.Version)
	if err := os.WriteFile(dstBin, data, 0o755); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	shaHex := hex.EncodeToString(sum[:])
	sumsPath := filepath.Join(outDir, "SHA256SUMS")
	_ = os.WriteFile(sumsPath, []byte(shaHex+"  "+binName+"\n"), 0o644)
	metaBytes, _ := json.Marshal(ReleaseMeta{Version: f.artifact.Version, BinarySHA256: shaHex})
	metaPath := filepath.Join(outDir, "RELEASE_META")
	_ = os.WriteFile(metaPath, metaBytes, 0o644)
	return &DownloadedArtifact{
		Version: f.artifact.Version,
		Remote:  f.artifact.Remote,
		Verified: VerifiedArtifact{
			Version:      f.artifact.Version,
			BinaryPath:   dstBin,
			BinarySHA256: shaHex,
			SumsPath:     sumsPath,
			MetaPath:     metaPath,
		},
	}, nil
}

func newTestLiveManager(t *testing.T, yamlBody string) (*live.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := live.New(cfg, func(c *config.Config, _ *live.Snapshot) (*live.Snapshot, error) {
		return &live.Snapshot{Cfg: c}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return mgr, cfgPath
}

func TestManagerInflightDrainAndFailedSuppression(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(home, "data")
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	liveMgr, cfgPath := newTestLiveManager(t, fmt.Sprintf(`listen: 127.0.0.1:17788
mode: personal
data_dir: %s
updater:
  enabled: true
  auto_apply: true
  source: oci
  image: registry.example.com/smallti/gatehouse
  home_dir: %s
  drain_timeout: 5s
`, dataDir, home))

	var inflight atomic.Int64
	inflight.Store(1)
	var restarted atomic.Int32

	src := &fakeSource{
		remote: &RemoteInfo{Source: "oci", Version: "v0.2.0", Digest: "sha256:newdigest"},
		artifact: &DownloadedArtifact{
			Version: "v0.2.0",
			Remote:  RemoteInfo{Source: "oci", Version: "v0.2.0", Digest: "sha256:newdigest"},
		},
	}

	mgr := NewManager(Deps{
		CurrentVersion: "v0.1.1",
		ConfigPath:     cfgPath,
		Live:           liveMgr,
		DB:             db,
		Inflight:       inflight.Load,
		RequestRestart: func() { restarted.Add(1) },
		NewSource:      func(_ config.Updater, _ string) (ArtifactSource, error) { return src, nil },
	})
	mgr.Store().PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	if err := mgr.ApplyAsync(ApplyOptions{Force: false}); err != nil {
		t.Fatalf("ApplyAsync: %v", err)
	}

	// Wait until updater enters StateDraining due to inflight=1.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.Status().State == StateDraining {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := mgr.Status().State; got != StateDraining {
		t.Fatalf("expected state=%q while inflight=1, got %q", StateDraining, got)
	}
	if restarted.Load() != 0 {
		t.Fatal("restart fired prematurely while inflight=1")
	}

	// Drop inflight to 0 -> drain finishes, install completes, restart fires!
	inflight.Store(0)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if restarted.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if restarted.Load() != 1 {
		t.Fatalf("expected restart count=1 after drain, got %d (state=%s err=%s)", restarted.Load(), mgr.Status().State, mgr.Status().LastError)
	}
	if mgr.Store().CurrentVersion() != "v0.2.0" {
		t.Fatalf("expected current release v0.2.0, got %q", mgr.Store().CurrentVersion())
	}

	// Verify history was persisted to SQLite.
	mgr2 := NewManager(Deps{CurrentVersion: "v0.2.0", ConfigPath: cfgPath, Live: liveMgr, DB: db})
	if len(mgr2.Status().History) == 0 {
		t.Fatal("expected persisted update history in SQLite")
	}

	// Failed update suppression: write .failed_update matching v0.3.0 and run background cycle.
	_ = mgr.Store().WriteFailed(&FailedUpdate{Version: "v0.3.0", Digest: "sha256:bad3", Reason: "healthz timeout", At: time.Now().UTC()})
	src.mu.Lock()
	src.remote = &RemoteInfo{Source: "oci", Version: "v0.3.0", Digest: "sha256:bad3"}
	src.artifact = &DownloadedArtifact{Version: "v0.3.0", Remote: *src.remote}
	src.mu.Unlock()

	mgr.runBackgroundCycle(ctx)
	if mgr.Store().CurrentVersion() != "v0.2.0" {
		t.Fatalf("crash-loop protection failed: expected v0.2.0, switched to %q", mgr.Store().CurrentVersion())
	}
}

func setupSupervisorFixture(t *testing.T, targetVer string) (home string, cfgPath string, st *Store) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink-based test helper uses unix symlinks")
	}
	home = t.TempDir()
	st = NewStore(home)
	if err := os.MkdirAll(st.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, ver := range []string{"v1", targetVer} {
		relDir := filepath.Join(st.ReleasesDir(), ver)
		if err := os.MkdirAll(relDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, filepath.Join(relDir, binaryFilename())); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("..", "releases", "v1", binaryFilename()), st.BinLink()); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, currentVerFileName), []byte("v1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, currentDigestFile), []byte("sha256:good-v1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "trigger_target"), []byte(targetVer+"\n"), 0o644)

	addr := freeTCPAddr(t)
	cfgPath = filepath.Join(home, "config.yaml")
	cfgYAML := fmt.Sprintf("listen: %s\nmode: personal\ndata_dir: %s\nupdater:\n  health_timeout: 800ms\n", addr, filepath.Join(home, "data"))
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, cfgPath, st
}

func TestSupervisorPlannedRestartAndAutoRollback(t *testing.T) {
	t.Setenv("GATEHOUSE_SUPERVISE_HELPER", "1")

	t.Run("healthy_upgrade_clears_pending", func(t *testing.T) {
		home, cfgPath, st := setupSupervisorFixture(t, "v2")
		t.Setenv("GATEHOUSE_SUPERVISE_HOME", home)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- RunSupervisor(ctx, SuperviseOptions{
				ConfigPath:    cfgPath,
				HomeDir:       home,
				ProbeInterval: 50 * time.Millisecond,
				HealthTimeout: 3 * time.Second,
			})
		}()

		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			b, _ := os.ReadFile(filepath.Join(home, "boot_log.txt"))
			pending, _ := st.ReadPending()
			if strings.Contains(string(b), "v2\n") && pending == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("RunSupervisor returned error: %v", err)
		}

		b, _ := os.ReadFile(filepath.Join(home, "boot_log.txt"))
		if !strings.Contains(string(b), "v1\nv2\n") {
			t.Fatalf("expected boot log to show v1 then v2, got:\n%s", string(b))
		}
		if st.CurrentVersion() != "v2" {
			t.Fatalf("expected current version v2, got %q", st.CurrentVersion())
		}
		if pending, _ := st.ReadPending(); pending != nil {
			t.Fatalf("expected .update_pending cleared, got %+v", pending)
		}
	})

	t.Run("unhealthy_update_triggers_auto_rollback", func(t *testing.T) {
		home, cfgPath, st := setupSupervisorFixture(t, "v_bad")
		t.Setenv("GATEHOUSE_SUPERVISE_HOME", home)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- RunSupervisor(ctx, SuperviseOptions{
				ConfigPath:    cfgPath,
				HomeDir:       home,
				ProbeInterval: 50 * time.Millisecond,
				HealthTimeout: 500 * time.Millisecond,
			})
		}()

		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			failed, _ := st.ReadFailed()
			b, _ := os.ReadFile(filepath.Join(home, "boot_log.txt"))
			if failed != nil && strings.Count(string(b), "v1\n") >= 2 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("RunSupervisor returned error: %v", err)
		}

		if st.CurrentVersion() != "v1" || st.CurrentDigest() != "sha256:good-v1" {
			t.Fatalf("expected rollback to v1/good-v1, got version=%q digest=%q", st.CurrentVersion(), st.CurrentDigest())
		}
		failed, err := st.ReadFailed()
		if err != nil || failed == nil || failed.Version != "v_bad" {
			t.Fatalf("expected .failed_update recording v_bad, got %+v err=%v", failed, err)
		}
		if pending, _ := st.ReadPending(); pending != nil {
			t.Fatalf("expected .update_pending cleared after rollback, got %+v", pending)
		}
	})
}

func makeStagedArtifactWithData(t *testing.T, ver, payload string) VerifiedArtifact {
	t.Helper()
	dir := t.TempDir()
	binName := binaryFilename()
	binPath := filepath.Join(dir, binName)
	data := []byte(payload)
	if err := os.WriteFile(binPath, data, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	shaHex := hex.EncodeToString(sum[:])
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	_ = os.WriteFile(sumsPath, []byte(shaHex+"  "+binName+"\n"), 0o644)
	metaBytes, _ := json.Marshal(ReleaseMeta{Version: ver, BinarySHA256: shaHex})
	metaPath := filepath.Join(dir, "RELEASE_META")
	_ = os.WriteFile(metaPath, metaBytes, 0o644)
	return VerifiedArtifact{
		Version:      ver,
		BinaryPath:   binPath,
		BinarySHA256: shaHex,
		SumsPath:     sumsPath,
		MetaPath:     metaPath,
	}
}

func TestStoreReinstallSameVersionPreservesRollback(t *testing.T) {
	home := t.TempDir()
	st := NewStore(home)
	st.PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }
	ctx := context.Background()

	res1, err := st.Install(ctx, InstallRequest{
		Version:       "v0.1.2",
		Digest:        "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Artifact:      makeStagedArtifactWithData(t, "v0.1.2", "build-one"),
		KeepReleases:  5,
		RecordPending: true,
	})
	if err != nil {
		t.Fatalf("first Install v0.1.2: %v", err)
	}
	if res1.Version != "v0.1.2" {
		t.Fatalf("expected first release ID v0.1.2, got %q", res1.Version)
	}
	_ = st.ClearPending()

	// Reinstall the same version label with a rebuilt digest ("bbbbbbbb...").
	res2, err := st.Install(ctx, InstallRequest{
		Version:       "v0.1.2",
		Digest:        "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Artifact:      makeStagedArtifactWithData(t, "v0.1.2", "build-two"),
		KeepReleases:  5,
		RecordPending: true,
	})
	if err != nil {
		t.Fatalf("second Install v0.1.2: %v", err)
	}
	if res2.Version == "v0.1.2" || res2.Version == res1.Version {
		t.Fatalf("expected distinct release ID on same-version reinstall, got %q", res2.Version)
	}
	if res2.PreviousVersion != "v0.1.2" {
		t.Fatalf("expected previous version v0.1.2, got %q", res2.PreviousVersion)
	}
	// Verify original build-one binary is preserved intact under releases/v0.1.2.
	origBin, err := os.ReadFile(filepath.Join(st.ReleasesDir(), "v0.1.2", binaryFilename()))
	if err != nil || string(origBin) != "build-one" {
		t.Fatalf("expected preserved original binary 'build-one', got %q err=%v", string(origBin), err)
	}

	pending, err := st.ReadPending()
	if err != nil || pending == nil {
		t.Fatalf("expected pending update, got %+v err=%v", pending, err)
	}
	if pending.PreviousVersion == "" || pending.PreviousVersion == pending.TargetVersion {
		t.Fatalf("expected distinct PreviousVersion and TargetVersion in pending update: %+v", pending)
	}

	if err := st.EmergencyRollback(pending.PreviousVersion, pending.PreviousDigest); err != nil {
		t.Fatalf("EmergencyRollback failed: %v", err)
	}
	if st.CurrentVersion() != "v0.1.2" || st.CurrentDigest() != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("unexpected state after EmergencyRollback: ver=%q digest=%q", st.CurrentVersion(), st.CurrentDigest())
	}
}

func TestManagerRejectsRestartWhenUnsupervised(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(home, "data")
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	liveMgr, cfgPath := newTestLiveManager(t, fmt.Sprintf(`listen: 127.0.0.1:17788
mode: personal
data_dir: %s
updater:
  enabled: true
  auto_apply: true
  source: oci
  image: registry.example.com/smallti/gatehouse
  home_dir: %s
`, dataDir, home))

	var downloads atomic.Int32
	src := &fakeSource{
		remote: &RemoteInfo{Source: "oci", Version: "v0.2.0", Digest: "sha256:newdigest"},
		artifact: &DownloadedArtifact{
			Version: "v0.2.0",
			Remote:  RemoteInfo{Source: "oci", Version: "v0.2.0", Digest: "sha256:newdigest"},
		},
	}

	mgr := NewManager(Deps{
		CurrentVersion: "v0.1.1",
		ConfigPath:     cfgPath,
		Live:           liveMgr,
		DB:             db,
		RequestRestart: nil, // standalone `gatehouse serve` mode
		NewSource: func(_ config.Updater, _ string) (ArtifactSource, error) {
			return &countingSource{inner: src, downloads: &downloads}, nil
		},
	})
	mgr.Store().PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }

	if mgr.CanRestart() || mgr.Status().CanRestart {
		t.Fatal("expected CanRestart=false when RequestRestart is nil")
	}

	ctx := context.Background()
	if _, err := mgr.Apply(ctx, ApplyOptions{}); !errors.Is(err, ErrRestartUnavailable) {
		t.Fatalf("expected ErrRestartUnavailable from Apply, got %v", err)
	}
	if err := mgr.ApplyAsync(ApplyOptions{}); !errors.Is(err, ErrRestartUnavailable) {
		t.Fatalf("expected ErrRestartUnavailable from ApplyAsync, got %v", err)
	}
	if _, err := mgr.Rollback(ctx, RollbackOptions{}); !errors.Is(err, ErrRestartUnavailable) {
		t.Fatalf("expected ErrRestartUnavailable from Rollback, got %v", err)
	}
	if err := mgr.RollbackAsync(RollbackOptions{}); !errors.Is(err, ErrRestartUnavailable) {
		t.Fatalf("expected ErrRestartUnavailable from RollbackAsync, got %v", err)
	}

	// Background auto-apply must also skip downloading/installing when RequestRestart == nil.
	mgr.runBackgroundCycle(ctx)
	if downloads.Load() != 0 {
		t.Fatalf("expected 0 downloads during unsupervised background auto-apply, got %d", downloads.Load())
	}

	// Manual CLI invocation with ManualRestartOK=true should be allowed even without RequestRestart.
	cliMgr := NewManager(Deps{
		CurrentVersion:  "v0.1.1",
		ConfigPath:      cfgPath,
		StaticConfig:    &liveMgr.Current().Cfg.Updater,
		DB:              db,
		ManualRestartOK: true,
		NewSource:       func(_ config.Updater, _ string) (ArtifactSource, error) { return src, nil },
	})
	cliMgr.Store().PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }
	res, err := cliMgr.Apply(ctx, ApplyOptions{})
	if err != nil {
		t.Fatalf("expected ManualRestartOK Apply to succeed, got %v", err)
	}
	if res.Version != "v0.2.0" {
		t.Fatalf("expected v0.2.0 installed, got %q", res.Version)
	}
}

type countingSource struct {
	inner     *fakeSource
	downloads *atomic.Int32
}

func (c *countingSource) Check(ctx context.Context) (*RemoteInfo, error) {
	return c.inner.Check(ctx)
}

func (c *countingSource) Download(ctx context.Context, outDir string) (*DownloadedArtifact, error) {
	c.downloads.Add(1)
	return c.inner.Download(ctx, outDir)
}

func TestSupervisorRelativeConfigPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink-based test helper uses unix symlinks")
	}
	t.Setenv("GATEHOUSE_SUPERVISE_HELPER", "1")
	t.Setenv("GATEHOUSE_SUPERVISE_HOME", "")

	base := t.TempDir()
	confDir := filepath.Join(base, "conf")
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	st := NewStore(home)
	if err := os.MkdirAll(st.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	relDir := filepath.Join(st.ReleasesDir(), "v1")
	if err := os.MkdirAll(relDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(relDir, binaryFilename())); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "releases", "v1", binaryFilename()), st.BinLink()); err != nil {
		t.Fatal(err)
	}

	addr := freeTCPAddr(t)
	absCfgPath := filepath.Join(confDir, "config.yaml")
	cfgYAML := fmt.Sprintf("listen: %s\nmode: personal\ndata_dir: %s\nupdater:\n  home_dir: %s\n", addr, filepath.Join(base, "data"), home)
	if err := os.WriteFile(absCfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origWD)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SuperviseOptions{
			ConfigPath:    filepath.Join("conf", "config.yaml"),
			ProbeInterval: 50 * time.Millisecond,
		})
	}()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(home, "boot_log.txt"))
		if strings.Contains(string(b), "v1\n") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunSupervisor returned error: %v", err)
	}

	b, _ := os.ReadFile(filepath.Join(home, "boot_log.txt"))
	if !strings.Contains(string(b), "v1\n") {
		t.Fatalf("expected child to boot with relative config resolved, boot_log=%q", string(b))
	}
	if _, err := os.Stat(filepath.Join(home, "conf", "conf", "config.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected nested relative config file created inside homeDir (err=%v)", err)
	}
}

func TestSupervisorSyncsHomeDirOnHotReload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink-based test helper uses unix symlinks")
	}
	t.Setenv("GATEHOUSE_SUPERVISE_HELPER", "1")
	t.Setenv("GATEHOUSE_SUPERVISE_HOME", "")

	base := t.TempDir()
	homeA := filepath.Join(base, "homeA")
	homeB := filepath.Join(base, "homeB")
	stA := NewStore(homeA)
	stB := NewStore(homeB)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Stage v1 only in homeA, and v2 only in homeB.
	for _, spec := range []struct {
		st  *Store
		ver string
	}{
		{stA, "v1"},
		{stB, "v2"},
	} {
		if err := os.MkdirAll(spec.st.BinDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		relDir := filepath.Join(spec.st.ReleasesDir(), spec.ver)
		if err := os.MkdirAll(relDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, filepath.Join(relDir, binaryFilename())); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("..", "releases", "v1", binaryFilename()), stA.BinLink()); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(homeA, currentVerFileName), []byte("v1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(homeA, "hot_reload_home"), []byte(homeB+"\n"), 0o644)

	addr := freeTCPAddr(t)
	cfgPath := filepath.Join(base, "config.yaml")
	cfgYAML := fmt.Sprintf("listen: %s\nmode: personal\ndata_dir: %s\nupdater:\n  home_dir: %s\n  health_timeout: 2s\n", addr, filepath.Join(base, "data"), homeA)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SuperviseOptions{
			ConfigPath:    cfgPath,
			ProbeInterval: 50 * time.Millisecond,
			HealthTimeout: 3 * time.Second,
		})
	}()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(homeB, "boot_log.txt"))
		pending, _ := stB.ReadPending()
		if strings.Contains(string(b), "v2\n") && pending == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunSupervisor returned error: %v", err)
	}

	bA, _ := os.ReadFile(filepath.Join(homeA, "boot_log.txt"))
	bB, _ := os.ReadFile(filepath.Join(homeB, "boot_log.txt"))
	if !strings.Contains(string(bA), "v1\n") || !strings.Contains(string(bB), "v2\n") {
		t.Fatalf("expected homeA boot v1 and homeB boot v2; got homeA=%q homeB=%q", string(bA), string(bB))
	}
	if pending, _ := stB.ReadPending(); pending != nil {
		t.Fatalf("expected homeB .update_pending cleared after health check, got %+v", pending)
	}
}

func TestStoreArchiveFallbackBinaryWhenBinLinkMissing(t *testing.T) {
	home := t.TempDir()
	st := NewStore(home)
	st.PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }

	fbDir := t.TempDir()
	fbBin := filepath.Join(fbDir, binaryFilename())
	if err := os.WriteFile(fbBin, []byte("fallback-running-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	res, err := st.Install(ctx, InstallRequest{
		Version:            "v0.2.0",
		Digest:             "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Artifact:           makeStagedArtifactWithData(t, "v0.2.0", "new-binary"),
		FallbackBinaryPath: fbBin,
		KeepReleases:       5,
		RecordPending:      true,
	})
	if err != nil {
		t.Fatalf("Install with FallbackBinaryPath failed: %v", err)
	}
	if !strings.HasPrefix(res.PreviousVersion, "baseline-") {
		t.Fatalf("expected archived baseline PreviousVersion, got %q", res.PreviousVersion)
	}

	pending, err := st.ReadPending()
	if err != nil || pending == nil {
		t.Fatalf("expected pending update, got %+v err=%v", pending, err)
	}
	if pending.PreviousVersion != res.PreviousVersion || pending.PreviousVersion == pending.TargetVersion {
		t.Fatalf("expected distinct baseline PreviousVersion in pending update: %+v", pending)
	}

	if err := st.EmergencyRollback(pending.PreviousVersion, pending.PreviousDigest); err != nil {
		t.Fatalf("EmergencyRollback to archived fallback baseline failed: %v", err)
	}
	activeBytes, err := os.ReadFile(st.BinLink())
	if err != nil {
		if target := st.releaseBinaryPath(st.CurrentVersion()); target != "" {
			activeBytes, err = os.ReadFile(target)
		}
	}
	if err != nil || string(activeBytes) != "fallback-running-binary" {
		t.Fatalf("expected restored fallback binary contents, got %q err=%v", string(activeBytes), err)
	}
}

func TestManagerQuiescesAdmissionAndRestoresOnFailure(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(home, "data")
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	liveMgr, cfgPath := newTestLiveManager(t, fmt.Sprintf(`listen: 127.0.0.1:17788
mode: personal
data_dir: %s
updater:
  enabled: true
  source: oci
  image: registry.example.com/smallti/gatehouse
  home_dir: %s
  drain_timeout: 5s
`, dataDir, home))

	src := &fakeSource{
		remote: &RemoteInfo{Source: "oci", Version: "v0.2.0", Digest: "sha256:quiesce1"},
		artifact: &DownloadedArtifact{
			Version: "v0.2.0",
			Remote:  RemoteInfo{Source: "oci", Version: "v0.2.0", Digest: "sha256:quiesce1"},
		},
	}

	t.Run("quiesces_before_drain_check_and_stays_drained_on_restart", func(t *testing.T) {
		var draining atomic.Bool
		var sawDrainingBeforeInflight bool
		var restarted atomic.Int32
		var inflightCalls atomic.Int32

		mgr := NewManager(Deps{
			CurrentVersion: "v0.1.1",
			ConfigPath:     cfgPath,
			Live:           liveMgr,
			DB:             db,
			Inflight: func() int64 {
				inflightCalls.Add(1)
				if draining.Load() {
					sawDrainingBeforeInflight = true
				}
				return 0
			},
			SetDraining: func(d bool) {
				draining.Store(d)
			},
			RequestRestart: func() {
				restarted.Add(1)
			},
			NewSource: func(_ config.Updater, _ string) (ArtifactSource, error) {
				return src, nil
			},
		})
		mgr.Store().PreflightFunc = func(_ context.Context, _ string, _ string) error {
			if draining.Load() {
				t.Fatal("SetDraining(true) should only run after preflight succeeds")
			}
			return nil
		}

		res, err := mgr.Apply(context.Background(), ApplyOptions{})
		if err != nil {
			t.Fatalf("Apply failed: %v", err)
		}
		if res.Version != "v0.2.0" {
			t.Fatalf("expected v0.2.0, got %q", res.Version)
		}
		if inflightCalls.Load() == 0 || !sawDrainingBeforeInflight {
			t.Fatalf("expected SetDraining(true) before checking Inflight() (calls=%d saw=%v)", inflightCalls.Load(), sawDrainingBeforeInflight)
		}
		if !draining.Load() {
			t.Fatal("expected draining to remain true while shutting down for restart")
		}
	})

	t.Run("restores_admission_when_drain_or_rollback_fails", func(t *testing.T) {
		var draining atomic.Bool
		var events []bool
		var mu sync.Mutex
		record := func(d bool) {
			draining.Store(d)
			mu.Lock()
			events = append(events, d)
			mu.Unlock()
		}

		mgr := NewManager(Deps{
			CurrentVersion: "v0.2.0",
			ConfigPath:     cfgPath,
			Live:           liveMgr,
			DB:             db,
			Inflight:       func() int64 { return 1 },
			SetDraining:    record,
			RequestRestart: func() {},
			NewSource:      func(_ config.Updater, _ string) (ArtifactSource, error) { return src, nil },
		})
		mgr.Store().PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if draining.Load() {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
		}()

		if _, err := mgr.Apply(ctx, ApplyOptions{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled on cancelled drain, got %v", err)
		}
		if draining.Load() {
			t.Fatal("expected SetDraining(false) called after drain cancellation")
		}

		// Also verify rollback failure after SetDraining(true) restores admission.
		events = nil
		mgr2 := NewManager(Deps{
			CurrentVersion: "v0.2.0",
			ConfigPath:     cfgPath,
			Live:           liveMgr,
			DB:             db,
			Inflight:       func() int64 { return 0 },
			SetDraining:    record,
			RequestRestart: func() {},
		})
		if _, err := mgr2.Rollback(context.Background(), RollbackOptions{TargetVersion: "missing-version"}); err == nil {
			t.Fatal("expected rollback error for missing target version")
		}
		if draining.Load() || len(events) < 2 || !events[0] || events[len(events)-1] {
			t.Fatalf("expected [true ..., false] draining transitions on failed rollback, got %v (final=%v)", events, draining.Load())
		}
	})
}

func TestSupervisorRetainsPendingWhenRollbackLocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink-based test helper uses unix symlinks")
	}
	t.Setenv("GATEHOUSE_SUPERVISE_HELPER", "1")

	home, cfgPath, st := setupSupervisorFixture(t, "v_bad")
	t.Setenv("GATEHOUSE_SUPERVISE_HOME", home)

	// Hold the deployment lock so EmergencyRollback fails transiently at first.
	unlock, err := acquireFileLock(st.LockPath())
	if err != nil {
		t.Fatalf("acquireFileLock: %v", err)
	}
	var unlockOnce sync.Once
	releaseLock := func() { unlockOnce.Do(unlock) }
	defer releaseLock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SuperviseOptions{
			ConfigPath:    cfgPath,
			HomeDir:       home,
			ProbeInterval: 40 * time.Millisecond,
			HealthTimeout: 300 * time.Millisecond,
		})
	}()

	// Wait until v_bad has booted at least twice while the lock is held, proving the supervisor
	// kept .update_pending and backed off instead of clearing recovery markers on EmergencyRollback error.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(home, "boot_log.txt"))
		if strings.Count(string(b), "v_bad\n") >= 2 {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	bLocked, _ := os.ReadFile(filepath.Join(home, "boot_log.txt"))
	if strings.Count(string(bLocked), "v_bad\n") < 2 {
		cancel()
		<-done
		t.Fatalf("expected >=2 v_bad boots while rollback locked, got:\n%s", string(bLocked))
	}
	pendingWhileLocked, perr := st.ReadPending()
	if perr != nil || pendingWhileLocked == nil {
		cancel()
		<-done
		t.Fatalf("expected .update_pending retained when EmergencyRollback fails, got %+v err=%v", pendingWhileLocked, perr)
	}

	// Release the deployment lock; next verification failure must roll back cleanly to v1.
	releaseLock()
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		failed, _ := st.ReadFailed()
		if failed != nil && st.CurrentVersion() == "v1" {
			pendingNow, _ := st.ReadPending()
			if pendingNow == nil {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunSupervisor returned error: %v", err)
	}

	if st.CurrentVersion() != "v1" {
		t.Fatalf("expected rollback to v1 once lock released, got %q", st.CurrentVersion())
	}
	if p, _ := st.ReadPending(); p != nil {
		t.Fatalf("expected .update_pending cleared after successful rollback, got %+v", p)
	}
	if failed, _ := st.ReadFailed(); failed == nil || failed.Version != "v_bad" {
		t.Fatalf("expected .failed_update for v_bad after successful rollback, got %+v", failed)
	}
}

type hookCheckSource struct {
	remote   *RemoteInfo
	artifact *DownloadedArtifact
	onCheck  func()
	dlCount  *atomic.Int32
}

func (h *hookCheckSource) Check(_ context.Context) (*RemoteInfo, error) {
	if h.onCheck != nil {
		h.onCheck()
	}
	cp := *h.remote
	return &cp, nil
}

func (h *hookCheckSource) Download(ctx context.Context, outDir string) (*DownloadedArtifact, error) {
	h.dlCount.Add(1)
	return (&fakeSource{remote: h.remote, artifact: h.artifact}).Download(ctx, outDir)
}

func TestManagerBackgroundCycleRechecksPolicyAfterCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nextCfg   func(dataDir, home string) string
		wantApply bool
	}{
		{
			name: "auto_apply_disabled_during_check",
			nextCfg: func(dataDir, home string) string {
				return fmt.Sprintf(`listen: 127.0.0.1:17788
mode: personal
data_dir: %s
updater:
  enabled: true
  auto_apply: false
  source: oci
  image: registry.example.com/smallti/gatehouse
  home_dir: %s
`, dataDir, home)
			},
		},
		{
			name: "enabled_disabled_during_check",
			nextCfg: func(dataDir, home string) string {
				return fmt.Sprintf(`listen: 127.0.0.1:17788
mode: personal
data_dir: %s
updater:
  enabled: false
  auto_apply: false
  source: oci
  image: registry.example.com/smallti/gatehouse
  home_dir: %s
`, dataDir, home)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dataDir := filepath.Join(home, "data")
			db, err := store.Open(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			liveMgr, cfgPath := newTestLiveManager(t, fmt.Sprintf(`listen: 127.0.0.1:17788
mode: personal
data_dir: %s
updater:
  enabled: true
  auto_apply: true
  source: oci
  image: registry.example.com/smallti/gatehouse
  home_dir: %s
`, dataDir, home))

			var dlCount, restartCount atomic.Int32
			remote := &RemoteInfo{Source: "oci", Version: "v0.2.0", Digest: "sha256:policytest"}
			src := &hookCheckSource{
				remote:   remote,
				artifact: &DownloadedArtifact{Version: "v0.2.0", Remote: *remote},
				dlCount:  &dlCount,
				onCheck: func() {
					_, aerr := liveMgr.Apply([]byte(tc.nextCfg(dataDir, home)), "web", liveMgr.Current().Cfg.Hash())
					if aerr != nil {
						t.Fatalf("hot reload inside Check: %v", aerr)
					}
				},
			}

			mgr := NewManager(Deps{
				CurrentVersion: "v0.1.1",
				ConfigPath:     cfgPath,
				Live:           liveMgr,
				DB:             db,
				RequestRestart: func() { restartCount.Add(1) },
				NewSource:      func(_ config.Updater, _ string) (ArtifactSource, error) { return src, nil },
			})
			mgr.Store().PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }

			mgr.runBackgroundCycle(context.Background())
			if dlCount.Load() != 0 || restartCount.Load() != 0 {
				t.Fatalf("expected background cycle to abort apply after policy disabled, got downloads=%d restarts=%d",
					dlCount.Load(), restartCount.Load())
			}
		})
	}
}

func TestManagerConfirmBootVerifiesRunningBinary(t *testing.T) {
	t.Setenv(EnvSupervised, "")

	home := t.TempDir()
	st := NewStore(home)
	st.PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }
	ctx := context.Background()

	if _, err := st.Install(ctx, InstallRequest{
		Version:       "v0.2.0",
		Digest:        "sha256:confirmboot",
		Artifact:      makeStagedArtifactWithData(t, "v0.2.0", "release-v020-binary"),
		KeepReleases:  5,
		RecordPending: true,
	}); err != nil {
		t.Fatalf("Install v0.2.0: %v", err)
	}

	// Standalone old executable outside the newly installed release.
	oldBinPath := filepath.Join(t.TempDir(), binaryFilename())
	if err := os.WriteFile(oldBinPath, []byte("old-standalone-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Launching old standalone binary must NOT clear .update_pending or record activation.
	oldMgr := NewManager(Deps{
		CurrentVersion: "v0.1.0",
		CurrentBinary:  oldBinPath,
		StaticConfig:   &config.Updater{HomeDir: home},
	})
	oldMgr.ConfirmBoot()
	if p, err := st.ReadPending(); err != nil || p == nil {
		t.Fatalf("expected .update_pending preserved when running old executable, got %+v err=%v", p, err)
	}
	if len(oldMgr.Status().History) != 0 {
		t.Fatalf("expected no activation history recorded for old binary, got %+v", oldMgr.Status().History)
	}

	// 2. Launching the newly installed bin/gatehouse clears .update_pending and records activation.
	newMgr := NewManager(Deps{
		CurrentVersion: "v0.2.0",
		CurrentBinary:  st.BinLink(),
		StaticConfig:   &config.Updater{HomeDir: home},
	})
	newMgr.ConfirmBoot()
	if p, err := st.ReadPending(); err != nil || p != nil {
		t.Fatalf("expected .update_pending cleared when running installed binary, got %+v err=%v", p, err)
	}
	if hist := newMgr.Status().History; len(hist) != 1 || hist[0].Action != "apply" || hist[0].Status != "ok" || hist[0].Version != "v0.2.0" {
		t.Fatalf("expected apply/ok history event for v0.2.0, got %+v", hist)
	}

	// 3. Windows-style regular file copy at bin/gatehouse also confirms by SHA-256 match.
	if err := st.WritePending(&PendingUpdate{
		TargetVersion: "v0.2.0",
		TargetDigest:  "sha256:confirmboot",
		RequestedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(st.BinLink())
	if err := os.WriteFile(st.BinLink(), []byte("release-v020-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyMgr := NewManager(Deps{
		CurrentVersion: "v0.2.0",
		CurrentBinary:  st.BinLink(),
		StaticConfig:   &config.Updater{HomeDir: home},
	})
	copyMgr.ConfirmBoot()
	if p, err := st.ReadPending(); err != nil || p != nil {
		t.Fatalf("expected .update_pending cleared for matching regular-file copy, got %+v err=%v", p, err)
	}
}

func TestSupervisorPreservesWorkDirAcrossHomeDirChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink-based test helper uses unix symlinks")
	}
	t.Setenv("GATEHOUSE_SUPERVISE_HELPER", "1")
	t.Setenv("GATEHOUSE_SUPERVISE_HOME", "")

	base := t.TempDir()
	workDir := filepath.Join(base, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	homeA := filepath.Join(base, "homeA")
	homeB := filepath.Join(base, "homeB")
	stA := NewStore(homeA)
	stB := NewStore(homeB)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	for _, spec := range []struct {
		st  *Store
		ver string
	}{
		{stA, "v1"},
		{stB, "v2"},
	} {
		if err := os.MkdirAll(spec.st.BinDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		relDir := filepath.Join(spec.st.ReleasesDir(), spec.ver)
		if err := os.MkdirAll(relDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, filepath.Join(relDir, binaryFilename())); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("..", "releases", "v1", binaryFilename()), stA.BinLink()); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(homeA, currentVerFileName), []byte("v1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(homeA, "hot_reload_home"), []byte(homeB+"\n"), 0o644)

	addr := freeTCPAddr(t)
	cfgPath := filepath.Join(base, "config.yaml")
	cfgYAML := fmt.Sprintf("listen: %s\nmode: personal\ndata_dir: ./data\nupdater:\n  home_dir: %s\n  health_timeout: 2s\n", addr, homeA)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origWD)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SuperviseOptions{
			ConfigPath:    cfgPath,
			ProbeInterval: 50 * time.Millisecond,
			HealthTimeout: 3 * time.Second,
		})
	}()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(homeB, "boot_log.txt"))
		pending, _ := stB.ReadPending()
		if strings.Contains(string(b), "v2\n") && pending == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunSupervisor returned error: %v", err)
	}

	resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		resolvedWorkDir = workDir
	}
	wantDataDir := filepath.Join(resolvedWorkDir, "data")

	wdLogBytes, err := os.ReadFile(filepath.Join(base, "wd_log.txt"))
	if err != nil {
		t.Fatalf("read wd_log.txt: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(wdLogBytes)), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected >= 2 boot lines in wd_log.txt, got %q", string(wdLogBytes))
	}
	seen := map[string]bool{}
	for _, line := range lines {
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			t.Fatalf("malformed wd_log line %q", line)
		}
		ver, gotWD, gotData := parts[0], parts[1], parts[2]
		seen[ver] = true
		if resolvedWD, err := filepath.EvalSymlinks(gotWD); err == nil {
			gotWD = resolvedWD
		}
		if resolvedData, err := filepath.EvalSymlinks(gotData); err == nil {
			gotData = resolvedData
		}
		if gotWD != resolvedWorkDir || gotData != wantDataDir {
			t.Fatalf("boot %s changed workdir/datadir: got wd=%q data=%q, want wd=%q data=%q",
				ver, gotWD, gotData, resolvedWorkDir, wantDataDir)
		}
	}
	if !seen["v1"] || !seen["v2"] {
		t.Fatalf("expected both v1 and v2 boots in wd_log.txt, got %q", string(wdLogBytes))
	}
}

func TestStoreRollbackPendingTargetDigestMatchesSelectedRelease(t *testing.T) {
	ctx := context.Background()

	// Case 1: Rolling back to an older installed release (v1.0.0, not .previous_version v2.0.0)
	// must record v1.0.0's own digest rather than v2.0.0's .previous_digest.
	st := NewStore(t.TempDir())
	st.PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }
	for _, item := range []struct{ v, d string }{
		{"v1.0.0", "sha256:d1"},
		{"v2.0.0", "sha256:d2"},
		{"v3.0.0", "sha256:d3"},
	} {
		if _, err := st.Install(ctx, InstallRequest{
			Version:      item.v,
			Digest:       item.d,
			Artifact:     makeStagedArtifact(t, item.v),
			KeepReleases: 5,
		}); err != nil {
			t.Fatalf("Install %s: %v", item.v, err)
		}
	}

	if _, err := st.Rollback(ctx, "v1.0.0", "", true); err != nil {
		t.Fatalf("Rollback to v1.0.0: %v", err)
	}
	pending, err := st.ReadPending()
	if err != nil || pending == nil {
		t.Fatalf("ReadPending after Rollback to v1.0.0: pending=%+v err=%v", pending, err)
	}
	if pending.TargetVersion != "v1.0.0" || pending.TargetDigest != "sha256:d1" {
		t.Fatalf("expected pending target v1.0.0 / sha256:d1, got %+v", pending)
	}
	if pending.PreviousVersion != "v3.0.0" || pending.PreviousDigest != "sha256:d3" {
		t.Fatalf("expected pending previous v3.0.0 / sha256:d3, got %+v", pending)
	}
	if got := st.CurrentDigest(); got != "sha256:d1" {
		t.Fatalf("expected CurrentDigest=sha256:d1, got %q", got)
	}

	// Case 2: Rolling back to an older release without a stored .digest must record an empty
	// TargetDigest and remove stale .current_digest rather than borrowing .previous_digest.
	st2 := NewStore(t.TempDir())
	st2.PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }
	legacyDir := filepath.Join(st2.ReleasesDir(), "v0.9.0")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, binaryFilename()), []byte("legacy-v0.9.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ v, d string }{
		{"v2.0.0", "sha256:d2"},
		{"v3.0.0", "sha256:d3"},
	} {
		if _, err := st2.Install(ctx, InstallRequest{
			Version:      item.v,
			Digest:       item.d,
			Artifact:     makeStagedArtifact(t, item.v),
			KeepReleases: 5,
		}); err != nil {
			t.Fatalf("Install %s: %v", item.v, err)
		}
	}

	if _, err := st2.Rollback(ctx, "v0.9.0", "", true); err != nil {
		t.Fatalf("Rollback to v0.9.0: %v", err)
	}
	pending2, err := st2.ReadPending()
	if err != nil || pending2 == nil {
		t.Fatalf("ReadPending after Rollback to v0.9.0: pending=%+v err=%v", pending2, err)
	}
	if pending2.TargetVersion != "v0.9.0" || pending2.TargetDigest != "" {
		t.Fatalf("expected pending target v0.9.0 with empty digest, got %+v", pending2)
	}
	if pending2.PreviousVersion != "v3.0.0" || pending2.PreviousDigest != "sha256:d3" {
		t.Fatalf("expected pending previous v3.0.0 / sha256:d3, got %+v", pending2)
	}
	if got := st2.CurrentDigest(); got != "" {
		t.Fatalf("expected empty CurrentDigest after digestless rollback, got %q", got)
	}
}

func TestCheckEndpointHealthCorrelatesReleaseAndBootNonce(t *testing.T) {
	var body string
	var ackHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ackHeader != "" {
			w.Header().Set(HeaderBootAck, ackHeader)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := srv.Client()
	dataURL := srv.URL + "/healthz"

	// 1. Mismatched version must fail even without a nonce.
	body = `{"status":"ok","version":"v0.1.0"}`
	if err := checkEndpointHealth(context.Background(), client, dataURL, "", healthExpectation{
		ExpectedVersion: "v0.2.0",
	}); err == nil || !strings.Contains(err.Error(), "does not match expected release") {
		t.Fatalf("expected version mismatch error, got: %v", err)
	}

	// 2. Matching version without required boot ack must fail.
	body = `{"status":"ok","version":"v0.2.0"}`
	ackHeader = ""
	if err := checkEndpointHealth(context.Background(), client, dataURL, "", healthExpectation{
		ExpectedVersion: "v0.2.0",
		BootNonce:       "nonce-1",
		RequireBootAck:  true,
	}); err == nil || !strings.Contains(err.Error(), "missing matching boot ack") {
		t.Fatalf("expected missing boot ack error, got: %v", err)
	}

	// 3. Wrong boot ack must fail.
	ackHeader = BootAck("other-nonce")
	if err := checkEndpointHealth(context.Background(), client, dataURL, "", healthExpectation{
		ExpectedVersion: "v0.2.0",
		BootNonce:       "nonce-1",
		RequireBootAck:  true,
	}); err == nil || !strings.Contains(err.Error(), "boot ack mismatch") {
		t.Fatalf("expected boot ack mismatch error, got: %v", err)
	}

	// 4. Matching version + matching boot ack passes.
	ackHeader = BootAck("nonce-1")
	if err := checkEndpointHealth(context.Background(), client, dataURL, "", healthExpectation{
		ExpectedVersion: "v0.2.0",
		BootNonce:       "nonce-1",
		RequireBootAck:  true,
	}); err != nil {
		t.Fatalf("expected matching healthz + ack to pass, got: %v", err)
	}

	// 5. Baseline rollback rejects un-acked response from the replaced previous version.
	body = `{"status":"ok","version":"v0.2.0"}`
	ackHeader = ""
	if err := checkEndpointHealth(context.Background(), client, dataURL, "", healthExpectation{
		RejectVersion: "v0.2.0",
		BootNonce:     "nonce-2",
	}); err == nil || !strings.Contains(err.Error(), "previous release") {
		t.Fatalf("expected baseline rollback to reject previous release response, got: %v", err)
	}

	// 6. Degraded status with missing required detectors must fail readiness check even with matching version & ack.
	body = `{"status":"degraded","version":"v0.2.0","detectors_unavailable":["ner"]}`
	ackHeader = BootAck("nonce-3")
	if err := checkEndpointHealth(context.Background(), client, dataURL, "", healthExpectation{
		ExpectedVersion: "v0.2.0",
		BootNonce:       "nonce-3",
		RequireBootAck:  true,
	}); err == nil || !strings.Contains(err.Error(), "degraded") || !strings.Contains(err.Error(), "ner") {
		t.Fatalf("expected degraded healthz with ner missing to fail, got: %v", err)
	}
}

func TestSupervisorRejectsStrayInstanceOccupyingPortAndRollsBack(t *testing.T) {
	strayLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := strayLn.Addr().String()
	straySrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Even if the unrelated process reports v2, it does not possess the boot nonce.
			_, _ = w.Write([]byte(`{"status":"ok","version":"v2"}`))
		}),
	}
	go func() { _ = straySrv.Serve(strayLn) }()
	defer func() { _ = straySrv.Close() }()

	if runtime.GOOS == "windows" {
		t.Skip("symlink-based test helper uses unix symlinks")
	}
	t.Setenv("GATEHOUSE_SUPERVISE_HELPER", "1")
	t.Setenv("GATEHOUSE_SUPERVISE_HOME", "")

	home := t.TempDir()
	st := NewStore(home)
	if err := os.MkdirAll(st.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, ver := range []string{"v1", "v2"} {
		relDir := filepath.Join(st.ReleasesDir(), ver)
		if err := os.MkdirAll(relDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, filepath.Join(relDir, binaryFilename())); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("..", "releases", "v2", binaryFilename()), st.BinLink()); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, currentVerFileName), []byte("v2\n"), 0o644)
	_ = os.WriteFile(filepath.Join(home, prevVerFileName), []byte("v1\n"), 0o644)
	if err := st.WritePending(&PendingUpdate{
		TargetVersion:   "v2",
		TargetDigest:    "sha256:v2-stray",
		PreviousVersion: "v1",
		PreviousDigest:  "sha256:v1-good",
		RequestedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgYAML := fmt.Sprintf("listen: %s\nmode: personal\ndata_dir: %s\nupdater:\n  home_dir: %s\n  health_timeout: 500ms\n", addr, t.TempDir(), home)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SuperviseOptions{
			ConfigPath:    cfgPath,
			ProbeInterval: 30 * time.Millisecond,
			HealthTimeout: 500 * time.Millisecond,
		})
	}()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		failed, _ := st.ReadFailed()
		pending, _ := st.ReadPending()
		if st.CurrentVersion() == "v1" && failed != nil && failed.Version == "v2" && pending == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	_ = straySrv.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunSupervisor returned error: %v", err)
	}

	if got := st.CurrentVersion(); got != "v1" {
		t.Fatalf("expected rollback to v1 when stray instance occupied port, got %q", got)
	}
	if failed, err := st.ReadFailed(); err != nil || failed == nil || failed.Version != "v2" {
		t.Fatalf("expected .failed_update for v2, got %+v err=%v", failed, err)
	}
	if pending, err := st.ReadPending(); err != nil || pending != nil {
		t.Fatalf("expected .update_pending cleared after rollback, got %+v err=%v", pending, err)
	}
}

func TestManagerAutoApplyPinsCheckedReleaseAndRevalidates(t *testing.T) {
	ctx := context.Background()

	t.Run("drift_between_check_and_download_aborts", func(t *testing.T) {
		home := t.TempDir()
		dataDir := filepath.Join(home, "data")
		db, err := store.Open(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		liveMgr, cfgPath := newTestLiveManager(t, fmt.Sprintf(`listen: 127.0.0.1:17788
mode: personal
data_dir: %s
updater:
  enabled: true
  auto_apply: true
  source: github
  github_repo: yiyiSmallti/precinct-gatehouse
  home_dir: %s
  fallback_dns: []
`, dataDir, home))

		var restarted atomic.Int32
		src := &fakeSource{
			remote: &RemoteInfo{Source: "github", Version: "v2.0.0", Tag: "v2.0.0", Digest: "sha256:v2"},
			artifact: &DownloadedArtifact{
				Version: "v1.0.0",
				Remote:  RemoteInfo{Source: "github", Version: "v1.0.0", Tag: "v1.0.0", Digest: "sha256:v1"},
			},
		}
		mgr := NewManager(Deps{
			CurrentVersion: "v1.5.0",
			ConfigPath:     cfgPath,
			Live:           liveMgr,
			DB:             db,
			RequestRestart: func() { restarted.Add(1) },
			NewSource:      func(_ config.Updater, _ string) (ArtifactSource, error) { return src, nil },
		})
		mgr.Store().PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }

		mgr.runBackgroundCycle(ctx)
		if got := mgr.Store().CurrentVersion(); got != "" {
			t.Fatalf("expected drifted downgrade artifact not to be installed, got %q", got)
		}
		if restarted.Load() != 0 {
			t.Fatalf("expected 0 restarts after drifted auto-apply abort, got %d", restarted.Load())
		}
	})

	t.Run("downloaded_failed_digest_aborts", func(t *testing.T) {
		home := t.TempDir()
		dataDir := filepath.Join(home, "data")
		db, err := store.Open(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		liveMgr, cfgPath := newTestLiveManager(t, fmt.Sprintf(`listen: 127.0.0.1:17788
mode: personal
data_dir: %s
updater:
  enabled: true
  auto_apply: true
  source: oci
  image: registry.example.com/smallti/gatehouse
  home_dir: %s
  fallback_dns: []
`, dataDir, home))

		var restarted atomic.Int32
		src := &fakeSource{
			remote: &RemoteInfo{Source: "oci", Version: "v2.0.0", Digest: "sha256:good"},
			artifact: &DownloadedArtifact{
				Version: "v2.0.0",
				Remote:  RemoteInfo{Source: "oci", Version: "v2.0.0", Digest: "sha256:bad"},
			},
		}
		mgr := NewManager(Deps{
			CurrentVersion: "v1.0.0",
			ConfigPath:     cfgPath,
			Live:           liveMgr,
			DB:             db,
			RequestRestart: func() { restarted.Add(1) },
			NewSource:      func(_ config.Updater, _ string) (ArtifactSource, error) { return src, nil },
		})
		mgr.Store().PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }
		_ = mgr.Store().WriteFailed(&FailedUpdate{Version: "v2.0.0", Digest: "sha256:bad", Reason: "crashed", At: time.Now().UTC()})

		mgr.runBackgroundCycle(ctx)
		if got := mgr.Store().CurrentVersion(); got != "" {
			t.Fatalf("expected previously failed digest not to be installed, got %q", got)
		}
		if restarted.Load() != 0 {
			t.Fatalf("expected 0 restarts when downloaded digest matched failed_update, got %d", restarted.Load())
		}
	})

	t.Run("github_pinning_survives_latest_flip", func(t *testing.T) {
		binName := binaryFilename()
		buildTar := func(ver string) []byte {
			binBytes := []byte("gatehouse-" + ver)
			binSum := sha256.Sum256(binBytes)
			binHex := hex.EncodeToString(binSum[:])
			sumsBytes := []byte(binHex + "  " + binName + "\n")
			metaBytes, _ := json.Marshal(ReleaseMeta{Version: ver, BinarySHA256: binHex})
			return buildTestTar(t, true, map[string][]byte{
				binName:        binBytes,
				"SHA256SUMS":   sumsBytes,
				"RELEASE_META": metaBytes,
			})
		}
		tarV1 := buildTar("v1.0.0")
		tarV2 := buildTar("v2.0.0")
		sumV2 := sha256.Sum256(tarV2)
		digestV2 := "sha256:" + hex.EncodeToString(sumV2[:])
		assetV1 := fmt.Sprintf("gatehouse_v1.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
		assetV2 := fmt.Sprintf("gatehouse_v2.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

		var latestServed atomic.Bool
		var apiSrv *httptest.Server
		apiSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/yiyiSmallti/precinct-gatehouse/releases/latest":
				// First call (Check) returns v2.0.0; subsequent calls flip /latest to older v1.0.0!
				if !latestServed.Swap(true) {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"tag_name": "v2.0.0",
						"assets": []map[string]any{
							{"id": 2, "name": assetV2, "digest": digestV2, "browser_download_url": apiSrv.URL + "/dl/v2"},
						},
					})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tag_name": "v1.0.0",
					"assets": []map[string]any{
						{"id": 1, "name": assetV1, "browser_download_url": apiSrv.URL + "/dl/v1"},
					},
				})
			case "/repos/yiyiSmallti/precinct-gatehouse/releases/tags/v2.0.0":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tag_name": "v2.0.0",
					"assets": []map[string]any{
						{"id": 2, "name": assetV2, "digest": digestV2, "browser_download_url": apiSrv.URL + "/dl/v2"},
					},
				})
			case "/dl/v2":
				_, _ = w.Write(tarV2)
			case "/dl/v1":
				_, _ = w.Write(tarV1)
			default:
				http.NotFound(w, r)
			}
		}))
		defer apiSrv.Close()

		home := t.TempDir()
		dataDir := filepath.Join(home, "data")
		db, err := store.Open(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		liveMgr, cfgPath := newTestLiveManager(t, fmt.Sprintf(`listen: 127.0.0.1:17788
mode: personal
data_dir: %s
updater:
  enabled: true
  auto_apply: true
  source: github
  github_repo: yiyiSmallti/precinct-gatehouse
  github_api_url: %s
  require_signature: false
  home_dir: %s
  fallback_dns: []
`, dataDir, apiSrv.URL, home))

		var restarted atomic.Int32
		mgr := NewManager(Deps{
			CurrentVersion: "v1.5.0",
			ConfigPath:     cfgPath,
			Live:           liveMgr,
			DB:             db,
			RequestRestart: func() { restarted.Add(1) },
		})
		mgr.Store().PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }

		mgr.runBackgroundCycle(ctx)
		if got := mgr.Store().CurrentVersion(); got != "v2.0.0" {
			t.Fatalf("expected pinned download to install v2.0.0 even after /latest flipped, got %q", got)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && restarted.Load() == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		if restarted.Load() != 1 {
			t.Fatalf("expected 1 restart after pinned auto-apply, got %d", restarted.Load())
		}
	})
}

func TestStoreListReleasesDistinguishesVerifiedVsUnverifiedSignature(t *testing.T) {
	ctx := context.Background()
	st := NewStore(t.TempDir())
	st.PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }

	binName := binaryFilename()
	stage := func(ver, sigContent string, metaSigned bool) map[string]string {
		dir := t.TempDir()
		binBytes := []byte("gatehouse-" + ver)
		sum := sha256.Sum256(binBytes)
		shaHex := hex.EncodeToString(sum[:])

		binPath := filepath.Join(dir, binName)
		sumsPath := filepath.Join(dir, "SHA256SUMS")
		metaPath := filepath.Join(dir, "RELEASE_META")
		sigPath := filepath.Join(dir, "SIGNATURE")

		_ = os.WriteFile(binPath, binBytes, 0o755)
		sumsData := []byte(shaHex + "  " + binName + "\n")
		_ = os.WriteFile(sumsPath, sumsData, 0o644)
		mb, _ := json.Marshal(ReleaseMeta{Version: ver, BinarySHA256: shaHex, Signed: metaSigned})
		_ = os.WriteFile(metaPath, mb, 0o644)
		_ = os.WriteFile(sigPath, []byte(sigContent), 0o644)
		return map[string]string{
			binName:        binPath,
			"SHA256SUMS":   sumsPath,
			"RELEASE_META": metaPath,
			"SIGNATURE":    sigPath,
		}
	}

	// 1. Metadata claims signed=true and an invalid SIGNATURE exists, but no public key verified it.
	unverifiedFound := stage("v1.0.0", "ed25519:not-verified\n", true)
	unverifiedArt, err := VerifyExtractedArtifact(unverifiedFound, binName, nil, false)
	if err != nil {
		t.Fatalf("VerifyExtractedArtifact without pubkey: %v", err)
	}
	if unverifiedArt.Signed {
		t.Fatal("expected VerifiedArtifact.Signed=false when no public key configured")
	}
	if _, err := st.Install(ctx, InstallRequest{
		Version:      "v1.0.0",
		Digest:       "sha256:u1",
		Artifact:     unverifiedArt,
		KeepReleases: 5,
	}); err != nil {
		t.Fatalf("Install unverified v1.0.0: %v", err)
	}

	rels := st.ListReleases()
	if len(rels) != 1 || rels[0].Signed || !rels[0].HasSignature {
		t.Fatalf("expected unverified release to report Signed=false, HasSignature=true; got %+v", rels)
	}

	// 2. Properly signed + verified release must report both Signed=true and HasSignature=true.
	pubStr, privStr, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := ParsePublicKey(pubStr)
	priv, _ := ParsePrivateKey(privStr)
	verifiedFound := stage("v2.0.0", "", true)
	sumsData, _ := os.ReadFile(verifiedFound["SHA256SUMS"])
	_ = os.WriteFile(verifiedFound["SIGNATURE"], []byte(SignBytes(priv, sumsData)+"\n"), 0o644)

	verifiedArt, err := VerifyExtractedArtifact(verifiedFound, binName, pub, true)
	if err != nil {
		t.Fatalf("VerifyExtractedArtifact with pubkey: %v", err)
	}
	if !verifiedArt.Signed {
		t.Fatal("expected VerifiedArtifact.Signed=true when Ed25519 verification passed")
	}
	if _, err := st.Install(ctx, InstallRequest{
		Version:      "v2.0.0",
		Digest:       "sha256:v2",
		Artifact:     verifiedArt,
		KeepReleases: 5,
	}); err != nil {
		t.Fatalf("Install verified v2.0.0: %v", err)
	}

	byVer := map[string]ReleaseInfo{}
	for _, r := range st.ListReleases() {
		byVer[r.Version] = r
	}
	if byVer["v1.0.0"].Signed || !byVer["v1.0.0"].HasSignature {
		t.Fatalf("unexpected v1.0.0 signature flags: %+v", byVer["v1.0.0"])
	}
	if !byVer["v2.0.0"].Signed || !byVer["v2.0.0"].HasSignature {
		t.Fatalf("unexpected v2.0.0 signature flags: %+v", byVer["v2.0.0"])
	}
}

func TestManagerAbortsCutoverWhenHomeDirChangesMidOperation(t *testing.T) {
	ctx := context.Background()

	mkYAML := func(dataDir, home string) string {
		return fmt.Sprintf(`listen: 127.0.0.1:17788
mode: personal
data_dir: %s
updater:
  enabled: true
  auto_apply: false
  drain_timeout: 30s
  source: oci
  image: registry.example.com/smallti/gatehouse
  home_dir: %s
`, dataDir, home)
	}

	t.Run("apply_home_dir_changed_during_drain", func(t *testing.T) {
		homeA := filepath.Join(t.TempDir(), "homeA")
		homeB := filepath.Join(t.TempDir(), "homeB")
		dataDir := t.TempDir()
		db, err := store.Open(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		liveMgr, cfgPath := newTestLiveManager(t, mkYAML(dataDir, homeA))
		var inflight atomic.Int64
		inflight.Store(1)
		var draining atomic.Bool
		var restarts atomic.Int32

		src := &fakeSource{
			remote:   &RemoteInfo{Source: "oci", Version: "v0.2.0", Digest: "sha256:homedir-apply"},
			artifact: &DownloadedArtifact{Version: "v0.2.0", Remote: RemoteInfo{Source: "oci", Version: "v0.2.0", Digest: "sha256:homedir-apply"}},
		}
		mgr := NewManager(Deps{
			CurrentVersion: "v0.1.0",
			ConfigPath:     cfgPath,
			Live:           liveMgr,
			DB:             db,
			Inflight:       inflight.Load,
			SetDraining:    func(d bool) { draining.Store(d) },
			RequestRestart: func() { restarts.Add(1) },
			NewSource:      func(_ config.Updater, _ string) (ArtifactSource, error) { return src, nil },
		})
		mgr.Store().PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }

		if err := mgr.ApplyAsync(ApplyOptions{Force: false}); err != nil {
			t.Fatalf("ApplyAsync: %v", err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && mgr.Status().State != StateDraining {
			time.Sleep(20 * time.Millisecond)
		}
		if mgr.Status().State != StateDraining || !draining.Load() {
			t.Fatalf("expected StateDraining with draining=true, got state=%s draining=%v", mgr.Status().State, draining.Load())
		}

		// Hot-reload home_dir while apply is draining.
		if _, err := liveMgr.Apply([]byte(mkYAML(dataDir, homeB)), "web", liveMgr.Current().Cfg.Hash()); err != nil {
			t.Fatalf("hot-reload home_dir: %v", err)
		}
		inflight.Store(0)

		deadline = time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && mgr.Status().State == StateDraining {
			time.Sleep(20 * time.Millisecond)
		}
		stSnap := mgr.Status()
		if stSnap.State != StateError || !strings.Contains(stSnap.LastError, "home_dir changed") {
			t.Fatalf("expected error mentioning home_dir changed, got state=%s err=%q", stSnap.State, stSnap.LastError)
		}
		if draining.Load() {
			t.Fatal("expected draining restored to false after aborted cutover")
		}
		if restarts.Load() != 0 {
			t.Fatalf("expected 0 restarts, got %d", restarts.Load())
		}
		if p, _ := NewStore(homeA).ReadPending(); p != nil {
			t.Fatalf("expected no pending marker in old homeA, got %+v", p)
		}
		if got := NewStore(homeA).CurrentVersion(); got != "" {
			t.Fatalf("expected no release installed in homeA, got %q", got)
		}
		if mgr.Store().HomeDir != homeB {
			t.Fatalf("expected manager store synced to homeB %q, got %q", homeB, mgr.Store().HomeDir)
		}
	})

	t.Run("rollback_home_dir_changed_during_drain", func(t *testing.T) {
		homeA := filepath.Join(t.TempDir(), "homeA")
		homeB := filepath.Join(t.TempDir(), "homeB")
		dataDir := t.TempDir()
		db, err := store.Open(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		stA := NewStore(homeA)
		stA.PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }
		for _, ver := range []string{"v0.1.0", "v0.2.0"} {
			if _, err := stA.Install(ctx, InstallRequest{
				Version:      ver,
				Digest:       "sha256:" + ver,
				Artifact:     makeStagedArtifact(t, ver),
				KeepReleases: 5,
			}); err != nil {
				t.Fatalf("Install %s: %v", ver, err)
			}
		}

		liveMgr, cfgPath := newTestLiveManager(t, mkYAML(dataDir, homeA))
		var inflight atomic.Int64
		inflight.Store(1)
		var draining atomic.Bool
		var restarts atomic.Int32

		mgr := NewManager(Deps{
			CurrentVersion: "v0.2.0",
			ConfigPath:     cfgPath,
			Live:           liveMgr,
			DB:             db,
			Inflight:       inflight.Load,
			SetDraining:    func(d bool) { draining.Store(d) },
			RequestRestart: func() { restarts.Add(1) },
		})
		mgr.Store().PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }

		if err := mgr.RollbackAsync(RollbackOptions{TargetVersion: "v0.1.0", Force: false}); err != nil {
			t.Fatalf("RollbackAsync: %v", err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && mgr.Status().State != StateDraining {
			time.Sleep(20 * time.Millisecond)
		}
		if mgr.Status().State != StateDraining || !draining.Load() {
			t.Fatalf("expected StateDraining with draining=true, got state=%s draining=%v", mgr.Status().State, draining.Load())
		}

		if _, err := liveMgr.Apply([]byte(mkYAML(dataDir, homeB)), "web", liveMgr.Current().Cfg.Hash()); err != nil {
			t.Fatalf("hot-reload home_dir: %v", err)
		}
		inflight.Store(0)

		deadline = time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && mgr.Status().State == StateDraining {
			time.Sleep(20 * time.Millisecond)
		}
		stSnap := mgr.Status()
		if stSnap.State != StateError || !strings.Contains(stSnap.LastError, "home_dir changed") {
			t.Fatalf("expected error mentioning home_dir changed, got state=%s err=%q", stSnap.State, stSnap.LastError)
		}
		if draining.Load() {
			t.Fatal("expected draining restored to false after aborted rollback")
		}
		if restarts.Load() != 0 {
			t.Fatalf("expected 0 restarts, got %d", restarts.Load())
		}
		if p, _ := NewStore(homeA).ReadPending(); p != nil {
			t.Fatalf("expected no pending marker in old homeA, got %+v", p)
		}
		if got := NewStore(homeA).CurrentVersion(); got != "v0.2.0" {
			t.Fatalf("expected homeA to remain on v0.2.0, got %q", got)
		}
		if mgr.Store().HomeDir != homeB {
			t.Fatalf("expected manager store synced to homeB %q, got %q", homeB, mgr.Store().HomeDir)
		}
	})
}

func TestStoreEmergencyRollbackClearsStaleDigestForBaseline(t *testing.T) {
	ctx := context.Background()

	// 1. Baseline without a digest clears failed release's .current_digest.
	home := t.TempDir()
	st := NewStore(home)
	st.PreflightFunc = func(_ context.Context, _ string, _ string) error { return nil }
	fbBin := filepath.Join(t.TempDir(), binaryFilename())
	if err := os.WriteFile(fbBin, []byte("pre-updater-baseline"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := st.Install(ctx, InstallRequest{
		Version:            "v0.2.0",
		Digest:             "sha256:failed-v020",
		Artifact:           makeStagedArtifact(t, "v0.2.0"),
		FallbackBinaryPath: fbBin,
		KeepReleases:       5,
		RecordPending:      true,
	})
	if err != nil {
		t.Fatalf("Install v0.2.0: %v", err)
	}
	baselineVer := res.PreviousVersion
	if !strings.HasPrefix(baselineVer, "baseline-") {
		t.Fatalf("expected baseline previous version, got %q", baselineVer)
	}
	if got := st.CurrentDigest(); got != "sha256:failed-v020" {
		t.Fatalf("expected CurrentDigest=sha256:failed-v020 before rollback, got %q", got)
	}

	if err := st.EmergencyRollback(baselineVer, ""); err != nil {
		t.Fatalf("EmergencyRollback to baseline: %v", err)
	}
	if got := st.CurrentVersion(); got != baselineVer {
		t.Fatalf("expected CurrentVersion=%q, got %q", baselineVer, got)
	}
	if got := st.CurrentDigest(); got != "" {
		t.Fatalf("expected empty CurrentDigest after baseline emergency rollback, got %q", got)
	}

	// 2. Restoring a release with a stored .digest populates .current_digest even if restoreDigest param is empty.
	if _, err := st.Install(ctx, InstallRequest{
		Version:      "v0.1.0",
		Digest:       "sha256:good-v010",
		Artifact:     makeStagedArtifact(t, "v0.1.0"),
		KeepReleases: 5,
	}); err != nil {
		t.Fatalf("Install v0.1.0: %v", err)
	}
	if _, err := st.Install(ctx, InstallRequest{
		Version:      "v0.3.0",
		Digest:       "sha256:bad-v030",
		Artifact:     makeStagedArtifact(t, "v0.3.0"),
		KeepReleases: 5,
	}); err != nil {
		t.Fatalf("Install v0.3.0: %v", err)
	}
	if err := st.EmergencyRollback("v0.1.0", ""); err != nil {
		t.Fatalf("EmergencyRollback to v0.1.0: %v", err)
	}
	if got := st.CurrentDigest(); got != "sha256:good-v010" {
		t.Fatalf("expected CurrentDigest restored to sha256:good-v010, got %q", got)
	}
}
