//go:build !windows

package updater

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
)

func runHandoffHelper(cfgPath string) {
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

	if os.Getenv(EnvHandoff) != "1" {
		os.Exit(4)
	}
	hasFlag := false
	for _, a := range os.Args {
		if a == "--handoff" || a == "-handoff" {
			hasFlag = true
			break
		}
	}
	if !hasFlag {
		os.Exit(5)
	}

	hc, err := NewWorkerHandoffFromEnv()
	if err != nil {
		os.Exit(6)
	}
	defer hc.Close()

	dataMux := http.NewServeMux()
	dataMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
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
	dataMux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"version":%q,"pid":%d}`, ver, os.Getpid())
	})
	dataMux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "start:%s\n", ver)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		relFile := filepath.Join(home, "release_slow")
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(relFile); err == nil {
				break
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(15 * time.Millisecond):
			}
		}
		_, _ = fmt.Fprintf(w, "end:%s\n", ver)
	})

	admMux := http.NewServeMux()
	admMux.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "admin:%s:%d", ver, os.Getpid())
	})

	probeDataLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(7)
	}
	probeDataSrv := &http.Server{Handler: dataMux}
	go func() { _ = probeDataSrv.Serve(probeDataLn) }()

	var probeAdmLn net.Listener
	var probeAdmSrv *http.Server
	var probeAdmAddr string
	if cfg.Admin.Listen != "" {
		probeAdmLn, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(8)
		}
		probeAdmAddr = probeAdmLn.Addr().String()
		probeAdmSrv = &http.Server{Handler: admMux}
		go func() { _ = probeAdmSrv.Serve(probeAdmLn) }()
	}
	closeProbes := func() {
		_ = probeDataSrv.Close()
		_ = probeDataLn.Close()
		if probeAdmSrv != nil {
			_ = probeAdmSrv.Close()
		}
		if probeAdmLn != nil {
			_ = probeAdmLn.Close()
		}
	}

	if err := hc.SendEvent(HandoffEvent{
		Event:     "probe_ready",
		DataAddr:  probeDataLn.Addr().String(),
		AdminAddr: probeAdmAddr,
	}); err != nil {
		closeProbes()
		os.Exit(9)
	}

	var dataLn, admLn net.Listener
	for {
		cmd, ok := <-hc.Commands()
		if !ok || cmd.Cmd == "abort" || cmd.Cmd == "drain" {
			closeProbes()
			return
		}
		if cmd.Cmd == "activate" {
			dataLn, admLn, err = hc.RecvListeners(cfg.Admin.Listen != "")
			if err != nil {
				_ = hc.SendEvent(HandoffEvent{Event: "activated", Error: err.Error()})
				closeProbes()
				os.Exit(10)
			}
			break
		}
	}

	dataSrv := &http.Server{Handler: dataMux}
	go func() { _ = dataSrv.Serve(dataLn) }()
	var admSrv *http.Server
	if admLn != nil {
		admSrv = &http.Server{Handler: admMux}
		go func() { _ = admSrv.Serve(admLn) }()
	}
	_ = hc.SendEvent(HandoffEvent{Event: "activated"})
	closeProbes()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)

	trigFile := filepath.Join(home, "trigger_restart_"+ver)
	var sentRestart atomic.Bool
	tk := time.NewTicker(15 * time.Millisecond)
	defer tk.Stop()

	for {
		select {
		case <-sig:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = dataSrv.Shutdown(ctx)
			if admSrv != nil {
				_ = admSrv.Shutdown(ctx)
			}
			cancel()
			return
		case cmd, ok := <-hc.Commands():
			if !ok {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = dataSrv.Shutdown(ctx)
				if admSrv != nil {
					_ = admSrv.Shutdown(ctx)
				}
				cancel()
				return
			}
			switch cmd.Cmd {
			case "drain":
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if admSrv != nil {
					_ = admSrv.Shutdown(ctx)
				}
				_ = dataSrv.Shutdown(ctx)
				cancel()
				_ = os.WriteFile(filepath.Join(home, fmt.Sprintf("drained_%s_%d", ver, os.Getpid())), []byte("ok"), 0o644)
				return
			case "abort":
				_ = os.WriteFile(filepath.Join(home, fmt.Sprintf("aborted_%s_%d", ver, os.Getpid())), []byte("ok"), 0o644)
			}
		case <-tk.C:
			if !sentRestart.Load() {
				if _, err := os.Stat(trigFile); err == nil {
					sentRestart.Store(true)
					_ = os.Remove(trigFile)
					_ = hc.SendEvent(HandoffEvent{Event: "restart"})
				}
			}
		}
	}
}

func writeHandoffTestConfig(t *testing.T, dir, home, listenAddr, adminAddr string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.yaml")
	dataDir := filepath.Join(dir, "data")
	_ = os.MkdirAll(dataDir, 0o755)
	raw := fmt.Sprintf(`listen: %s
mode: personal
data_dir: %s
admin:
  listen: %s
updater:
  home_dir: %s
  health_timeout: 5s
