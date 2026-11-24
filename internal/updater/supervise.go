package updater

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	"sync"
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
// child loop, forwards SIGHUP/SIGTERM/SIGINT, performs zero-downtime FD handoff
// upgrades when supported by the target binary (with legacy restart fallback),
// and automatically rolls back pending updates if health checks fail.
func RunSupervisor(ctx context.Context, opts SuperviseOptions) error {
	sess := newSuperviseSession(ctx, opts)
	defer sess.shutdownAll(nil)

	hupCh := notifyReloadSignals()
	defer stopReloadSignals(hupCh)

	var active *supervisedWorker
	for {
		if ctx.Err() != nil {
			sess.shutdownAll(active)
			return nil
		}

		if active != nil {
			restartRequested := sess.monitorActiveHandoff(active, hupCh)
			if ctx.Err() != nil {
				sess.shutdownAll(active)
				return nil
			}
			if !restartRequested {
				active = nil
				continue
			}
		}

		spec, st := sess.resolveSpec()
		if spec.handoff {
			nextActive, done := sess.spawnAndActivateHandoff(spec, st, hupCh, &active)
			if done || ctx.Err() != nil {
				sess.shutdownAll(nextActive)
				return nil
			}
			active = nextActive
			continue
		}

		if active != nil {
			// Rolling back to a legacy binary without --handoff, or config changed
			// to disable handoff: drain and stop current handoff worker first to free ports.
			sess.log.Info("target release does not support --handoff; draining active worker for legacy restart")
			sess.retireWorkerSync(active)
			active = nil
			sess.closeAllSockets()
		}

		if done := sess.runLegacyWorker(spec, st, hupCh); done {
			return nil
		}
	}
}

type workerSpec struct {
	homeDir       string
	binPath       string
	pending       *PendingUpdate
	handoff       bool
	listenAddr    string
	adminAddr     string
	healthTimeout time.Duration
}

type supervisedWorker struct {
	cmd       *exec.Cmd
	waitCh    chan error
	sh        *supervisorHandoff
	startedAt time.Time
	binPath   string
	dataAddr  string
	adminAddr string
}

type superviseSession struct {
	ctx           context.Context
	configPath    string
	optHome       string
	optBin        string
	exePath       string
	workDir       string
	stdout        io.Writer
	stderr        io.Writer
	log           *slog.Logger
	probeInterval time.Duration
	healthTimeout time.Duration

	homeDir   string
	backoff   time.Duration
	dataSock  *handoffSocket
	adminSock *handoffSocket

	drainingMu    sync.Mutex
	drainingProcs map[int]*os.Process
	drainingWG    sync.WaitGroup
}

func newSuperviseSession(ctx context.Context, opts SuperviseOptions) *superviseSession {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	configPath := strings.TrimSpace(opts.ConfigPath)
	if configPath != "" {
		configPath = absCleanPath(config.ExpandHome(configPath))
	}
	optHome := strings.TrimSpace(opts.HomeDir)
	if optHome != "" {
		optHome = absCleanPath(config.ExpandHome(optHome))
	}
	optBin := strings.TrimSpace(opts.BinaryPath)
	if optBin != "" {
		optBin = absCleanPath(optBin)
	}
	exePath, _ := os.Executable()
	if exePath != "" {
		exePath = absCleanPath(exePath)
	}
	workDir, _ := os.Getwd()
	if workDir != "" {
		workDir = absCleanPath(workDir)
	}
	probeInterval := opts.ProbeInterval
	if probeInterval <= 0 {
		probeInterval = 400 * time.Millisecond
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	return &superviseSession{
		ctx:           ctx,
		configPath:    configPath,
		optHome:       optHome,
		optBin:        optBin,
		exePath:       exePath,
		workDir:       workDir,
		stdout:        stdout,
		stderr:        stderr,
		log:           log,
		probeInterval: probeInterval,
		healthTimeout: opts.HealthTimeout,
		drainingProcs: make(map[int]*os.Process),
	}
}

func absCleanPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

func (s *superviseSession) resolveHome() string {
	if s.configPath != "" {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			if s.homeDir != "" {
				s.log.Warn("supervisor failed loading config while resolving home_dir; keeping previous home", "err", err, "home", s.homeDir)
				return s.homeDir
			}
		} else if strings.TrimSpace(cfg.Updater.HomeDir) != "" {
			return absCleanPath(ResolveHomeDir(cfg.Updater.HomeDir, s.configPath, s.exePath))
		}
	}
	if s.optHome != "" {
		return s.optHome
	}
	return absCleanPath(ResolveHomeDir("", s.configPath, s.exePath))
}

