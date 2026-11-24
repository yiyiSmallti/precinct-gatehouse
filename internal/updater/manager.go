package updater

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/live"
)

const (
	historySettingsKey = "updater_history"
	maxHistoryEvents   = 30
)

// ErrBusy is returned when an update apply or rollback is already in progress.
var ErrBusy = errors.New("an update operation is already in progress")

// ErrRestartUnavailable is returned when an online update or rollback requires
// restarting the service, but the gateway is not running under `gatehouse supervise`.
var ErrRestartUnavailable = errors.New("automatic restart requires running under `gatehouse supervise`")

// Deps configures a Manager.
type Deps struct {
	CurrentVersion  string
	ConfigPath      string
	Live            *live.Manager
	StaticConfig    *config.Updater
	DB              *sql.DB
	Inflight        func() int64
	ResolveSecret   func(ref string) (string, error)
	RequestRestart  func()
	ManualRestartOK bool
	Log             *slog.Logger

	// Optional test hooks.
	NewSource   func(cfg config.Updater, secret string) (ArtifactSource, error)
	HTTPTimeout time.Duration
}

// ApplyOptions controls Apply / ApplyAsync behavior.
type ApplyOptions struct {
	Tag       string
	Force     bool
	NoRestart bool
}

// RollbackOptions controls Rollback / RollbackAsync behavior.
type RollbackOptions struct {
	TargetVersion string
	Force         bool
	NoRestart     bool
}

// Manager orchestrates periodic update checks, downloads, verification,
// graceful in-flight draining, release switching, and update history.
type Manager struct {
	deps  Deps
	store *Store
	log   *slog.Logger

	mu          sync.RWMutex
	state       string
	message     string
	busy        bool
	available   *RemoteInfo
	lastCheckAt *time.Time
	nextCheckAt *time.Time
	lastError   string
	history     []UpdateEvent

	notifyCh chan struct{}
	bgCtx    context.Context
}

// NewManager creates an updater Manager.
func NewManager(d Deps) *Manager {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	if strings.TrimSpace(d.ConfigPath) != "" {
		cfgPath := config.ExpandHome(strings.TrimSpace(d.ConfigPath))
		if abs, err := filepath.Abs(cfgPath); err == nil {
			cfgPath = abs
		} else {
			cfgPath = filepath.Clean(cfgPath)
		}
		d.ConfigPath = cfgPath
	}
	exePath, _ := os.Executable()
	if exePath != "" {
		if abs, err := filepath.Abs(exePath); err == nil {
			exePath = abs
		}
	}
	cfg := currentUpdaterConfig(d)
	home := resolveManagerHome(cfg.HomeDir, d.ConfigPath, exePath)
	m := &Manager{
		deps:     d,
		store:    NewStore(home),
		log:      log,
		state:    StateIdle,
		notifyCh: make(chan struct{}, 1),
		bgCtx:    context.Background(),
	}
	m.loadHistory()
	return m
}

func resolveManagerHome(cfgHomeDir, configPath, exePath string) string {
	effective := strings.TrimSpace(cfgHomeDir)
	if effective == "" {
		effective = strings.TrimSpace(os.Getenv(EnvHomeDir))
	}
	home := ResolveHomeDir(effective, configPath, exePath)
	if abs, err := filepath.Abs(home); err == nil {
		return abs
	}
	return home
}

func currentUpdaterConfig(d Deps) config.Updater {
	if d.Live != nil {
		if snap := d.Live.Current(); snap != nil && snap.Cfg != nil {
			return snap.Cfg.Updater
		}
	}
	if d.StaticConfig != nil {
		return *d.StaticConfig
	}
	return config.Updater{}
}

func (m *Manager) currentConfig() config.Updater {
	return currentUpdaterConfig(m.deps)
}