providers:
  - name: p
    base_url: http://127.0.0.1:19999/v1
    models: [m1]
`, listenAddr, dataDir, adminAddr, home)
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

func installHelperRelease(t *testing.T, st *Store, ver string, setActive bool) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	relDir := filepath.Join(st.ReleasesDir(), ver)
	if err := os.MkdirAll(relDir, 0o755); err != nil {
		t.Fatalf("mkdir release %s: %v", ver, err)
	}
	// Symlink per-version release binary to test binary so os.Stat/EvalSymlinks distinguishes releases cleanly.
	relBin := filepath.Join(relDir, "gatehouse")
	_ = os.Remove(relBin)
	if err := os.Symlink(exe, relBin); err != nil {
		t.Fatalf("symlink %s -> %s: %v", relBin, exe, err)
	}
	if setActive {
		_ = os.MkdirAll(filepath.Dir(st.BinLink()), 0o755)
		_ = os.Remove(st.BinLink())
		if err := os.Symlink(filepath.Join("..", "releases", ver, "gatehouse"), st.BinLink()); err != nil {
			t.Fatalf("symlink binLink: %v", err)
		}
	}
}

func waitHTTPPing(t *testing.T, url, wantVer string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var out struct {
					Version string `json:"version"`
					Pid     int    `json:"pid"`
				}
				if json.Unmarshal(b, &out) == nil && (wantVer == "" || out.Version == wantVer) {
					return out.Pid, resp.StatusCode
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to return version %q", url, wantVer)
	return 0, 0
}

func TestSupervisorZeroDowntimeHandoffUpgrade(t *testing.T) {
	t.Setenv("GATEHOUSE_SUPERVISE_HELPER", "1")
	t.Setenv("GATEHOUSE_HANDOFF_HELPER", "1")

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	st := NewStore(home)
	installHelperRelease(t, st, "v1", true)
	installHelperRelease(t, st, "v2", false)

	listenAddr := freeTCPAddr(t)
	adminAddr := freeTCPAddr(t)
	cfgPath := writeHandoffTestConfig(t, dir, home, listenAddr, adminAddr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SuperviseOptions{
			ConfigPath:    cfgPath,
			HomeDir:       home,
			ProbeInterval: 40 * time.Millisecond,
			HealthTimeout: 5 * time.Second,
		})
	}()

	pidV1, _ := waitHTTPPing(t, "http://"+listenAddr+"/ping", "v1")

	// Start a long-lived slow in-flight request against v1 before upgrading.
	slowClient := &http.Client{Timeout: 10 * time.Second}
	slowResp, err := slowClient.Get("http://" + listenAddr + "/slow")
	if err != nil {
		t.Fatalf("GET /slow: %v", err)
	}
	slowReader := bufio.NewReader(slowResp.Body)
	firstLine, err := slowReader.ReadString('\n')
	if err != nil || firstLine != "start:v1\n" {
		t.Fatalf("expected 'start:v1\\n', got %q (err=%v)", firstLine, err)
	}
	slowDone := make(chan error, 1)
	var slowRest bytes.Buffer
	go func() {
		defer slowResp.Body.Close()
		_, rerr := io.Copy(&slowRest, slowReader)
		slowDone <- rerr
	}()

	// Hammer /ping and /admin/ continuously during the entire upgrade transition.
	hammerStop := make(chan struct{})
	var hammerErr atomic.Value
	var reqCount atomic.Int64
	go func() {
		cl := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		for {
			select {
			case <-hammerStop:
				return
			default:
			}
			for _, u := range []string{"http://" + listenAddr + "/ping", "http://" + adminAddr + "/admin/"} {
				r, err := cl.Get(u)
				if err != nil {
					hammerErr.CompareAndSwap(nil, fmt.Sprintf("%s: %v", u, err))
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				r.Body.Close()
				if r.StatusCode != http.StatusOK {
					hammerErr.CompareAndSwap(nil, fmt.Sprintf("%s: status %d", u, r.StatusCode))
					return
				}
				reqCount.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// Atomically switch symlink to v2, write .update_pending, and trigger handoff restart in v1.
	tmpLink := st.BinLink() + ".tmp"
	if err := os.Symlink(filepath.Join("..", "releases", "v2", "gatehouse"), tmpLink); err != nil {
		t.Fatalf("tmp symlink: %v", err)
	}
	if err := os.Rename(tmpLink, st.BinLink()); err != nil {
		t.Fatalf("rename symlink: %v", err)
	}
	if err := st.WritePending(&PendingUpdate{
		TargetVersion:   "v2",
		PreviousVersion: "v1",
		RequestedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "trigger_restart_v1"), []byte("1"), 0o644); err != nil {
		t.Fatalf("trigger restart file: %v", err)
	}

	pidV2, _ := waitHTTPPing(t, "http://"+listenAddr+"/ping", "v2")
	if pidV2 == pidV1 {
		t.Fatalf("expected new PID for v2, still saw %d", pidV1)
	}

	// Now release slow request held inside draining v1, and confirm it finishes cleanly!
	if err := os.WriteFile(filepath.Join(home, "release_slow"), []byte("1"), 0o644); err != nil {
		t.Fatalf("release_slow: %v", err)
	}
	select {
	case err := <-slowDone:
		if err != nil {
			t.Fatalf("in-flight slow request on v1 failed during cutover: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for in-flight slow request on v1 to finish")
	}
	if got := slowRest.String(); got != "end:v1\n" {
		t.Fatalf("unexpected slow tail %q, want 'end:v1\\n'", got)
	}

	close(hammerStop)
	if v := hammerErr.Load(); v != nil {
		t.Fatalf("zero-downtime check failed: %v (completed=%d)", v, reqCount.Load())
	}
	if reqCount.Load() < 10 {
		t.Fatalf("expected concurrent traffic during handoff, got only %d requests", reqCount.Load())
	}

	// Verify old v1 worker exited gracefully after draining.
	drainedMarker := filepath.Join(home, fmt.Sprintf("drained_v1_%d", pidV1))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(drainedMarker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected old worker v1 (pid %d) to finish draining and exit", pidV1)
		}
		time.Sleep(25 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("RunSupervisor returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunSupervisor did not exit after cancel")
	}
}

func TestSupervisorZeroDowntimeHandoffRollbackKeepsOldServing(t *testing.T) {
	t.Setenv("GATEHOUSE_SUPERVISE_HELPER", "1")
	t.Setenv("GATEHOUSE_HANDOFF_HELPER", "1")

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	st := NewStore(home)
	installHelperRelease(t, st, "v1", true)
	installHelperRelease(t, st, "v_bad", false)

	listenAddr := freeTCPAddr(t)
	adminAddr := freeTCPAddr(t)
	cfgPath := writeHandoffTestConfig(t, dir, home, listenAddr, adminAddr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, SuperviseOptions{
			ConfigPath:    cfgPath,
			HomeDir:       home,
			ProbeInterval: 40 * time.Millisecond,
			HealthTimeout: 350 * time.Millisecond,
		})
	}()

	pidV1, _ := waitHTTPPing(t, "http://"+listenAddr+"/ping", "v1")

	hammerStop := make(chan struct{})
	var hammerErr atomic.Value
	go func() {
		cl := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		for {
			select {
			case <-hammerStop:
				return
			default:
			}
			for _, u := range []string{"http://" + listenAddr + "/ping", "http://" + adminAddr + "/admin/"} {
				r, err := cl.Get(u)
				if err != nil {
					hammerErr.CompareAndSwap(nil, fmt.Sprintf("%s: %v", u, err))
					return
				}
				b, _ := io.ReadAll(r.Body)
				r.Body.Close()
				if r.StatusCode != http.StatusOK {
					hammerErr.CompareAndSwap(nil, fmt.Sprintf("%s: status %d", u, r.StatusCode))
					return
				}
				if strings.HasSuffix(u, "/ping") && !strings.Contains(string(b), `"v1"`) {
					hammerErr.CompareAndSwap(nil, fmt.Sprintf("expected v1 continuously on rollback, got %s", string(b)))
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Switch symlink to broken v_bad and request handoff upgrade.
	tmpLink := st.BinLink() + ".tmp"
	if err := os.Symlink(filepath.Join("..", "releases", "v_bad", "gatehouse"), tmpLink); err != nil {
		t.Fatalf("tmp symlink: %v", err)
	}
	if err := os.Rename(tmpLink, st.BinLink()); err != nil {
		t.Fatalf("rename symlink: %v", err)
	}
	if err := st.WritePending(&PendingUpdate{
		TargetVersion:   "v_bad",
		TargetDigest:    "sha256:bad",
		PreviousVersion: "v1",
		RequestedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "trigger_restart_v1"), []byte("1"), 0o644); err != nil {
		t.Fatalf("trigger restart file: %v", err)
	}

	// Wait until rollback finishes: .failed_update written and v1 received "abort".
	abortMarker := filepath.Join(home, fmt.Sprintf("aborted_v1_%d", pidV1))
	deadline := time.Now().Add(8 * time.Second)
	for {
		failed, _ := st.ReadFailed()
		_, abortErr := os.Stat(abortMarker)
		if failed != nil && failed.Version == "v_bad" && abortErr == nil && st.CurrentVersion() == "v1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for zero-downtime rollback (cur=%s, failed=%+v, abortErr=%v)", st.CurrentVersion(), failed, abortErr)
		}
		time.Sleep(30 * time.Millisecond)
	}

	close(hammerStop)
	if v := hammerErr.Load(); v != nil {
		t.Fatalf("old worker experienced downtime or wrong version during failed upgrade rollback: %v", v)
	}
	pidAfter, _ := waitHTTPPing(t, "http://"+listenAddr+"/ping", "v1")
	if pidAfter != pidV1 {
		t.Fatalf("expected active v1 worker PID %d to stay unchanged across failed upgrade, got %d", pidV1, pidAfter)
	}

	cancel()
	<-done
}
