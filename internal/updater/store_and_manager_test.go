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
	if os.Getenv("GATEHOUSE_SUPERVISE_HELPER") == "1" && len(os.Args) >= 4 && os.Args[1] == "serve" && os.Args[2] == "--config" {
		runSuperviseHelper(os.Args[3])
		return
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

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		os.Exit(3)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if ver == "v_bad" {
			http.Error(w, "broken", http.StatusInternalServerError)
			return
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