func (m *Manager) syncStoreHome(cfg config.Updater) {
	exePath, _ := os.Executable()
	if exePath != "" {
		if abs, err := filepath.Abs(exePath); err == nil {
			exePath = abs
		}
	}
	home := resolveManagerHome(cfg.HomeDir, m.deps.ConfigPath, exePath)
	m.mu.Lock()
	if m.store.HomeDir != home {
		m.store = NewStore(home)
	}
	m.mu.Unlock()
}

// Store returns the underlying release store.
func (m *Manager) Store() *Store {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store
}

// ConfirmBoot cleans up .update_pending when the running process matches the
// pending target (primarily when running directly without `gatehouse supervise`),
// and records any recent .failed_update event into SQLite history once.
func (m *Manager) ConfirmBoot() {
	st := m.Store()
	if !IsSupervised() {
		if pending, err := st.ReadPending(); err == nil && pending != nil {
			if pending.TargetVersion == m.deps.CurrentVersion || st.CurrentVersion() == pending.TargetVersion {
				_ = st.ClearPending()
				m.recordEvent(UpdateEvent{
					At:      time.Now().UTC(),
					Action:  "apply",
					Version: pending.TargetVersion,
					Digest:  pending.TargetDigest,
					Status:  "ok",
					Detail:  "started on new release",
				})
			}
		}
	}
	if failed, err := st.ReadFailed(); err == nil && failed != nil {
		m.recordFailedOnce(failed)
	}
}

func (m *Manager) recordFailedOnce(failed *FailedUpdate) {
	m.mu.Lock()
	already := false
	for _, ev := range m.history {
		if ev.Action == "auto_rollback" && ev.Version == failed.Version && ev.At.Equal(failed.At) {
			already = true
			break
		}
	}
	m.mu.Unlock()
	if !already {
		m.recordEvent(UpdateEvent{
			At:      failed.At,
			Action:  "auto_rollback",
			Version: failed.Version,
			Digest:  failed.Digest,
			Status:  "failed",
			Detail:  failed.Reason,
		})
	}
}

// NotifyConfigChange wakes the background polling loop immediately after a hot reload.
func (m *Manager) NotifyConfigChange() {
	m.syncStoreHome(m.currentConfig())
	select {
	case m.notifyCh <- struct{}{}:
	default:
	}
}

// Start launches the background check/auto-apply loop until ctx is cancelled.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.bgCtx = ctx
	m.mu.Unlock()

	go m.runLoop(ctx)
}

func (m *Manager) runLoop(ctx context.Context) {
	for {
		cfg := m.currentConfig()
		m.syncStoreHome(cfg)
		if !cfg.Enabled {
			m.mu.Lock()
			m.nextCheckAt = nil
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-m.notifyCh:
				continue
			}
		}

		interval := cfg.Interval.D(5 * time.Minute)
		if interval < 30*time.Second {
			interval = 30 * time.Second
		}
		next := time.Now().Add(interval)
		m.mu.Lock()
		m.nextCheckAt = &next
		m.mu.Unlock()

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.notifyCh:
			timer.Stop()
			continue
		case <-timer.C:
		}

		m.runBackgroundCycle(ctx)
	}
}

func (m *Manager) runBackgroundCycle(ctx context.Context) {
	cfg := m.currentConfig()
	if !cfg.Enabled {
		return
	}
	st, err := m.Check(ctx)
	if err != nil {
		m.log.Warn("background update check failed", "err", err)
		return
	}
	if st.Available == nil || !st.Available.UpdateAvailable || !cfg.AutoApply {
		return
	}
	if !m.CanRestart() {
		m.log.Warn("skipping background auto-apply: gateway is not running under `gatehouse supervise`")
		return
	}
	if failed, _ := m.Store().ReadFailed(); failed != nil {
		var matched bool
		if failed.Digest != "" && st.Available.Digest != "" {
			matched = failed.Digest == st.Available.Digest
		} else {
			matched = failed.Version != "" && st.Available.Version != "" && failed.Version == st.Available.Version
		}
		if matched {
			m.log.Warn("skipping auto-apply for previously failed update",
				"version", st.Available.Version, "digest", st.Available.Digest, "reason", failed.Reason)
			return
		}
	}
	if _, err := m.Apply(ctx, ApplyOptions{Force: false}); err != nil && !errors.Is(err, ErrBusy) {
		m.log.Warn("background auto-update failed", "err", err)
	}
}

