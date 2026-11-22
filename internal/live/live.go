// Package live manages hot-reloadable configuration snapshots, atomic config
// writes, backup history, rollback and file watching.
package live

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/route"
)

const maxHistoryFiles = 30

var (
	// ErrConflict is returned by Apply when baseHash does not match the current config.
	ErrConflict   = errors.New("config conflict: file changed since last load")
	historyNameRe = regexp.MustCompile(`^config-[0-9.-]+\.yaml$`)
)

// BuildError wraps an error encountered while constructing runtime components
// from a syntactically valid config (e.g. unresolvable provider key_ref).
type BuildError struct {
	Err error
}

func (e *BuildError) Error() string { return e.Err.Error() }
func (e *BuildError) Unwrap() error { return e.Err }

// Snapshot is an immutable view of the active configuration and runtime engines.
type Snapshot struct {
	Cfg      *config.Config
	Reg      *provider.Registry
	Router   *route.Router
	Redactor *redact.Engine
	Version  int64
	LoadedAt time.Time
}

// LastError records the latest background reload failure.
type LastError struct {
	Msg string    `json:"msg"`
	At  time.Time `json:"at"`
}

// ApplyResult is returned after a successful Apply, Rollback or Reload.
type ApplyResult struct {
	Version         int64    `json:"version"`
	RestartRequired []string `json:"restart_required"`
}

// HistoryEntry describes one saved config backup.
type HistoryEntry struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
	Size int64     `json:"size"`
}

// Status summarizes the live config manager state.
type Status struct {
	Version         int64
	LoadedAt        time.Time
	Path            string
	Hash            string
	Source          string
	LastError       *LastError
	RestartRequired []string
	Snapshot        *Snapshot
}

// Manager holds the current Snapshot and serializes config reloads.
type Manager struct {
	cur     atomic.Pointer[Snapshot]
	mu      sync.Mutex
	path    string
	bootCfg *config.Config
	build   func(cfg *config.Config, prev *Snapshot) (*Snapshot, error)

	stateMu   sync.RWMutex
	source    string
	lastErr   *LastError
	restart   []string
	lastMtime time.Time
	lastSize  int64
}

// New builds the initial Snapshot and creates a Manager.
func New(initial *config.Config, build func(cfg *config.Config, prev *Snapshot) (*Snapshot, error)) (*Manager, error) {
	if initial == nil {
		return nil, fmt.Errorf("initial config is nil")
	}
	snap, err := build(initial, nil)
	if err != nil {
		return nil, err
	}
	snap.Version = 1
	snap.LoadedAt = time.Now()
	m := &Manager{
		path:    initial.Path(),
		bootCfg: initial,
		build:   build,
		source:  "startup",
	}
	if st, err := os.Stat(m.path); err == nil {
		m.lastMtime = st.ModTime()
		m.lastSize = st.Size()
	}
	m.cur.Store(snap)
	return m, nil
}

// Current returns the active immutable Snapshot.
func (m *Manager) Current() *Snapshot {
	return m.cur.Load()
}

// Preview validates newBytes and runs the build function without persisting or swapping.
func (m *Manager) Preview(newBytes []byte) (*config.Config, error) {
	prev := m.cur.Load()
	newCfg, err := config.Parse(newBytes, m.path)
	if err != nil {
		return nil, err
	}
	if _, err := m.build(newCfg, prev); err != nil {
		return nil, &BuildError{Err: err}
	}
	return newCfg, nil
}

// Status returns a consistent view of manager metadata and current Snapshot.
func (m *Manager) Status() Status {
	snap := m.cur.Load()
	m.stateMu.RLock()
	src := m.source
	var le *LastError
	if m.lastErr != nil {
		cp := *m.lastErr
		le = &cp
	}
	rr := append([]string(nil), m.restart...)
	m.stateMu.RUnlock()
	if rr == nil {
		rr = []string{}
	}
	return Status{
		Version:         snap.Version,
		LoadedAt:        snap.LoadedAt,
		Path:            m.path,
		Hash:            snap.Cfg.Hash(),
		Source:          src,
		LastError:       le,
		RestartRequired: rr,
		Snapshot:        snap,
	}
}

// Apply parses, validates, builds, backs up, atomically writes and swaps newBytes.
func (m *Manager) Apply(newBytes []byte, source, baseHash string) (ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.cur.Load()
	if baseHash != prev.Cfg.Hash() {
		return ApplyResult{}, ErrConflict
	}
	return m.applyLocked(prev, newBytes, source)
}

func (m *Manager) applyLocked(prev *Snapshot, newBytes []byte, source string) (ApplyResult, error) {
	newCfg, err := config.Parse(newBytes, m.path)
	if err != nil {
		return ApplyResult{}, err
	}
	snap, err := m.build(newCfg, prev)
	if err != nil {
		return ApplyResult{}, &BuildError{Err: err}
	}

	if err := m.backupOld(prev.Cfg); err != nil {
		return ApplyResult{}, fmt.Errorf("backup config: %w", err)
	}
	if err := atomicWriteFile(m.path, newBytes, 0o600); err != nil {
		return ApplyResult{}, fmt.Errorf("write config: %w", err)
	}

	snap.Version = prev.Version + 1
	snap.LoadedAt = time.Now()
	restart := config.RestartRequiredDiff(m.bootCfg, newCfg)
	m.cur.Store(snap)

	m.stateMu.Lock()
	m.source = source
	m.lastErr = nil
	m.restart = restart
	if st, err := os.Stat(m.path); err == nil {
		m.lastMtime = st.ModTime()
		m.lastSize = st.Size()
	}
	m.stateMu.Unlock()

	if restart == nil {
		restart = []string{}
	}
	return ApplyResult{Version: snap.Version, RestartRequired: restart}, nil
}

