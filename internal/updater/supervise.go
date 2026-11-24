package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
)

// SuperviseOptions configures RunSupervisor.
type SuperviseOptions struct {
	ConfigPath    string
	HomeDir       string
	BinaryPath    string
	Stdout        io.Writer
	Stderr        io.Writer
	Log           *slog.Logger
	ProbeInterval time.Duration
	HealthTimeout time.Duration // overrides config when > 0
}

// IsSupervised reports whether the current process is running under `gatehouse supervise`.
func IsSupervised() bool {
	return os.Getenv(EnvSupervised) == "1"
}

// RunSupervisor runs `<home>/bin/gatehouse serve --config <cfg>` in a managed
// child loop, forwards SIGHUP/SIGTERM/SIGINT, restarts immediately on ExitRestart
// (exit code 75), and automatically rolls back pending updates if health checks fail.
func RunSupervisor(ctx context.Context, opts SuperviseOptions) error {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	absPath := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return filepath.Clean(p)
	}
	configPath := strings.TrimSpace(opts.ConfigPath)
	if configPath != "" {
		configPath = absPath(config.ExpandHome(configPath))
	}
	optHome := strings.TrimSpace(opts.HomeDir)
	if optHome != "" {
		optHome = absPath(config.ExpandHome(optHome))
	}
	optBin := strings.TrimSpace(opts.BinaryPath)
	if optBin != "" {
		optBin = absPath(optBin)
	}
	exePath, _ := os.Executable()
	if exePath != "" {
		exePath = absPath(exePath)
	}
	resolveHome := func(prev string) string {
		if configPath != "" {
			cfg, err := config.Load(configPath)
			if err != nil {
				if prev != "" {
					log.Warn("supervisor failed loading config while resolving home_dir; keeping previous home", "err", err, "home", prev)
					return prev
				}
			} else if strings.TrimSpace(cfg.Updater.HomeDir) != "" {
				return absPath(ResolveHomeDir(cfg.Updater.HomeDir, configPath, exePath))
			}
		}
		if optHome != "" {
			return optHome
		}
		return absPath(ResolveHomeDir("", configPath, exePath))
	}

	probeInterval := opts.ProbeInterval
	if probeInterval <= 0 {
		probeInterval = 400 * time.Millisecond
	}

	hupCh := notifyReloadSignals()
	defer stopReloadSignals(hupCh)

	homeDir := ""
	backoff := time.Duration(0)
	for {
		if ctx.Err() != nil {
			return nil
		}

		homeDir = resolveHome(homeDir)
		store := NewStore(homeDir)

		binPath := optBin
		if binPath == "" {
			cand := store.BinLink()
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				binPath = cand
			} else if exePath != "" {
				binPath = exePath
			} else {
				binPath = cand
			}
		}

		pending, err := store.ReadPending()
		if err != nil {
			log.Warn("supervisor failed reading .update_pending", "err", err)
			pending = nil
		}

		args := []string{"serve"}
		if configPath != "" {
			args = append(args, "--config", configPath)
		}
		cmd := exec.Command(binPath, args...)
		env := make([]string, 0, len(os.Environ())+2)
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, EnvSupervised+"=") || strings.HasPrefix(kv, EnvHomeDir+"=") {
				continue
			}
			env = append(env, kv)
		}
		env = append(env, EnvSupervised+"=1")
		if optHome != "" {
			env = append(env, EnvHomeDir+"="+optHome)
		}
		cmd.Env = env
		if opts.Stdout != nil {
			cmd.Stdout = opts.Stdout
		} else {
			cmd.Stdout = os.Stdout
		}
		if opts.Stderr != nil {
			cmd.Stderr = opts.Stderr
		} else {
			cmd.Stderr = os.Stderr
		}
		if st, err := os.Stat(homeDir); err == nil && st.IsDir() {
			cmd.Dir = homeDir
		}

		startedAt := time.Now()
		if err := cmd.Start(); err != nil {
			if pending != nil && pending.PreviousVersion != "" && pending.PreviousVersion != pending.TargetVersion {
				log.Error("new release failed to start; rolling back", "target", pending.TargetVersion, "previous", pending.PreviousVersion, "err", err)
				var ok bool
				backoff, ok = handlePendingFailure(ctx, store, pending, fmt.Sprintf("start failed: %v", err), backoff, log)
				if !ok {
					return nil
				}
				continue
			}
			if pending != nil {
				_ = store.ClearPending()
			}
			log.Error("supervisor failed to start gatehouse", "bin", binPath, "err", err)
			backoff = nextBackoff(backoff)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			continue
		}

		log.Info("supervisor started gatehouse", "pid", cmd.Process.Pid, "bin", binPath, "pending_update", pending != nil)

		waitCh := make(chan error, 1)
		go func() {
			waitCh <- cmd.Wait()
		}()

		var (
			healthOK    bool
			healthErrCh chan error
			healthCtx   context.Context
			healthStop  context.CancelFunc
		)
		if pending != nil {
			dataURL, adminURL, hTimeout := probeTargets(configPath, opts.HealthTimeout)
			healthCtx, healthStop = context.WithCancel(ctx)
			healthErrCh = make(chan error, 1)
			go func() {
				healthErrCh <- waitForHealth(healthCtx, dataURL, adminURL, hTimeout, probeInterval)
			}()
		}

		var exitErr error
		var childExited bool
		var rollbackReason string
		stopping := false

	monitorLoop:
		for {
			select {
			case <-ctx.Done():
				if !stopping {
					stopping = true
					terminateProcess(cmd.Process)
				}
				if healthStop != nil {
					healthStop()
				}
				select {
				case exitErr = <-waitCh:
					childExited = true
				case <-time.After(15 * time.Second):
					_ = cmd.Process.Kill()
					exitErr = <-waitCh
					childExited = true
				}
				break monitorLoop

			case sig := <-hupCh:
				forwardSignal(cmd.Process, sig)

			case herr := <-healthErrCh:
				healthErrCh = nil
				if herr == nil {
					healthOK = true
					_ = store.ClearPending()
					log.Info("supervisor confirmed updated release healthy", "version", pending.TargetVersion)
				} else {
					rollbackReason = herr.Error()
					break monitorLoop
				}

			case exitErr = <-waitCh:
				childExited = true
				if pending != nil && !healthOK {
					rollbackReason = fmt.Sprintf("child exited before passing health check: %v", exitErr)
				}
				break monitorLoop
			}
		}

		if healthStop != nil {
			healthStop()
		}

		if ctx.Err() != nil {
			return nil
		}

		if pending != nil && !healthOK && rollbackReason != "" {
			if !childExited {
				terminateProcess(cmd.Process)
				select {
				case <-waitCh:
				case <-time.After(5 * time.Second):
					_ = cmd.Process.Kill()
					<-waitCh
				}
			}
			log.Error("updated release failed verification; initiating automatic rollback",
				"target", pending.TargetVersion, "previous", pending.PreviousVersion, "reason", rollbackReason)
			var ok bool
			backoff, ok = handlePendingFailure(ctx, store, pending, rollbackReason, backoff, log)
			if !ok {
				return nil
			}
			continue
		}

		if exitCode(exitErr) == ExitRestart {
			log.Info("gatehouse exited for planned update restart")
			backoff = 0
			continue
		}

		if time.Since(startedAt) > 30*time.Second {
			backoff = 0
		}
		backoff = nextBackoff(backoff)
		log.Warn("gatehouse exited unexpectedly; restarting", "err", exitErr, "backoff", backoff)
		if !sleepCtx(ctx, backoff) {
			return nil
		}
	}
}