// Status returns a snapshot of the current updater state, releases, and config.
func (m *Manager) Status() Status {
	cfg := m.currentConfig()
	m.syncStoreHome(cfg)
	st := m.Store()
	pending, _ := st.ReadPending()
	failed, _ := st.ReadFailed()

	var inflight int64
	if m.deps.Inflight != nil {
		inflight = m.deps.Inflight()
	}

	m.mu.RLock()
	var avail *RemoteInfo
	if m.available != nil {
		cp := *m.available
		avail = &cp
	}
	var lastChk, nextChk *time.Time
	if m.lastCheckAt != nil {
		t := *m.lastCheckAt
		lastChk = &t
	}
	if m.nextCheckAt != nil {
		t := *m.nextCheckAt
		nextChk = &t
	}
	history := append([]UpdateEvent(nil), m.history...)
	state := m.state
	msg := m.message
	lastErr := m.lastError
	m.mu.RUnlock()

	if history == nil {
		history = []UpdateEvent{}
	}

	return Status{
		CurrentVersion:  m.deps.CurrentVersion,
		ActiveRelease:   st.CurrentVersion(),
		PreviousRelease: st.PreviousVersion(),
		CurrentDigest:   st.CurrentDigest(),
		HomeDir:         st.HomeDir,
		Supervised:      IsSupervised(),
		CanRestart:      m.CanRestart(),
		State:           state,
		Message:         msg,
		Inflight:        inflight,
		Available:       avail,
		Pending:         pending,
		Failed:          failed,
		LastCheckAt:     lastChk,
		NextCheckAt:     nextChk,
		LastError:       lastErr,
		Releases:        st.ListReleases(),
		History:         history,
		Config:          m.summarizeConfig(cfg),
	}
}

func (m *Manager) summarizeConfig(cfg config.Updater) ConfigSummary {
	interval := cfg.Interval.D(5 * time.Minute)
	drain := cfg.DrainTimeout.D(5 * time.Minute)
	health := cfg.HealthTimeout.D(25 * time.Second)
	hasPwd := cfg.PasswordRef != "" && m.secretExists(cfg.PasswordRef)
	hasTok := cfg.TokenRef != "" && m.secretExists(cfg.TokenRef)
	return ConfigSummary{
		Enabled:          cfg.Enabled,
		AutoApply:        cfg.AutoApply,
		Source:           cfg.EffectiveSource(),
		Interval:         interval.String(),
		DrainTimeout:     drain.String(),
		HealthTimeout:    health.String(),
		KeepReleases:     cfg.KeepCount(),
		HomeDir:          cfg.HomeDir,
		FallbackDNS:      cfg.EffectiveFallbackDNS(),
		HasPublicKey:     strings.TrimSpace(cfg.PublicKey) != "",
		PublicKey:        cfg.PublicKey,
		RequireSignature: cfg.RequireSig(),
		Image:            cfg.Image,
		Tag:              cfg.EffectiveTag(),
		Username:         cfg.Username,
		HasPassword:      hasPwd,
		GitHubRepo:       cfg.GitHubRepo,
		GitHubAPIURL:     cfg.GitHubAPIURL,
		HasToken:         hasTok,
		Prerelease:       cfg.Prerelease,
	}
}

func (m *Manager) secretExists(ref string) bool {
	if m.deps.ResolveSecret != nil {
		v, err := m.deps.ResolveSecret(ref)
		return err == nil && v != ""
	}
	return false
}

