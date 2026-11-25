// Package plugin starts extension-point plugins as child processes and
// adapts them to Gatehouse's internal interfaces.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	sdk "github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

// DetectTimeout bounds one Detect call.
const DetectTimeout = 5 * time.Second

// Restart backoff of a plugin whose process exited: it doubles from
// RestartBackoffMin up to RestartBackoffMax and starts over once the plugin
// has stayed up for RestartBackoffMax.
const (
	RestartBackoffMin = time.Second
	RestartBackoffMax = 30 * time.Second
	exitPoll          = 100 * time.Millisecond
)

// Plugin states reported by Status.
const (
	StateRunning    = "running"
	StateRestarting = "restarting"
	StateStopped    = "stopped"
)

var errHostClosed = errors.New("plugin host is closed")

// Host owns running plugin processes. Every enabled plugin gets a supervisor
// goroutine that restarts its process when it exits; callers reach the
// current process only through Started.Impl, never a cached client.
type Host struct {
	mu      sync.Mutex
	closed  bool // no process may start any more
	stop    chan struct{}
	wg      sync.WaitGroup
	clients map[*goplugin.Client]struct{}
	plugins []*Started
}

func (h *Host) initLocked() {
	if h.stop == nil {
		h.stop = make(chan struct{})
		h.clients = map[*goplugin.Client]struct{}{}
	}
}

// StartDetectors starts every enabled detector plugin. It returns the
// detectors (supervised; possibly not running yet) and the names of required
// detectors (redaction.detectors) that are disabled; the caller fails closed
// on those. A detector that is not running fails its Detect calls.
func (h *Host) StartDetectors(cfg *config.Config, log *slog.Logger) ([]redact.ExternalDetector, []string) {
	required := map[string]bool{}
	for _, d := range cfg.Redaction.Detectors {
		required[d] = true
	}
	var dets []redact.ExternalDetector
	var missing []string
	for _, pc := range cfg.Plugins {
		if pc.Kind != sdk.KindDetector {
			continue
		}
		if pc.Enabled != nil && !*pc.Enabled {
			log.Warn("detector plugin disabled", "plugin", pc.Name, "required", required[pc.Name])
			if required[pc.Name] {
				missing = append(missing, pc.Name)
			}
			continue
		}
		dets = append(dets, &detector{s: h.supervise(pc, log)})
	}
	return dets, missing
}

// Started is a supervised plugin of any kind.
type Started struct {
	Name string
	Kind string
	pc   config.Plugin
	log  *slog.Logger

	mu       sync.RWMutex
	client   *goplugin.Client // nil while restarting
	impl     any              // sdk.AuthPolicy, sdk.AuditSink, *sdk.ProviderClient, sdk.Hooks or sdk.Detector
	state    string
	since    time.Time // when the current process started
	restarts int
	lastExit time.Time
	lastErr  string
}

// Impl returns the current plugin implementation, or false while the
// process is not running (crashed, restarting or stopped).
func (s *Started) Impl() (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil || s.client.Exited() {
		return nil, false
	}
	return s.impl, true
}

// Exited reports whether the plugin process is not running right now.
func (s *Started) Exited() bool {
	_, ok := s.Impl()
	return !ok
}