func (s *superviseSession) resolveSpec() (workerSpec, *Store) {
	s.homeDir = s.resolveHome()
	st := NewStore(s.homeDir)

	binPath := s.optBin
	if binPath == "" {
		cand := st.BinLink()
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			binPath = cand
		} else if s.exePath != "" {
			binPath = s.exePath
		} else {
			binPath = cand
		}
	}

	pending, err := st.ReadPending()
	if err != nil {
		s.log.Warn("supervisor failed reading .update_pending", "err", err)
		pending = nil
	}

	spec := workerSpec{
		homeDir:       s.homeDir,
		binPath:       binPath,
		pending:       pending,
		healthTimeout: 25 * time.Second,
	}
	if cfg, cfgErr := config.Load(s.configPath); cfgErr == nil {
		spec.listenAddr = strings.TrimSpace(cfg.Listen)
		spec.adminAddr = strings.TrimSpace(cfg.Admin.Listen)
		spec.healthTimeout = cfg.Updater.HealthTimeout.D(25 * time.Second)
		if s.healthTimeout > 0 {
			spec.healthTimeout = s.healthTimeout
		}
		if spec.listenAddr != "" && supportsHandoff(binPath) {
			spec.handoff = true
		}
	} else if s.healthTimeout > 0 {
		spec.healthTimeout = s.healthTimeout
	}
	return spec, st
}

func (s *superviseSession) buildBaseCmd(binPath, bootNonce string) *exec.Cmd {
	args := []string{"serve"}
	if s.configPath != "" {
		args = append(args, "--config", s.configPath)
	}
	cmd := exec.Command(binPath, args...)
	env := make([]string, 0, len(os.Environ())+8)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, EnvSupervised+"=") ||
			strings.HasPrefix(kv, EnvHomeDir+"=") ||
			strings.HasPrefix(kv, EnvBootNonce+"=") ||
			strings.HasPrefix(kv, EnvHandoff+"=") ||
			strings.HasPrefix(kv, EnvFDSockFD+"=") ||
			strings.HasPrefix(kv, EnvListenFD+"=") ||
			strings.HasPrefix(kv, EnvListenAddr+"=") ||
			strings.HasPrefix(kv, EnvAdminFD+"=") ||
			strings.HasPrefix(kv, EnvAdminAddr+"=") ||
			strings.HasPrefix(kv, EnvCtrlWriteFD+"=") ||
			strings.HasPrefix(kv, EnvCmdReadFD+"=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, EnvSupervised+"=1")
	if s.optHome != "" {
		env = append(env, EnvHomeDir+"="+s.optHome)
	}
	if bootNonce != "" {
		env = append(env, EnvBootNonce+"="+bootNonce)
	}
	cmd.Env = env
	cmd.Stdout = s.stdout
	cmd.Stderr = s.stderr
	if s.workDir != "" {
		cmd.Dir = s.workDir
	}
	return cmd
}

func (s *superviseSession) closeAllSockets() {
	if s.dataSock != nil {
		_ = s.dataSock.Close()
		s.dataSock = nil
	}
	if s.adminSock != nil {
		_ = s.adminSock.Close()
		s.adminSock = nil
	}
}