func (m *Manager) buildSource(cfg config.Updater, tagOverride string) (ArtifactSource, error) {
	srcType := cfg.EffectiveSource()
	if srcType == "" {
		return nil, errors.New("updater source is not configured (set image or github_repo)")
	}
	if tagOverride != "" {
		cfg.Tag = tagOverride
	}

	var secret string
	var ref string
	if srcType == "oci" {
		ref = cfg.PasswordRef
	} else if srcType == "github" {
		ref = cfg.TokenRef
	}
	if ref != "" {
		if m.deps.ResolveSecret == nil {
			return nil, fmt.Errorf("cannot resolve secret ref %q without resolver", ref)
		}
		v, err := m.deps.ResolveSecret(ref)
		if err != nil {
			return nil, fmt.Errorf("resolve secret %q: %w", ref, err)
		}
		secret = v
	}

	if m.deps.NewSource != nil {
		return m.deps.NewSource(cfg, secret)
	}

	var pub ed25519.PublicKey
	if strings.TrimSpace(cfg.PublicKey) != "" {
		var err error
		pub, err = ParsePublicKey(cfg.PublicKey)
		if err != nil {
			return nil, err
		}
	}
	timeout := m.deps.HTTPTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	httpClient := NewHTTPClient(timeout, cfg.EffectiveFallbackDNS())

	switch srcType {
	case "oci":
		if strings.TrimSpace(cfg.Image) == "" {
			return nil, errors.New("updater.image is required for oci source")
		}
		return &OCISource{
			Image:      cfg.Image,
			Tag:        cfg.EffectiveTag(),
			Username:   cfg.Username,
			Password:   secret,
			HTTPClient: httpClient,
			PubKey:     pub,
			RequireSig: cfg.RequireSig(),
		}, nil
	case "github":
		if strings.TrimSpace(cfg.GitHubRepo) == "" {
			return nil, errors.New("updater.github_repo is required for github source")
		}
		ghTag := strings.TrimSpace(tagOverride)
		if ghTag == "" {
			ghTag = strings.TrimSpace(cfg.Tag)
		}
		if ghTag == "latest" {
			ghTag = ""
		}
		return &GitHubSource{
			Repo:       cfg.GitHubRepo,
			Tag:        ghTag,
			APIURL:     cfg.GitHubAPIURL,
			Token:      secret,
			Prerelease: cfg.Prerelease,
			HTTPClient: httpClient,
			PubKey:     pub,
			RequireSig: cfg.RequireSig(),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported updater source %q", srcType)
	}
}

func (m *Manager) setState(state, msg, lastErr string) {
	m.mu.Lock()
	m.state = state
	m.message = msg
	if lastErr != "" || state != StateError {
		m.lastError = lastErr
	}
	m.mu.Unlock()
}

// Check queries the configured update source and refreshes Status().Available.
func (m *Manager) Check(ctx context.Context) (Status, error) {
	cfg := m.currentConfig()
	m.syncStoreHome(cfg)

	src, err := m.buildSource(cfg, "")
	if err != nil {
		m.setState(StateError, "配置不完整", err.Error())
		return m.Status(), err
	}

	m.mu.Lock()
	wasBusy := m.busy
	if !wasBusy {
		m.state = StateChecking
		m.message = "正在检查远程更新..."
	}
	m.mu.Unlock()

	remote, err := src.Check(ctx)
	now := time.Now().UTC()
	if err != nil {
		m.mu.Lock()
		m.lastCheckAt = &now
		if !m.busy {
			m.state = StateError
			m.message = "检查更新失败"
			m.lastError = err.Error()
		}
		m.mu.Unlock()
		m.recordEvent(UpdateEvent{
			At:     now,
			Action: "check",
			Status: "failed",
			Detail: err.Error(),
		})
		return m.Status(), err
	}

	st := m.Store()
	curDigest := st.CurrentDigest()
	switch remote.Source {
	case "oci":
		if curDigest != "" {
			remote.UpdateAvailable = remote.Digest != "" && remote.Digest != curDigest
		} else if remote.Version != "" && remote.Version == m.deps.CurrentVersion && !IsNewerVersion(remote.Version, m.deps.CurrentVersion) {
			if remote.Digest != "" {
				_ = st.WriteCurrentDigest(remote.Digest)
			}
			remote.UpdateAvailable = false
		} else {
			remote.UpdateAvailable = true
		}
	case "github":
		remote.UpdateAvailable = IsNewerVersion(remote.Version, m.deps.CurrentVersion)
	}

	m.mu.Lock()
	m.lastCheckAt = &now
	cp := *remote
	m.available = &cp
	if !m.busy {
		m.lastError = ""
		if remote.UpdateAvailable {
			m.state = StateAvailable
			m.message = fmt.Sprintf("发现可用更新 %s", remote.Version)
		} else {
			m.state = StateIdle
			m.message = "已是最新版本"
		}
	}
	m.mu.Unlock()

	detail := "up-to-date"
	if remote.UpdateAvailable {
		detail = "update available"
	}
	m.recordEvent(UpdateEvent{
		At:      now,
		Action:  "check",
		Version: remote.Version,
		Digest:  remote.Digest,
		Status:  "ok",
		Detail:  detail,
	})
	return m.Status(), nil
}

// CanRestart reports whether the Manager can trigger an automatic process restart
// after switching releases (i.e. when running under `gatehouse supervise`).
func (m *Manager) CanRestart() bool {
	return m.deps.RequestRestart != nil
}

// ApplyAsync begins an update in the background, returning ErrBusy if one is active
// or ErrRestartUnavailable if a restart is required without supervisor support.
func (m *Manager) ApplyAsync(opts ApplyOptions) error {
	if !opts.NoRestart && !m.CanRestart() && !m.deps.ManualRestartOK {
		return ErrRestartUnavailable
	}
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return ErrBusy
	}
	m.busy = true
	m.state = StateDownloading
	m.message = "准备拉取更新包..."
	m.lastError = ""
	ctx := m.bgCtx
	m.mu.Unlock()

	go func() {
		_, err := m.runApplyLocked(ctx, opts)
		if err != nil {
			m.log.Error("async update apply failed", "err", err)
		}
	}()
	return nil
}