func handlePendingFailure(ctx context.Context, store *Store, pending *PendingUpdate, reason string, backoff time.Duration, log *slog.Logger) (time.Duration, bool) {
	if pending.PreviousVersion != "" && pending.PreviousVersion != pending.TargetVersion {
		if rerr := store.EmergencyRollback(pending.PreviousVersion, pending.PreviousDigest); rerr != nil {
			log.Error("automatic rollback failed; keeping .update_pending for retry",
				"target", pending.TargetVersion, "previous", pending.PreviousVersion, "err", rerr)
			next := nextBackoff(backoff)
			if !sleepCtx(ctx, next) {
				return next, false
			}
			return next, true
		}
		log.Info("automatically rolled back to previous release", "version", pending.PreviousVersion)
		_ = store.WriteFailed(&FailedUpdate{
			Version: pending.TargetVersion,
			Digest:  pending.TargetDigest,
			Reason:  reason,
			At:      time.Now().UTC(),
		})
		_ = store.ClearPending()
		return 0, true
	}
	_ = store.ClearPending()
	next := nextBackoff(backoff)
	if !sleepCtx(ctx, next) {
		return next, false
	}
	return next, true
}

func probeTargets(configPath string, overrideTimeout time.Duration) (dataURL, adminURL string, timeout time.Duration) {
	listen := "127.0.0.1:7788"
	adminListen := ""
	timeout = 25 * time.Second
	if cfg, err := config.Load(configPath); err == nil {
		if cfg.Listen != "" {
			listen = cfg.Listen
		}
		adminListen = cfg.Admin.Listen
		timeout = cfg.Updater.HealthTimeout.D(25 * time.Second)
	}
	if overrideTimeout > 0 {
		timeout = overrideTimeout
	}
	dataURL = probeURL(listen, "/healthz")
	if adminListen != "" {
		adminURL = probeURL(adminListen, "/admin/")
	}
	return dataURL, adminURL, timeout
}

func probeURL(listenAddr, path string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "http://127.0.0.1:7788" + path
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + path
}

func waitForHealth(ctx context.Context, dataURL, adminURL string, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	lastErr := errors.New("health check timed out")
	for {
		if err := checkEndpointHealth(ctx, client, dataURL, adminURL); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check timed out after %s: %w", timeout, lastErr)
		}
		if !sleepCtx(ctx, interval) {
			return ctx.Err()
		}
	}
}

func checkEndpointHealth(ctx context.Context, client *http.Client, dataURL, adminURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dataURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", dataURL, resp.StatusCode)
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("invalid healthz json: %w", err)
	}
	if payload.Status != "ok" && payload.Status != "degraded" {
		return fmt.Errorf("healthz status=%q", payload.Status)
	}

	if adminURL != "" {
		areq, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL, nil)
		if err != nil {
			return err
		}
		aresp, err := client.Do(areq)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(aresp.Body, 4096))
		aresp.Body.Close()
		if aresp.StatusCode != http.StatusOK && aresp.StatusCode != http.StatusMovedPermanently && aresp.StatusCode != http.StatusFound {
			return fmt.Errorf("%s returned HTTP %d", adminURL, aresp.StatusCode)
		}
	}
	return nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

func nextBackoff(cur time.Duration) time.Duration {
	switch {
	case cur <= 0:
		return time.Second
	case cur < 2*time.Second:
		return 2 * time.Second
	case cur < 5*time.Second:
		return 5 * time.Second
	case cur < 10*time.Second:
		return 10 * time.Second
	default:
		return 30 * time.Second
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