func (s *superviseSession) ensureSockets(listenAddr, adminAddr string, activePtr **supervisedWorker) (nextData, nextAdmin *handoffSocket, obsolete []*handoffSocket, err error) {
	if s.dataSock != nil && s.dataSock.Addr() == listenAddr {
		nextData = s.dataSock
	} else {
		if s.dataSock != nil {
			obsolete = append(obsolete, s.dataSock)
		}
		nextData, err = listenHandoffSocket(listenAddr)
		if err != nil && isAddrInUse(err) && activePtr != nil && *activePtr != nil {
			s.log.Info("listen address changed and conflicts with active worker; draining active worker before rebinding", "addr", listenAddr)
			s.retireWorkerSync(*activePtr)
			*activePtr = nil
			s.closeAllSockets()
			obsolete = nil
			nextData, err = listenHandoffSocket(listenAddr)
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("bind listen %s: %w", listenAddr, err)
		}
	}

	if adminAddr == "" {
		if s.adminSock != nil {
			obsolete = append(obsolete, s.adminSock)
		}
		return nextData, nil, obsolete, nil
	}

	if s.adminSock != nil && s.adminSock.Addr() == adminAddr {
		nextAdmin = s.adminSock
	} else {
		if s.adminSock != nil {
			obsolete = append(obsolete, s.adminSock)
		}
		nextAdmin, err = listenHandoffSocket(adminAddr)
		if err != nil && isAddrInUse(err) && activePtr != nil && *activePtr != nil {
			s.log.Info("admin listen address changed and conflicts with active worker; draining active worker before rebinding", "addr", adminAddr)
			s.retireWorkerSync(*activePtr)
			*activePtr = nil
			s.closeAllSockets()
			obsolete = nil
			nextData, err = listenHandoffSocket(listenAddr)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("rebind listen %s: %w", listenAddr, err)
			}
			nextAdmin, err = listenHandoffSocket(adminAddr)
		}
		if err != nil {
			if nextData != s.dataSock {
				_ = nextData.Close()
			}
			return nil, nil, nil, fmt.Errorf("bind admin listen %s: %w", adminAddr, err)
		}
	}
	return nextData, nextAdmin, obsolete, nil
}

func stopAndWaitProcess(proc *os.Process, waitCh chan error, grace time.Duration) {
	if proc == nil {
		return
	}
	terminateProcess(proc)
	select {
	case <-waitCh:
	case <-time.After(grace):
		_ = proc.Kill()
		<-waitCh
	}
}

func (s *superviseSession) retireWorkerSync(w *supervisedWorker) {
	if w == nil {
		return
	}
	if w.sh != nil {
		_ = w.sh.SendCmd("drain")
		defer w.sh.Close()
	}
	select {
	case <-w.waitCh:
	case <-time.After(15 * time.Second):
		if w.cmd != nil && w.cmd.Process != nil {
			stopAndWaitProcess(w.cmd.Process, w.waitCh, 5*time.Second)
		}
	}
}

func (s *superviseSession) retireWorkerAsync(w *supervisedWorker, obsolete ...*handoffSocket) {
	if w == nil {
		for _, sock := range obsolete {
			_ = sock.Close()
		}
		return
	}
	pid := 0
	var proc *os.Process
	if w.cmd != nil && w.cmd.Process != nil {
		pid = w.cmd.Process.Pid
		proc = w.cmd.Process
		s.drainingMu.Lock()
		s.drainingProcs[pid] = proc
		s.drainingMu.Unlock()
	}
	if w.sh != nil {
		if err := w.sh.SendCmd("drain"); err != nil {
			s.log.Warn("failed sending drain to replaced worker; terminating it", "pid", pid, "err", err)
			terminateProcess(proc)
		}
	} else if proc != nil {
		terminateProcess(proc)
	}
	s.drainingWG.Add(1)
	go func() {
		defer s.drainingWG.Done()
		err := <-w.waitCh
		if w.sh != nil {
			w.sh.Close()
		}
		for _, sock := range obsolete {
			_ = sock.Close()
		}
		if pid != 0 {
			s.drainingMu.Lock()
			delete(s.drainingProcs, pid)
			s.drainingMu.Unlock()
		}
		if err != nil {
			s.log.Warn("draining worker exited with error", "pid", pid, "err", err)
		} else {
			s.log.Info("draining worker exited cleanly after handoff", "pid", pid)
		}
	}()
}