// Status is a plugin's state for /healthz.
type Status struct {
	Name      string     `json:"name"`
	Kind      string     `json:"kind"`
	State     string     `json:"state"`
	Restarts  int        `json:"restarts"`
	LastExit  *time.Time `json:"last_exit,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

// Status reports the plugin's current state.
func (s *Started) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{Name: s.Name, Kind: s.Kind, State: s.state, Restarts: s.restarts, LastError: s.lastErr}
	if st.State == StateRunning && (s.client == nil || s.client.Exited()) {
		st.State = StateRestarting // exited; the supervisor has not noticed yet
	}
	if !s.lastExit.IsZero() {
		t := s.lastExit
		st.LastExit = &t
	}
	return st
}

// Status lists every supervised plugin in start order.
func (h *Host) Status() []Status {
	h.mu.Lock()
	ps := append([]*Started(nil), h.plugins...)
	h.mu.Unlock()
	out := make([]Status, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Status())
	}
	return out
}

// StartKind starts every enabled plugin of kind under supervision. A plugin
// whose first start fails is returned too (not running; its supervisor keeps
// retrying). Disabled plugins are returned by name in disabled.
func (h *Host) StartKind(cfg *config.Config, kind string, log *slog.Logger) (plugins []*Started, disabled []string) {
	for _, pc := range cfg.Plugins {
		if pc.Kind != kind {
			continue
		}
		if pc.Enabled != nil && !*pc.Enabled {
			log.Warn("plugin disabled", "plugin", pc.Name, "kind", kind)
			disabled = append(disabled, pc.Name)
			continue
		}
		plugins = append(plugins, h.supervise(pc, log))
	}
	return plugins, disabled
}

// supervise starts pc once synchronously, then hands it to a supervisor
// goroutine that restarts the process whenever it exits.
func (h *Host) supervise(pc config.Plugin, log *slog.Logger) *Started {
	s := &Started{Name: pc.Name, Kind: pc.Kind, pc: pc, log: log, state: StateRestarting}
	raw, c, err := h.launch(pc)
	if err != nil {
		s.lastErr = err.Error()
		log.Error("plugin failed to start; retrying in the background", "plugin", pc.Name, "kind", pc.Kind, "err", err)
	} else {
		s.client, s.impl, s.state, s.since = c, raw, StateRunning, time.Now()
		log.Info("plugin started", "plugin", pc.Name, "kind", pc.Kind)
	}
	h.mu.Lock()
	h.initLocked()
	h.plugins = append(h.plugins, s)
	if !h.closed {
		h.wg.Add(1)
		go h.run(s)
	}
	h.mu.Unlock()
	return s
}

// run is the supervisor loop of one plugin.
func (h *Host) run(s *Started) {
	defer h.wg.Done()
	backoff := RestartBackoffMin
	for {
		s.mu.RLock()
		c, since := s.client, s.since
		s.mu.RUnlock()
		if c != nil {
			if !h.waitExit(c) {
				return
			}
			now := time.Now()
			s.mu.Lock()
			s.client, s.impl, s.state, s.lastExit = nil, nil, StateRestarting, now
			s.mu.Unlock()
			h.discard(c)
			if now.Sub(since) >= RestartBackoffMax {
				backoff = RestartBackoffMin
			}
			s.log.Warn("plugin process exited; restarting", "plugin", s.Name, "kind", s.Kind,
				"uptime", now.Sub(since).Round(time.Millisecond).String(), "retry_in", backoff.String())
		}
		if !h.sleep(backoff) {
			return
		}
		raw, nc, err := h.launch(s.pc)
		if errors.Is(err, errHostClosed) {
			return
		}
		next := min(backoff*2, RestartBackoffMax)
		if err != nil {
			s.mu.Lock()
			s.lastErr = err.Error()
			s.mu.Unlock()
			s.log.Error("plugin restart failed", "plugin", s.Name, "kind", s.Kind, "err", err, "retry_in", next.String())
			backoff = next
			continue
		}
		s.mu.Lock()
		s.client, s.impl, s.state, s.since, s.lastErr = nc, raw, StateRunning, time.Now(), ""
		s.restarts++
		n := s.restarts
		s.mu.Unlock()
		s.log.Info("plugin restarted", "plugin", s.Name, "kind", s.Kind, "restarts", n)
		backoff = next // a crash right after this restart waits longer
	}
}

// waitExit polls until c's process exits (true) or supervision stops (false).
func (h *Host) waitExit(c *goplugin.Client) bool {
	t := time.NewTicker(exitPoll)
	defer t.Stop()
	for {
		if c.Exited() {
			return true
		}
		select {
		case <-h.stop:
			return false
		case <-t.C:
		}
	}
}

// sleep waits d unless supervision stops first.
func (h *Host) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-h.stop:
		return false
	case <-t.C:
		return true
	}
}

// launch starts one plugin process and dispenses its declared kind. A
// process that starts while the host is closing is killed before returning,
// so Close never leaves one behind.
func (h *Host) launch(pc config.Plugin) (any, *goplugin.Client, error) {
	c := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  sdk.Handshake,
		Plugins:          sdk.PluginSet(),
		Cmd:              exec.Command(pc.Path, pc.Args...),
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		Logger:           hclog.New(&hclog.LoggerOptions{Name: "plugin." + pc.Name, Output: io.Discard}),
		StartTimeout:     10 * time.Second,
	})
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, nil, errHostClosed
	}
	h.initLocked()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	var raw any
	rpc, err := c.Client()
	if err == nil {
		raw, err = rpc.Dispense(pc.Kind)
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		err = errHostClosed
	}
	if err != nil {
		h.discard(c)
		return nil, nil, err
	}
	return raw, c, nil
}

// discard kills c (a no-op once exited) and forgets it.
func (h *Host) discard(c *goplugin.Client) {
	c.Kill()
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// StopRestarts stops every supervisor without killing running plugins: a
// plugin that exits from now on stays down. Shutdown and the FD handoff
// drain call it first so in-flight requests keep their plugins while nothing
// is restarted behind a stopping gateway.
func (h *Host) StopRestarts() {
	h.mu.Lock()
	h.initLocked()
	select {
	case <-h.stop:
	default:
		close(h.stop)
	}
	h.mu.Unlock()
}

// Close stops supervision and kills all plugin processes. It waits for the
// supervisors, so a restart in progress is killed rather than orphaned.
func (h *Host) Close() {
	h.StopRestarts()
	h.mu.Lock()
	h.closed = true
	cs := make([]*goplugin.Client, 0, len(h.clients))
	for c := range h.clients {
		cs = append(cs, c)
	}
	ps := append([]*Started(nil), h.plugins...)
	h.mu.Unlock()
	for _, c := range cs {
		c.Kill()
	}
	h.wg.Wait()
	for _, p := range ps {
		p.mu.Lock()
		p.client, p.impl, p.state = nil, nil, StateStopped
		p.mu.Unlock()
	}
}

type detector struct {
	s *Started
}

func (d *detector) Name() string { return d.s.Name }

// Exited reports whether the detector subprocess is not running right now.
func (d *detector) Exited() bool { return d.s.Exited() }

// Detect calls the plugin and converts code point offsets to byte offsets.
// A plugin whose process is not running is an error (the engine fails closed).
func (d *detector) Detect(ctx context.Context, text string) ([]redact.Finding, error) {
	raw, ok := d.s.Impl()
	if !ok {
		return nil, fmt.Errorf("plugin process not running")
	}
	ctx, cancel := context.WithTimeout(ctx, DetectTimeout)
	defer cancel()
	fs, err := raw.(sdk.Detector).Detect(ctx, text, "", nil)
	if err != nil {
		return nil, err
	}
	// rune index -> byte offset
	idx := make([]int, 0, utf8.RuneCountInString(text)+1)
	for i := range text {
		idx = append(idx, i)
	}
	idx = append(idx, len(text))
	out := make([]redact.Finding, 0, len(fs))
	for _, f := range fs {
		if f.Start < 0 || f.End > len(idx)-1 || f.Start >= f.End || f.Kind == "" {
			continue
		}
		out = append(out, redact.Finding{Kind: f.Kind, Start: idx[f.Start], End: idx[f.End], Source: d.s.Name})
	}
	return out, nil
}