// Apply downloads, verifies, preflights, drains in-flight requests, installs,
// and triggers a restart (unless opts.NoRestart is set).
func (m *Manager) Apply(ctx context.Context, opts ApplyOptions) (InstallResult, error) {
	if !opts.NoRestart && !m.CanRestart() && !m.deps.ManualRestartOK {
		return InstallResult{}, ErrRestartUnavailable
	}
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return InstallResult{}, ErrBusy
	}
	m.busy = true
	m.state = StateDownloading
	m.message = "准备拉取更新包..."
	m.lastError = ""
	m.mu.Unlock()

	return m.runApplyLocked(ctx, opts)
}

func (m *Manager) releaseBusyOnError(err error, action, ver, digest string) {
	m.mu.Lock()
	m.busy = false
	m.state = StateError
	m.message = "更新失败"
	m.lastError = err.Error()
	m.mu.Unlock()
	m.recordEvent(UpdateEvent{
		At:      time.Now().UTC(),
		Action:  action,
		Version: ver,
		Digest:  digest,
		Status:  "failed",
		Detail:  err.Error(),
	})
}

func (m *Manager) runApplyLocked(ctx context.Context, opts ApplyOptions) (InstallResult, error) {
	cfg := m.currentConfig()
	m.syncStoreHome(cfg)
	st := m.Store()

	src, err := m.buildSource(cfg, opts.Tag)
	if err != nil {
		m.releaseBusyOnError(err, "apply", opts.Tag, "")
		return InstallResult{}, err
	}

	if err := os.MkdirAll(st.ReleasesDir(), 0o755); err != nil {
		m.releaseBusyOnError(err, "apply", opts.Tag, "")
		return InstallResult{}, err
	}
	pullTmp, err := os.MkdirTemp(st.ReleasesDir(), ".pull-*")
	if err != nil {
		m.releaseBusyOnError(err, "apply", opts.Tag, "")
		return InstallResult{}, err
	}
	defer os.RemoveAll(pullTmp)

	m.setState(StateDownloading, "正在拉取并校验更新包...", "")
	art, err := src.Download(ctx, pullTmp)
	if err != nil {
		m.releaseBusyOnError(err, "apply", opts.Tag, "")
		return InstallResult{}, err
	}

	m.setState(StateVerifying, fmt.Sprintf("正在预检 %s...", art.Version), "")
	if err := st.Preflight(ctx, art.Verified.BinaryPath, m.deps.ConfigPath); err != nil {
		m.releaseBusyOnError(err, "apply", art.Version, art.Remote.Digest)
		return InstallResult{}, err
	}

	if !opts.Force {
		drainTimeout := cfg.DrainTimeout.D(5 * time.Minute)
		if err := m.waitForInflightDrain(ctx, drainTimeout); err != nil {
			m.releaseBusyOnError(err, "apply", art.Version, art.Remote.Digest)
			return InstallResult{}, err
		}
	}

	m.setState(StateApplying, fmt.Sprintf("正在切换版本 %s...", art.Version), "")
	res, err := st.Install(ctx, InstallRequest{
		Version:       art.Version,
		Digest:        art.Remote.Digest,
		Artifact:      art.Verified,
		ConfigPath:    m.deps.ConfigPath,
		KeepReleases:  cfg.KeepCount(),
		RecordPending: !opts.NoRestart,
		SkipPreflight: true,
	})
	if err != nil {
		m.releaseBusyOnError(err, "apply", art.Version, art.Remote.Digest)
		return InstallResult{}, err
	}

	_ = st.ClearFailed()
	m.recordEvent(UpdateEvent{
		At:      time.Now().UTC(),
		Action:  "apply",
		Version: res.Version,
		Digest:  art.Remote.Digest,
		Status:  "ok",
		Detail:  fmt.Sprintf("switched from %s", res.PreviousVersion),
	})

	if opts.NoRestart || m.deps.RequestRestart == nil {
		m.mu.Lock()
		m.busy = false
		m.state = StateIdle
		m.message = fmt.Sprintf("已安装 %s（未重启）", res.Version)
		m.available = nil
		m.mu.Unlock()
		return res, nil
	}

	m.mu.Lock()
	m.busy = false
	m.state = StateRestarting
	m.message = fmt.Sprintf("正在重启切换到 %s...", res.Version)
	m.available = nil
	m.mu.Unlock()

	m.scheduleRestart()
	return res, nil
}