func (s *superviseSession) shutdownAll(active *supervisedWorker) {
	s.closeAllSockets()
	if active != nil && active.cmd != nil && active.cmd.Process != nil {
		stopAndWaitProcess(active.cmd.Process, active.waitCh, 15*time.Second)
		if active.sh != nil {
			active.sh.Close()
		}
	}
	s.drainingMu.Lock()
	for _, proc := range s.drainingProcs {
		terminateProcess(proc)
	}
	s.drainingMu.Unlock()

	doneCh := make(chan struct{})
	go func() {
		s.drainingWG.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(15 * time.Second):
		s.drainingMu.Lock()
		for _, proc := range s.drainingProcs {
			_ = proc.Kill()
		}
		s.drainingMu.Unlock()
		<-doneCh
	}
}

func (s *superviseSession) spawnAndActivateHandoff(spec workerSpec, st *Store, hupCh <-chan os.Signal, activePtr **supervisedWorker) (*supervisedWorker, bool) {
	nextData, nextAdmin, obsolete, err := s.ensureSockets(spec.listenAddr, spec.adminAddr, activePtr)
	if err != nil {
		s.log.Error("supervisor failed preparing handoff sockets", "err", err)
		if spec.pending != nil {
			nextBackoff, ok := handlePendingFailure(s.ctx, st, spec.pending, err.Error(), s.backoff, s.log)
			s.backoff = nextBackoff
			if !ok {
				return *activePtr, true
			}
			if *activePtr != nil && (*activePtr).sh != nil {
				_ = (*activePtr).sh.SendCmd("abort")
			}
			return *activePtr, false
		}
		s.backoff = nextBackoff(s.backoff)
		if !sleepCtx(s.ctx, s.backoff) {
			return *activePtr, true
		}
		return *activePtr, false
	}

	cleanupNewSockets := func() {
		if nextData != nil && nextData != s.dataSock {
			_ = nextData.Close()
		}
		if nextAdmin != nil && nextAdmin != s.adminSock {
			_ = nextAdmin.Close()
		}
	}

	bootNonce := newBootNonce()
	cmd := s.buildBaseCmd(spec.binPath, bootNonce)
	sh, err := startSupervisorHandoff(cmd, spec.listenAddr, spec.adminAddr)
	if err != nil {
		cleanupNewSockets()
		s.log.Error("supervisor failed setting up handoff pipes", "err", err)
		s.backoff = nextBackoff(s.backoff)
		if !sleepCtx(s.ctx, s.backoff) {
			return *activePtr, true
		}
		return *activePtr, false
	}

	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		sh.afterStart(false)
		cleanupNewSockets()
		if spec.pending != nil && spec.pending.PreviousVersion != "" && spec.pending.PreviousVersion != spec.pending.TargetVersion {
			s.log.Error("new release failed to start; rolling back", "target", spec.pending.TargetVersion, "previous", spec.pending.PreviousVersion, "err", err)
			nextB, ok := handlePendingFailure(s.ctx, st, spec.pending, fmt.Sprintf("start failed: %v", err), s.backoff, s.log)
			s.backoff = nextB
			if !ok {
				return *activePtr, true
			}
			if *activePtr != nil && (*activePtr).sh != nil {
				_ = (*activePtr).sh.SendCmd("abort")
			}
			return *activePtr, false
		}
		if spec.pending != nil {
			_ = st.ClearPending()
		}
		s.log.Error("supervisor failed to start gatehouse", "bin", spec.binPath, "err", err)
		s.backoff = nextBackoff(s.backoff)
		if !sleepCtx(s.ctx, s.backoff) {
			return *activePtr, true
		}
		return *activePtr, false
	}
	sh.afterStart(true)
	s.log.Info("supervisor started gatehouse candidate", "pid", cmd.Process.Pid, "bin", spec.binPath, "pending_update", spec.pending != nil, "handoff", true)

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	cand := &supervisedWorker{
		cmd:       cmd,
		waitCh:    waitCh,
		sh:        sh,
		startedAt: startedAt,
		binPath:   spec.binPath,
		dataAddr:  spec.listenAddr,
		adminAddr: spec.adminAddr,
	}

	var activeWait chan error
	if *activePtr != nil {
		activeWait = (*activePtr).waitCh
	}

	failCandidate := func(candExited bool, reason string) (*supervisedWorker, bool) {
		if !candExited {
			stopAndWaitProcess(cand.cmd.Process, cand.waitCh, 5*time.Second)
		}
		cand.sh.Close()
		cleanupNewSockets()

		if spec.pending != nil {
			s.log.Error("updated release failed verification; initiating automatic rollback",
				"target", spec.pending.TargetVersion, "previous", spec.pending.PreviousVersion, "reason", reason)
			nextB, ok := handlePendingFailure(s.ctx, st, spec.pending, reason, s.backoff, s.log)
			s.backoff = nextB
			if !ok {
				return *activePtr, true
			}
			if *activePtr != nil && (*activePtr).sh != nil {
				_ = (*activePtr).sh.SendCmd("abort")
			}
			return *activePtr, false
		}

		s.log.Warn("handoff worker failed readiness; retrying", "reason", reason)
		if *activePtr != nil && (*activePtr).sh != nil {
			_ = (*activePtr).sh.SendCmd("abort")
			return *activePtr, false
		}
		s.backoff = nextBackoff(s.backoff)
		if !sleepCtx(s.ctx, s.backoff) {
			return nil, true
		}
		return nil, false
	}

	// Phase 1: wait for probe_ready.
	probeTimer := time.NewTimer(spec.healthTimeout)
	defer probeTimer.Stop()
	var probeEv HandoffEvent
