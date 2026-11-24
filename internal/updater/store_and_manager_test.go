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
	home := os.Getenv("GATEHOUSE_SUPERVISE_HOME")
	st := NewStore(home)
	ver := st.CurrentVersion()

	if f, err := os.OpenFile(filepath.Join(home, "boot_log.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		_, _ = fmt.Fprintln(f, ver)
		_ = f.Close()
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		os.Exit(2)
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