// ReloadFromDisk reads the config file from disk and reloads if its hash changed.
// Unlike Apply, it never writes or backs up the config file.
func (m *Manager) ReloadFromDisk(source string) (ApplyResult, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	prev := m.cur.Load()
	b, err := os.ReadFile(m.path)
	if err != nil {
		m.recordError(err)
		return ApplyResult{}, false, err
	}
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	if h == prev.Cfg.Hash() {
		m.stateMu.RLock()
		rr := append([]string(nil), m.restart...)
		m.stateMu.RUnlock()
		if rr == nil {
			rr = []string{}
		}
		return ApplyResult{Version: prev.Version, RestartRequired: rr}, false, nil
	}

	newCfg, err := config.Parse(b, m.path)
	if err != nil {
		m.recordError(err)
		return ApplyResult{}, false, err
	}
	snap, err := m.build(newCfg, prev)
	if err != nil {
		be := &BuildError{Err: err}
		m.recordError(be)
		return ApplyResult{}, false, be
	}

	snap.Version = prev.Version + 1
	snap.LoadedAt = time.Now()
	restart := config.RestartRequiredDiff(m.bootCfg, newCfg)
	m.cur.Store(snap)

	m.stateMu.Lock()
	m.source = source
	m.lastErr = nil
	m.restart = restart
	if st, err := os.Stat(m.path); err == nil {
		m.lastMtime = st.ModTime()
		m.lastSize = st.Size()
	}
	m.stateMu.Unlock()

	if restart == nil {
		restart = []string{}
	}
	return ApplyResult{Version: snap.Version, RestartRequired: restart}, true, nil
}

func (m *Manager) recordError(err error) {
	m.stateMu.Lock()
	m.lastErr = &LastError{Msg: err.Error(), At: time.Now()}
	m.stateMu.Unlock()
}

// Watch polls the config file every 2 seconds and reloads on external edits.
func (m *Manager) Watch(ctx context.Context, log *slog.Logger) {
	tk := time.NewTicker(2 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			st, err := os.Stat(m.path)
			if err != nil {
				continue
			}
			m.stateMu.RLock()
			changed := !st.ModTime().Equal(m.lastMtime) || st.Size() != m.lastSize
			m.stateMu.RUnlock()
			if !changed {
				continue
			}
			m.stateMu.Lock()
			m.lastMtime = st.ModTime()
			m.lastSize = st.Size()
			m.stateMu.Unlock()

			res, reloaded, rerr := m.ReloadFromDisk("file")
			if rerr != nil {
				if log != nil {
					log.Warn("config auto-reload failed", "err", rerr)
				}
			} else if reloaded && log != nil {
				log.Info("config reloaded from disk", "version", res.Version, "restart_required", res.RestartRequired)
			}
		}
	}
}

func (m *Manager) historyDir() string {
	return filepath.Join(m.cur.Load().Cfg.DataDir, "config-history")
}

func (m *Manager) backupOld(prev *config.Config) error {
	dir := filepath.Join(prev.DataDir, "config-history")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	oldBytes, err := os.ReadFile(m.path)
	if err != nil {
		oldBytes = prev.Source()
	}
	var dst string
	for {
		name := fmt.Sprintf("config-%s.yaml", time.Now().UTC().Format("20060102-150405.000"))
		dst = filepath.Join(dir, name)
		if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := atomicWriteFile(dst, oldBytes, 0o600); err != nil {
		return err
	}
	return pruneHistory(dir, maxHistoryFiles)
}

func pruneHistory(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && historyNameRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return nil
	}
	sort.Strings(names)
	for _, old := range names[:len(names)-keep] {
		_ = os.Remove(filepath.Join(dir, old))
	}
	return nil
}

// HistoryList returns saved config backups sorted newest first.
func (m *Manager) HistoryList() ([]HistoryEntry, error) {
	dir := m.historyDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []HistoryEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []HistoryEntry{}
	for _, e := range entries {
		if e.IsDir() || !historyNameRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		at := info.ModTime().UTC()
		if tsStr := stringsTrimHistory(e.Name()); tsStr != "" {
			if parsed, perr := time.Parse("20060102-150405.000", tsStr); perr == nil {
				at = parsed
			}
		}
		out = append(out, HistoryEntry{
			Name: e.Name(),
			At:   at,
			Size: info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

func stringsTrimHistory(name string) string {
	if len(name) < len("config-")+len(".yaml") {
		return ""
	}
	return name[len("config-") : len(name)-len(".yaml")]
}

// HistoryRead reads a specific backup file by validated filename.
func (m *Manager) HistoryRead(name string) ([]byte, error) {
	if !historyNameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid history filename %q", name)
	}
	return os.ReadFile(filepath.Join(m.historyDir(), name))
}

// Rollback restores a backup file and applies it as a new live config version.
func (m *Manager) Rollback(name string) (ApplyResult, error) {
	if !historyNameRe.MatchString(name) {
		return ApplyResult{}, fmt.Errorf("invalid history filename %q", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.cur.Load()
	b, err := os.ReadFile(filepath.Join(prev.Cfg.DataDir, "config-history", name))
	if err != nil {
		return ApplyResult{}, err
	}
	return m.applyLocked(prev, b, "rollback")
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