probeWaitLoop:
	for {
		select {
		case <-s.ctx.Done():
			stopAndWaitProcess(cand.cmd.Process, cand.waitCh, 5*time.Second)
			cand.sh.Close()
			cleanupNewSockets()
			return *activePtr, true

		case sig := <-hupCh:
			if *activePtr != nil && (*activePtr).cmd != nil && (*activePtr).cmd.Process != nil {
				forwardSignal((*activePtr).cmd.Process, sig)
			}

		case err := <-activeWait:
			activeWait = nil
			if *activePtr != nil {
				(*activePtr).sh.Close()
				*activePtr = nil
			}
			s.log.Warn("active worker exited unexpectedly while replacement was starting", "err", err)

		case <-probeTimer.C:
			return failCandidate(false, "timed out waiting for candidate probe_ready")

		case exitErr := <-cand.waitCh:
			return failCandidate(true, fmt.Sprintf("child exited before probe_ready: %v", exitErr))

		case ev, ok := <-cand.sh.Events():
			if !ok {
				return failCandidate(false, "child closed control pipe before probe_ready")
			}
			if ev.Event == "probe_ready" {
				if ev.Error != "" {
					return failCandidate(false, ev.Error)
				}
				probeEv = ev
				break probeWaitLoop
			}
		}
	}

	// Phase 2: health verification against ephemeral probe endpoints.
	if probeEv.DataAddr == "" {
		return failCandidate(false, "candidate probe_ready missing data_addr")
	}
	probeDataURL := probeURL(probeEv.DataAddr, "/healthz")
	probeAdminURL := ""
	if spec.adminAddr != "" {
		if probeEv.AdminAddr == "" {
			return failCandidate(false, "candidate probe_ready missing admin_addr")
		}
		probeAdminURL = probeURL(probeEv.AdminAddr, "/admin/")
	}
	expect := healthExpectation{BootNonce: bootNonce, RequireBootAck: bootNonce != ""}
	if spec.pending != nil {
		expect = buildHealthExpectation(st, spec.pending, bootNonce)
	}

	healthCtx, healthStop := context.WithCancel(s.ctx)
	defer healthStop()
	healthErrCh := make(chan error, 1)
	go func() {
		healthErrCh <- waitForHealth(healthCtx, probeDataURL, probeAdminURL, spec.healthTimeout, s.probeInterval, expect)
	}()

	candExited := false