// RollbackAsync starts an asynchronous rollback to opts.TargetVersion (or previous).
func (m *Manager) RollbackAsync(opts RollbackOptions) error {
	if !opts.NoRestart && !m.CanRestart() && !m.deps.ManualRestartOK {
		return ErrRestartUnavailable
	}
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return ErrBusy
	}
	m.busy = true
	m.state = StateApplying
	m.message = "准备回滚版本..."
	m.lastError = ""
	ctx := m.bgCtx
	m.mu.Unlock()

	go func() {
		_, err := m.runRollbackLocked(ctx, opts)
		if err != nil {
			m.log.Error("async rollback failed", "err", err)
		}
	}()
	return nil
}

// Rollback switches to opts.TargetVersion (or previous) and triggers restart.
func (m *Manager) Rollback(ctx context.Context, opts RollbackOptions) (InstallResult, error) {
	if !opts.NoRestart && !m.CanRestart() && !m.deps.ManualRestartOK {
		return InstallResult{}, ErrRestartUnavailable
	}
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return InstallResult{}, ErrBusy
	}
	m.busy = true
	m.state = StateApplying
	m.message = "准备回滚版本..."
	m.lastError = ""
	m.mu.Unlock()

	return m.runRollbackLocked(ctx, opts)
}

func (m *Manager) runRollbackLocked(ctx context.Context, opts RollbackOptions) (InstallResult, error) {
	cfg := m.currentConfig()
	m.syncStoreHome(cfg)
	st := m.Store()

	if !opts.Force {
		drainTimeout := cfg.DrainTimeout.D(5 * time.Minute)
		if err := m.waitForInflightDrain(ctx, drainTimeout); err != nil {
			m.releaseBusyOnError(err, "rollback", opts.TargetVersion, "")
			return InstallResult{}, err
		}
	}

	m.setState(StateApplying, "正在回滚发布目录...", "")
	res, err := st.Rollback(ctx, opts.TargetVersion, m.deps.ConfigPath, !opts.NoRestart)
	if err != nil {
		m.releaseBusyOnError(err, "rollback", opts.TargetVersion, "")
		return InstallResult{}, err
	}

	m.recordEvent(UpdateEvent{
		At:      time.Now().UTC(),
		Action:  "rollback",
		Version: res.Version,
		Status:  "ok",
		Detail:  fmt.Sprintf("rolled back from %s", res.PreviousVersion),
	})

	if opts.NoRestart || m.deps.RequestRestart == nil {
		m.mu.Lock()
		m.busy = false
		m.state = StateIdle
		m.message = fmt.Sprintf("已回滚到 %s（未重启）", res.Version)
		m.mu.Unlock()
		return res, nil
	}

	m.mu.Lock()
	m.busy = false
	m.state = StateRestarting
	m.message = fmt.Sprintf("正在重启切换到 %s...", res.Version)
	m.mu.Unlock()

	m.scheduleRestart()
	return res, nil
}