healthWaitLoop:
	for {
		select {
		case <-s.ctx.Done():
			healthStop()
			stopAndWaitProcess(cand.cmd.Process, cand.waitCh, 5*time.Second)
			cand.sh.Close()
			cleanupNewSockets()
			return *activePtr, true

		case sig := <-hupCh:
			if *activePtr != nil && (*activePtr).cmd != nil && (*activePtr).cmd.Process != nil {
				forwardSignal((*activePtr).cmd.Process, sig)
			}

		case err := <-activeWait:
			activeWait = nil
			if *activePtr != nil {
				(*activePtr).sh.Close()
				*activePtr = nil
			}
			s.log.Warn("active worker exited unexpectedly during candidate health verification", "err", err)

		case herr := <-healthErrCh:
			if herr == nil {
				break healthWaitLoop
			}
			return failCandidate(false, herr.Error())

		case exitErr := <-cand.waitCh:
			candExited = true
			healthStop()
			return failCandidate(candExited, fmt.Sprintf("child exited before passing health check: %v", exitErr))
		}
	}
	healthStop()

	// Phase 3: deliver live listener FDs + activate, then wait for "activated".
	if err := cand.sh.SendActivate(nextData, nextAdmin); err != nil {
		return failCandidate(false, fmt.Sprintf("send activate failed: %v", err))
	}

	actTimer := time.NewTimer(5 * time.Second)
	defer actTimer.Stop()
activateWaitLoop:
	for {
		select {
		case <-s.ctx.Done():
			stopAndWaitProcess(cand.cmd.Process, cand.waitCh, 5*time.Second)
			cand.sh.Close()
			cleanupNewSockets()
			return *activePtr, true

		case sig := <-hupCh:
			if *activePtr != nil && (*activePtr).cmd != nil && (*activePtr).cmd.Process != nil {
				forwardSignal((*activePtr).cmd.Process, sig)
			}

		case err := <-activeWait:
			activeWait = nil
			if *activePtr != nil {
				(*activePtr).sh.Close()
				*activePtr = nil
			}
			s.log.Warn("active worker exited during candidate activation", "err", err)

		case <-actTimer.C:
			return failCandidate(false, "timed out waiting for candidate activated ack")

		case exitErr := <-cand.waitCh:
			return failCandidate(true, fmt.Sprintf("child exited during activation: %v", exitErr))

		case ev, ok := <-cand.sh.Events():
			if !ok {
				return failCandidate(false, "child closed control pipe during activation")
			}
			if ev.Event == "activated" {
				if ev.Error != "" {
					return failCandidate(false, ev.Error)
				}
				break activateWaitLoop
			}
		}
	}

	if spec.pending != nil {
		_ = st.ClearPending()
		s.log.Info("supervisor confirmed updated release healthy (zero-downtime)", "version", spec.pending.TargetVersion)
	}
	s.dataSock = nextData
	s.adminSock = nextAdmin
	s.backoff = 0

	if *activePtr != nil {
		s.retireWorkerAsync(*activePtr, obsolete...)
	} else {
		for _, sock := range obsolete {
			_ = sock.Close()
		}
	}
	return cand, false
}

func (s *superviseSession) monitorActiveHandoff(w *supervisedWorker, hupCh <-chan os.Signal) bool {
	eventsCh := w.sh.Events()
	for {
		select {
		case <-s.ctx.Done():
			return false

		case sig := <-hupCh:
			if w.cmd != nil && w.cmd.Process != nil {
				forwardSignal(w.cmd.Process, sig)
			}

		case ev, ok := <-eventsCh:
			if !ok {
				eventsCh = nil
				continue
			}
			if ev.Event == "restart" {
				s.log.Info("gatehouse requested zero-downtime handoff restart", "pid", w.cmd.Process.Pid)
				return true
			}

		case exitErr := <-w.waitCh:
			w.sh.Close()
			if exitCode(exitErr) == ExitRestart {
				s.log.Info("gatehouse exited for planned update restart")
				s.backoff = 0
				return false
			}
			if time.Since(w.startedAt) > 30*time.Second {
				s.backoff = 0
			}
			s.backoff = nextBackoff(s.backoff)
			s.log.Warn("gatehouse exited unexpectedly; restarting", "err", exitErr, "backoff", s.backoff)
			sleepCtx(s.ctx, s.backoff)
			return false
		}
	}
}

func (s *superviseSession) runLegacyWorker(spec workerSpec, st *Store, hupCh <-chan os.Signal) bool {
	var bootNonce string
	if spec.pending != nil {
		bootNonce = newBootNonce()
	}
	cmd := s.buildBaseCmd(spec.binPath, bootNonce)

	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		if spec.pending != nil && spec.pending.PreviousVersion != "" && spec.pending.PreviousVersion != spec.pending.TargetVersion {
			s.log.Error("new release failed to start; rolling back", "target", spec.pending.TargetVersion, "previous", spec.pending.PreviousVersion, "err", err)
			nextB, ok := handlePendingFailure(s.ctx, st, spec.pending, fmt.Sprintf("start failed: %v", err), s.backoff, s.log)
			s.backoff = nextB
			return !ok
		}
		if spec.pending != nil {
			_ = st.ClearPending()
		}
		s.log.Error("supervisor failed to start gatehouse", "bin", spec.binPath, "err", err)
		s.backoff = nextBackoff(s.backoff)
		return !sleepCtx(s.ctx, s.backoff)
	}

	s.log.Info("supervisor started gatehouse", "pid", cmd.Process.Pid, "bin", spec.binPath, "pending_update", spec.pending != nil)

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var (
		healthOK    bool
		healthErrCh chan error
		healthStop  context.CancelFunc
	)
	if spec.pending != nil {
		dataURL, adminURL, hTimeout := probeTargets(s.configPath, s.healthTimeout)
		expect := buildHealthExpectation(st, spec.pending, bootNonce)
		var healthCtx context.Context
		healthCtx, healthStop = context.WithCancel(s.ctx)
		healthErrCh = make(chan error, 1)
		go func() {
			healthErrCh <- waitForHealth(healthCtx, dataURL, adminURL, hTimeout, s.probeInterval, expect)
		}()
	}

	var exitErr error
	var childExited bool
	var rollbackReason string
	stopping := false

monitorLoop:
	for {
		select {
		case <-s.ctx.Done():
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
				_ = st.ClearPending()
				s.log.Info("supervisor confirmed updated release healthy", "version", spec.pending.TargetVersion)
			} else {
				rollbackReason = herr.Error()
				break monitorLoop
			}

		case exitErr = <-waitCh:
			childExited = true
			if spec.pending != nil && !healthOK {
				rollbackReason = fmt.Sprintf("child exited before passing health check: %v", exitErr)
			}
			break monitorLoop
		}
	}

	if healthStop != nil {
		healthStop()
	}

	if s.ctx.Err() != nil {
		return true
	}

	if spec.pending != nil && !healthOK && rollbackReason != "" {
		if !childExited {
			stopAndWaitProcess(cmd.Process, waitCh, 5*time.Second)
		}
		s.log.Error("updated release failed verification; initiating automatic rollback",
			"target", spec.pending.TargetVersion, "previous", spec.pending.PreviousVersion, "reason", rollbackReason)
		nextB, ok := handlePendingFailure(s.ctx, st, spec.pending, rollbackReason, s.backoff, s.log)
		s.backoff = nextB
		return !ok
	}

	if exitCode(exitErr) == ExitRestart {
		s.log.Info("gatehouse exited for planned update restart")
		s.backoff = 0
		return false
	}

	if time.Since(startedAt) > 30*time.Second {
		s.backoff = 0
	}
	s.backoff = nextBackoff(s.backoff)
	s.log.Warn("gatehouse exited unexpectedly; restarting", "err", exitErr, "backoff", s.backoff)
	return !sleepCtx(s.ctx, s.backoff)
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

type healthExpectation struct {
	ExpectedVersion string
	RejectVersion   string
	BootNonce       string
	RequireBootAck  bool
}