func (m *Manager) waitForInflightDrain(ctx context.Context, timeout time.Duration) error {
	if m.deps.Inflight == nil || timeout <= 0 {
		return nil
	}
	cur := m.deps.Inflight()
	if cur <= 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	tk := time.NewTicker(250 * time.Millisecond)
	defer tk.Stop()
	for cur > 0 {
		m.setState(StateDraining, fmt.Sprintf("等待 %d 个在途请求完成后切换...", cur), "")
		if time.Now().After(deadline) {
			m.log.Warn("in-flight drain timeout reached; proceeding with cutover", "remaining_inflight", cur, "timeout", timeout)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tk.C:
			cur = m.deps.Inflight()
		}
	}
	return nil
}

func (m *Manager) scheduleRestart() {
	if m.deps.RequestRestart == nil {
		return
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		m.deps.RequestRestart()
	}()
}

func (m *Manager) loadHistory() {
	if m.deps.DB == nil {
		return
	}
	var raw string
	err := m.deps.DB.QueryRow(`SELECT v FROM settings WHERE k=?`, historySettingsKey).Scan(&raw)
	if err != nil || raw == "" {
		return
	}
	var evs []UpdateEvent
	if json.Unmarshal([]byte(raw), &evs) == nil {
		m.mu.Lock()
		m.history = evs
		m.mu.Unlock()
	}
}

func (m *Manager) recordEvent(ev UpdateEvent) {
	m.mu.Lock()
	m.history = append([]UpdateEvent{ev}, m.history...)
	if len(m.history) > maxHistoryEvents {
		m.history = m.history[:maxHistoryEvents]
	}
	snapshot := append([]UpdateEvent(nil), m.history...)
	m.mu.Unlock()

	if m.deps.DB != nil {
		if b, err := json.Marshal(snapshot); err == nil {
			_, _ = m.deps.DB.Exec(
				`INSERT INTO settings(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
				historySettingsKey, string(b),
			)
		}
	}
}