// BootAck returns the expected X-Gatehouse-Boot-Ack header value for a boot nonce.
func BootAck(nonce string) string {
	sum := sha256.Sum256([]byte("gatehouse-boot-ack:" + nonce))
	return hex.EncodeToString(sum[:])
}

func newBootNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

func buildHealthExpectation(store *Store, pending *PendingUpdate, bootNonce string) healthExpectation {
	expect := healthExpectation{
		BootNonce: bootNonce,
	}
	if pending == nil {
		return expect
	}
	if strings.HasPrefix(pending.TargetVersion, "baseline-") {
		// Archived baseline binaries may predate boot-nonce support; still correlate
		// nonces if supported and reject responses matching the replaced release.
		expect.RejectVersion = releaseAppVersion(store, pending.PreviousVersion)
		return expect
	}
	expect.ExpectedVersion = releaseAppVersion(store, pending.TargetVersion)
	expect.RequireBootAck = bootNonce != ""
	return expect
}

func releaseAppVersion(store *Store, relID string) string {
	relID = strings.TrimSpace(relID)
	if relID == "" || store == nil {
		return ""
	}
	if err := ValidateVersion(relID); err == nil {
		if mb, err := os.ReadFile(filepath.Join(store.ReleasesDir(), relID, "RELEASE_META")); err == nil {
			var meta ReleaseMeta
			if json.Unmarshal(mb, &meta) == nil {
				if v := strings.TrimSpace(meta.Version); v != "" && !strings.HasPrefix(v, "baseline-") {
					return v
				}
			}
		}
	}
	if !strings.HasPrefix(relID, "baseline-") {
		return relID
	}
	return ""
}

func versionsEqual(a, b string) bool {
	na := strings.TrimPrefix(strings.TrimSpace(a), "v")
	nb := strings.TrimPrefix(strings.TrimSpace(b), "v")
	return na != "" && nb != "" && na == nb
}

func waitForHealth(ctx context.Context, dataURL, adminURL string, timeout, interval time.Duration, expect healthExpectation) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	lastErr := errors.New("health check timed out")
	for {
		if err := checkEndpointHealth(ctx, client, dataURL, adminURL, expect); err == nil {
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

func checkEndpointHealth(ctx context.Context, client *http.Client, dataURL, adminURL string, expect healthExpectation) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dataURL, nil)
	if err != nil {
		return err
	}
	if expect.BootNonce != "" {
		req.Header.Set(HeaderBootProbe, expect.BootNonce)
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
		Status               string   `json:"status"`
		Version              string   `json:"version"`
		DetectorsUnavailable []string `json:"detectors_unavailable"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("invalid healthz json: %w", err)
	}
	if payload.Status != "ok" {
		if len(payload.DetectorsUnavailable) > 0 {
			return fmt.Errorf("healthz status=%q (detectors_unavailable=%v)", payload.Status, payload.DetectorsUnavailable)
		}
		return fmt.Errorf("healthz status=%q", payload.Status)
	}
	ack := strings.TrimSpace(resp.Header.Get(HeaderBootAck))
	if expect.BootNonce != "" {
		wantAck := BootAck(expect.BootNonce)
		if ack != "" && ack != wantAck {
			return fmt.Errorf("healthz boot ack mismatch (response from another instance)")
		}
		if expect.RequireBootAck && ack != wantAck {
			return fmt.Errorf("healthz missing matching boot ack for launched child")
		}
	}
	gotVer := strings.TrimSpace(payload.Version)
	if expect.ExpectedVersion != "" {
		if gotVer == "" {
			return fmt.Errorf("healthz missing version field (expected %q)", expect.ExpectedVersion)
		}
		if !versionsEqual(gotVer, expect.ExpectedVersion) {
			return fmt.Errorf("healthz version %q does not match expected release %q", gotVer, expect.ExpectedVersion)
		}
	} else if expect.RejectVersion != "" && gotVer != "" && versionsEqual(gotVer, expect.RejectVersion) && ack == "" {
		return fmt.Errorf("healthz responded with previous release %q instead of rollback target", gotVer)
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
