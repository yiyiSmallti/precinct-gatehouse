package tiered

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
)

const (
	maxBodyBytes      = 1 << 30 // 1 GiB (supports unlimited images)
	maxPrefixEntries  = 100000
	maxSessionEntries = 20000
	prefixTTL         = 24 * time.Hour
	sessionTTL        = 12 * time.Hour
)

// ErrShadowMode is returned when attempting active-only operations (like manual warming) in shadow mode.
var ErrShadowMode = errors.New("tiered router is in shadow mode")

type BackendConfig = config.TieredBackend
type RouterConfig = config.TieredConfig

type BackendState struct {
	mu            sync.RWMutex
	cfg           BackendConfig
	target        *url.URL
	healthy       atomic.Bool
	activeReqs    atomic.Int64
	activeHeavy   atomic.Int64
	totalRouted   atomic.Int64
	heavyRouted   atomic.Int64
	lastHealthErr atomic.Value // string
	healthFails   atomic.Int32
}

func (b *BackendState) config() BackendConfig {
	b.mu.RLock()
	c := b.cfg
	b.mu.RUnlock()
	return c
}

func (b *BackendState) snapshot() (BackendConfig, *url.URL) {
	b.mu.RLock()
	c, u := b.cfg, b.target
	b.mu.RUnlock()
	return c, u
}

func (b *BackendState) updateConfig(c BackendConfig, u *url.URL) bool {
	b.mu.Lock()
	urlChanged := b.target == nil || b.target.String() != u.String()
	b.cfg = c
	b.target = u
	b.mu.Unlock()
	return urlChanged
}

type PrefixEntry struct {
	Tokens      int
	LastBackend string
	UpdatedAt   time.Time
}

type SessionEntry struct {
	Tier              int
	LastTokens        int
	LastBackend       string
	HeavyWriteBackend string
	HeavyWriteAt      time.Time
	UpdatedAt         time.Time
}

type RouteLogEntry struct {
	Time         string `json:"time"`
	Backend      string `json:"backend"`
	Reason       string `json:"reason"`
	CompressMode string `json:"compress_mode,omitempty"`
	OrigEst      int    `json:"orig_est,omitempty"`
	TotalEst     int    `json:"total_est"`
	CachedEst    int    `json:"cached_est"`
	MissEst      int    `json:"miss_est"`
	ActualPrompt int    `json:"actual_prompt,omitempty"`
	ActualCache  int    `json:"actual_cache,omitempty"`
	Status       int    `json:"status"`
	DurationMs   int64  `json:"duration_ms"`
}

type ShadowMismatch struct {
	Time            string `json:"time"`
	Predicted       string `json:"predicted"`
	PredictedReason string `json:"predicted_reason"`
	Actual          string `json:"actual"`
	ActualReason    string `json:"actual_reason"`
	TotalEst        int    `json:"total_est"`
	MissEst         int    `json:"miss_est"`
}

type ShadowBackendCount struct {
	Total   int64 `json:"total"`
	Matched int64 `json:"matched"`
}

type ShadowStats struct {
	Since      string                        `json:"since"`
	Compared   int64                         `json:"compared"`
	Matched    int64                         `json:"matched"`
	Unknown    int64                         `json:"unknown"`
	MatchRate  float64                       `json:"match_rate"`
	ByActual   map[string]ShadowBackendCount `json:"by_actual"`
	Mismatches []ShadowMismatch              `json:"mismatches"`
}

type BackendStatus struct {
	ID               string `json:"id"`
	URL              string `json:"url"`
	Tier             int    `json:"tier"`
	MaxContextTokens int    `json:"max_context_tokens"`
	MaxSafeInput     int    `json:"max_safe_input"`
	PromoteAt        int    `json:"promote_at"`
	DemoteBelow      int    `json:"demote_below"`
	SpeedScore       int    `json:"speed_score"`
	Healthy          bool   `json:"healthy"`
	LastHealthErr    string `json:"last_health_err,omitempty"`
	ActiveReqs       int64  `json:"active_reqs"`
	ActiveHeavy      int64  `json:"active_heavy"`
	TotalRouted      int64  `json:"total_routed"`
	HeavyRouted      int64  `json:"heavy_routed"`
}

type ProviderStatus struct {
	Provider           string          `json:"provider"`
	Mode               string          `json:"mode"`
	HeavyMissThreshold int             `json:"heavy_miss_threshold"`
	HeavyStickySec     int             `json:"heavy_sticky_sec"`
	SpilloverThreshold int             `json:"spillover_threshold"`
	SSEKeepaliveSec    int             `json:"sse_keepalive_sec"`
	HealthIntervalSec  int             `json:"health_interval_sec"`
	ZhThinkAnchor      bool            `json:"zh_think_anchor"`
	Backends           []BackendStatus `json:"backends"`
	CachedPrefixes     int             `json:"cached_prefixes"`
	TrackedSessions    int             `json:"tracked_sessions"`
	RecentRoutes       []RouteLogEntry `json:"recent_routes"`
	Shadow             ShadowStats     `json:"shadow"`
	StarterCache       map[string]any  `json:"starter_cache"`
	ContextCompress    map[string]any  `json:"context_compress"`
}

type WarmupFilterFunc func(ctx context.Context, provider, backendURL string, payload []byte) ([]byte, error)

// WarmupRestoreFunc restores redacted placeholders in Stage 2 summaries before local checkpoint freezing.
type WarmupRestoreFunc func(provider, text string) string

type Router struct {
	name                         string
	defaultDB                    string
	log                          *slog.Logger
	transport                    *http.Transport
	starters                     *StarterCache
	compressor                   *ContextCompressor
	sseKeepaliveInitDelay        atomic.Int64 // nanoseconds; 0 defaults to 20s
	sseKeepaliveIntervalOverride atomic.Int64 // nanoseconds; 0 uses sseKeepaliveInterval(sec)

	cfgMu         sync.RWMutex
	cfg           RouterConfig
	target        string
	apiKey        string
	resolveKeyFn  func(ref string) (string, error)
	warmupFilter  WarmupFilterFunc
	warmupRestore WarmupRestoreFunc
	backends      []*BackendState
	byID          map[string]*BackendState

	mu         sync.RWMutex
	prefixes   map[string]*PrefixEntry
	sessions   map[string]*SessionEntry
	recentLogs []RouteLogEntry

	shadowMu         sync.Mutex
	shadowSince      time.Time
	shadowCompared   int64
	shadowMatched    int64
	shadowUnknown    int64
	shadowByActual   map[string]ShadowBackendCount
	shadowMismatches []ShadowMismatch

	loopMu     sync.Mutex
	loopCtx    context.Context
	loopCancel context.CancelFunc
}

func defaultCompressPersistPath(starterPersist string) string {
	if starterPersist == "" {
		return ""
	}
	if strings.HasSuffix(starterPersist, "-starters.json") {
		return strings.TrimSuffix(starterPersist, "-starters.json") + "-checkpoints.json"
	}
	if strings.HasSuffix(starterPersist, ".json") {
		return strings.TrimSuffix(starterPersist, ".json") + "-checkpoints.json"
	}
	return starterPersist + "-checkpoints.json"
}

func normalizeRouterConfig(cfg RouterConfig, defaultPersist string) RouterConfig {
	if cfg.Mode == "" {
		cfg.Mode = "shadow"
	}
	if cfg.HeavyMissThreshold <= 0 {
		cfg.HeavyMissThreshold = 24000
	}
	if cfg.HeavyStickySec == 0 {
		cfg.HeavyStickySec = 12
	}
	if cfg.SpilloverThreshold == 0 {
		cfg.SpilloverThreshold = 4
	}
	if cfg.SSEKeepaliveSec == 0 {
		cfg.SSEKeepaliveSec = 15
	}
	if cfg.HealthIntervalSec <= 0 {
		cfg.HealthIntervalSec = 5
	}
	var sc StarterCacheConfig
	if cfg.StarterCache == nil {
		sc = defaultStarterConfig(defaultPersist)
	} else {
		sc = *cfg.StarterCache
		applyStarterDefaults(&sc, defaultPersist)
	}
	cfg.StarterCache = &sc

	cpPersist := defaultCompressPersistPath(defaultPersist)
	var cc ContextCompressConfig
	if cfg.ContextCompress == nil {
		cc = defaultContextCompressConfig(cpPersist)
	} else {
		cc = *cfg.ContextCompress
		applyContextCompressDefaults(&cc, cpPersist)
	}
	cfg.ContextCompress = &cc
	return cfg
}

func heavyStickyDuration(cfg RouterConfig) time.Duration {
	if cfg.HeavyStickySec < 0 {
		return 0
	}
	if cfg.HeavyStickySec == 0 {
		return 12 * time.Second
	}
	return time.Duration(cfg.HeavyStickySec) * time.Second
}

func spilloverThreshold(cfg RouterConfig) int {
	if cfg.SpilloverThreshold < 0 {
		return 0
	}
	if cfg.SpilloverThreshold == 0 {
		return 4
	}
	return cfg.SpilloverThreshold
}

func sseKeepaliveInterval(sec int) time.Duration {
	if sec < 0 {
		return 0
	}
	if sec == 0 {
		return 15 * time.Second
	}
	return time.Duration(sec) * time.Second
}

func (r *Router) SetSSEKeepaliveInitDelay(d time.Duration) {
	if r != nil {
		r.sseKeepaliveInitDelay.Store(int64(d))
	}
}

func (r *Router) getSSEKeepaliveInitDelay() time.Duration {
	if r != nil {
		if ns := r.sseKeepaliveInitDelay.Load(); ns > 0 {
			return time.Duration(ns)
		}
	}
	return 20 * time.Second
}

func (r *Router) getSSEKeepaliveInterval(sec int) time.Duration {
	if sec < 0 {
		return 0
	}
	if r != nil {
		if ns := r.sseKeepaliveIntervalOverride.Load(); ns > 0 {
			return time.Duration(ns)
		}
	}
	return sseKeepaliveInterval(sec)
}

func zhAnchorEnabled(c RouterConfig) bool {
	if c.ZhThinkAnchor == nil {
		return true
	}
	return *c.ZhThinkAnchor
}

func newDefaultTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 60 * time.Second,
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err == nil {
				if tc, ok := conn.(*net.TCPConn); ok {
					_ = tc.SetNoDelay(true)
				}
			}
			return conn, err
		},
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       120 * time.Second,
		ResponseHeaderTimeout: 2 * time.Hour,
		DisableCompression:    true,
	}
}

func newRouter(name string, cfg RouterConfig, defaultPersist string, tr *http.Transport, logger *slog.Logger) (*Router, error) {
	return newRouterWithTarget(name, "", "", cfg, defaultPersist, tr, logger)
}

func newRouterWithTarget(name, target, apiKey string, cfg RouterConfig, defaultPersist string, tr *http.Transport, logger *slog.Logger) (*Router, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if tr == nil {
		tr = newDefaultTransport()
	}
	norm := normalizeRouterConfig(cfg, defaultPersist)
	if target == "" {
		target = config.DefaultProviderTarget(&config.Provider{Tiered: &norm})
	}
	r := &Router{
		name:           name,
		defaultDB:      defaultPersist,
		log:            logger,
		transport:      tr,
		cfg:            norm,
		target:         target,
		apiKey:         apiKey,
		byID:           make(map[string]*BackendState),
		prefixes:       make(map[string]*PrefixEntry, 4096),
		sessions:       make(map[string]*SessionEntry, 1024),
		shadowSince:    time.Now(),
		shadowByActual: make(map[string]ShadowBackendCount),
	}
	r.starters = newStarterCache(*norm.StarterCache, r)
	r.starters.enforceTarget(target)
	r.compressor = newContextCompressor(*norm.ContextCompress, r)
	r.compressor.enforceTarget(target)
	if err := r.rebuildBackends(norm.Backends); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Router) rebuildBackends(list []BackendConfig) error {
	r.cfgMu.Lock()
	oldByID := r.byID
	nextList := make([]*BackendState, 0, len(list))
	nextByID := make(map[string]*BackendState, len(list))
	var toProbe []*BackendState
	for _, bc := range list {
		u, err := url.Parse(bc.URL)
		if err != nil {
			r.cfgMu.Unlock()
			return fmt.Errorf("invalid backend url %q: %w", bc.URL, err)
		}
		if existing, ok := oldByID[bc.ID]; ok {
			_, prevURL := existing.snapshot()
			if prevURL != nil && prevURL.String() == u.String() {
				existing.updateConfig(bc, u)
				nextList = append(nextList, existing)
				nextByID[bc.ID] = existing
				continue
			}
		}
		bs := &BackendState{cfg: bc, target: u}
		bs.lastHealthErr.Store("initializing")
		nextList = append(nextList, bs)
		nextByID[bc.ID] = bs
		toProbe = append(toProbe, bs)
	}
	sort.SliceStable(nextList, func(i, j int) bool {
		return nextList[i].config().Tier < nextList[j].config().Tier
	})
	r.backends = nextList
	r.byID = nextByID
	r.cfgMu.Unlock()

	if len(toProbe) > 0 {
		cfg := r.getConfig()
		probeBackends(toProbe, cfg.Mode, cfg.HealthMaxTimeouts, r.getAPIKey(), &http.Client{Timeout: 4000 * time.Millisecond}, r.log, r.name)
	}
	return nil
}

func (r *Router) apply(target, apiKey string, cfg RouterConfig) error {
	norm := normalizeRouterConfig(cfg, r.defaultDB)
	if target == "" {
		target = config.DefaultProviderTarget(&config.Provider{Tiered: &norm})
	}
	r.cfgMu.Lock()
	r.cfg = norm
	r.target = target
	r.apiKey = apiKey
	r.cfgMu.Unlock()

	if err := r.rebuildBackends(norm.Backends); err != nil {
		return err
	}
	r.starters.applyConfig(*norm.StarterCache)
	r.starters.enforceTarget(target)
	r.compressor.applyConfig(*norm.ContextCompress)
	r.compressor.enforceTarget(target)
	r.ensureLoops()
	loopCtx := r.loopContext()
	r.starters.syncWarmLoop(loopCtx, norm.Mode == "active" && norm.StarterCache.Enabled)
	r.compressor.syncLoop(loopCtx, norm.Mode == "active" && norm.ContextCompress.Enabled)
	return nil
}

func (r *Router) setKeyResolver(fn func(ref string) (string, error)) {
	if r == nil {
		return
	}
	r.cfgMu.Lock()
	r.resolveKeyFn = fn
	r.cfgMu.Unlock()
}

func (r *Router) resolveSecret(ref string) string {
	if r == nil || ref == "" {
		return ""
	}
	r.cfgMu.RLock()
	fn := r.resolveKeyFn
	r.cfgMu.RUnlock()
	if fn == nil {
		return ""
	}
	v, err := fn(ref)
	if err != nil {
		r.log.Warn("tiered key resolve failed", "provider", r.name, "err", err)
		return ""
	}
	return v
}

func (r *Router) getTarget() string {
	if r == nil {
		return "local"
	}
	r.cfgMu.RLock()
	t := r.target
	r.cfgMu.RUnlock()
	if t == "" {
		return "local"
	}
	return t
}

func (r *Router) getAPIKey() string {
	if r == nil {
		return ""
	}
	r.cfgMu.RLock()
	k := r.apiKey
	r.cfgMu.RUnlock()
	return k
}

func (r *Router) setWarmupFilter(fn WarmupFilterFunc) {
	if r == nil {
		return
	}
	r.cfgMu.Lock()
	r.warmupFilter = fn
	r.cfgMu.Unlock()
}

func (r *Router) getWarmupFilter() WarmupFilterFunc {
	if r == nil {
		return nil
	}
	r.cfgMu.RLock()
	fn := r.warmupFilter
	r.cfgMu.RUnlock()
	return fn
}

func (r *Router) setWarmupRestore(fn WarmupRestoreFunc) {
	if r == nil {
		return
	}
	r.cfgMu.Lock()
	r.warmupRestore = fn
	r.cfgMu.Unlock()
}

func (r *Router) getWarmupRestore() WarmupRestoreFunc {
	if r == nil {
		return nil
	}
	r.cfgMu.RLock()
	fn := r.warmupRestore
	r.cfgMu.RUnlock()
	return fn
}

func (r *Router) loopContext() context.Context {
	r.loopMu.Lock()
	defer r.loopMu.Unlock()
	if r.loopCtx == nil {
		ctx, cancel := context.WithCancel(context.Background())
		r.loopCtx = ctx
		r.loopCancel = cancel
	}
	return r.loopCtx
}

func (r *Router) ensureLoops() {
	r.loopMu.Lock()
	if r.loopCancel != nil {
		r.loopMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.loopCtx = ctx
	r.loopCancel = cancel
	r.loopMu.Unlock()

	r.startBackgroundLoops(ctx)
	cfg := r.getConfig()
	r.starters.syncWarmLoop(ctx, cfg.Mode == "active" && cfg.StarterCache.Enabled)
	r.compressor.syncLoop(ctx, cfg.Mode == "active" && cfg.ContextCompress.Enabled)
}

func (r *Router) close() {
	r.loopMu.Lock()
	if r.loopCancel != nil {
		r.loopCancel()
		r.loopCancel = nil
	}
	r.loopCtx = nil
	r.loopMu.Unlock()
	r.starters.syncWarmLoop(context.Background(), false)
	r.compressor.syncLoop(context.Background(), false)
	r.starters.saveToDisk()
	r.compressor.saveToDisk()
}

func (r *Router) getConfig() RouterConfig {
	r.cfgMu.RLock()
	c := r.cfg
	r.cfgMu.RUnlock()
	return c
}

func (r *Router) getBackends() []*BackendState {
	r.cfgMu.RLock()
	out := append([]*BackendState(nil), r.backends...)
	r.cfgMu.RUnlock()
	return out
}

func (r *Router) backendByID(id string) *BackendState {
	if r == nil || id == "" {
		return nil
	}
	r.cfgMu.RLock()
	b := r.byID[id]
	r.cfgMu.RUnlock()
	return b
}

func (r *Router) backendFor(id string, target *url.URL) *BackendState {
	if r == nil || id == "" || target == nil {
		return nil
	}
	r.cfgMu.RLock()
	b := r.byID[id]
	r.cfgMu.RUnlock()
	if b == nil {
		return nil
	}
	_, u := b.snapshot()
	if u == nil || u.String() != target.String() {
		return nil
	}
	return b
}

func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "Client.Timeout") || strings.Contains(err.Error(), "deadline exceeded")
}

func probeBackends(backends []*BackendState, mode string, cfgMaxTimeouts int, apiKey string, client *http.Client, logger *slog.Logger, routerName string) {
	var wg sync.WaitGroup
	for _, b := range backends {
		wg.Add(1)
		go func(b *BackendState) {
			defer wg.Done()
			prevHealthy := b.healthy.Load()
			bCfg := b.config()
			probeBase := strings.TrimSuffix(strings.TrimRight(bCfg.URL, "/"), "/v1")
			req, err := http.NewRequest(http.MethodGet, probeBase+"/get_server_info", nil)
			var resp *http.Response
			if err == nil {
				if apiKey != "" {
					req.Header.Set("Authorization", "Bearer "+apiKey)
				}
				resp, err = client.Do(req)
			}
			if err != nil {
				fails := b.healthFails.Add(1)
				maxTimeouts := int32(3)
				if b.activeHeavy.Load() > 0 {
					maxTimeouts = 24
				} else if b.activeReqs.Load() > 0 {
					maxTimeouts = 12
				}
				if (mode == "shadow" || bCfg.MaxContextTokens >= 524288) && maxTimeouts < 12 {
					maxTimeouts = 12
				}
				if cfgMaxTimeouts > int(maxTimeouts) {
					maxTimeouts = int32(cfgMaxTimeouts)
				}
				if !isTimeoutErr(err) || !prevHealthy || fails >= maxTimeouts {
					b.healthy.Store(false)
					b.lastHealthErr.Store(err.Error())
					if prevHealthy {
						logger.Warn("backend marked unhealthy", "provider", routerName, "backend", bCfg.ID, "fails", fails, "active", b.activeReqs.Load(), "heavy", b.activeHeavy.Load(), "err", err)
					}
				} else if fails == 4 || fails == 12 {
					logger.Warn("backend transient probe timeout while busy", "provider", routerName, "backend", bCfg.ID, "fails", fails, "max", maxTimeouts, "active", b.activeReqs.Load(), "heavy", b.activeHeavy.Load())
				}
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				b.healthFails.Store(0)
				b.healthy.Store(true)
				b.lastHealthErr.Store("")
				if !prevHealthy {
					logger.Info("backend marked healthy", "provider", routerName, "backend", bCfg.ID)
				}
			} else {
				b.healthFails.Add(1)
				b.healthy.Store(false)
				b.lastHealthErr.Store(fmt.Sprintf("HTTP %d", resp.StatusCode))
				if prevHealthy {
					logger.Warn("backend marked unhealthy", "provider", routerName, "backend", bCfg.ID, "status", resp.StatusCode)
				}
			}
		}(b)
	}
	wg.Wait()
}

func (r *Router) startBackgroundLoops(ctx context.Context) {
	client := &http.Client{Timeout: 4000 * time.Millisecond}
	go func() {
		for {
			sec := r.getConfig().HealthIntervalSec
			if sec <= 0 {
				sec = 5
			}
			tk := time.NewTimer(time.Duration(sec) * time.Second)
			select {
			case <-ctx.Done():
				tk.Stop()
				return
			case <-tk.C:
				cfg := r.getConfig()
				probeBackends(r.getBackends(), cfg.Mode, cfg.HealthMaxTimeouts, r.getAPIKey(), client, r.log, r.name)
			}
		}
	}()
	go func() {
		tk := time.NewTicker(10 * time.Minute)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				r.gcCaches()
			}
		}
	}()
}

func (r *Router) gcCaches() {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.prefixes {
		if now.Sub(v.UpdatedAt) > prefixTTL {
			delete(r.prefixes, k)
		}
	}
	if len(r.prefixes) > maxPrefixEntries {
		type kv struct {
			k string
			t time.Time
		}
		all := make([]kv, 0, len(r.prefixes))
		for k, v := range r.prefixes {
			all = append(all, kv{k, v.UpdatedAt})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		for i := 0; i < len(all)/2; i++ {
			delete(r.prefixes, all[i].k)
		}
	}
	for k, v := range r.sessions {
		if now.Sub(v.UpdatedAt) > sessionTTL {
			delete(r.sessions, k)
		}
	}
}

func (r *Router) seedWarmedPrefix(tools json.RawMessage, sysMsgs []json.RawMessage, totalTokens int, backendID string) {
	if len(sysMsgs) == 0 || totalTokens <= 0 {
		return
	}
	var toolsHash [32]byte
	toolsEst := 0
	if len(tools) > 0 && !bytes.Equal(bytes.TrimSpace(tools), []byte("null")) {
		t := compactJSONBytes(tools)
		toolsHash = sha256.Sum256(t)
		toolsEst = estimateTextTokens(string(t))
	}
	prevHash := hex.EncodeToString(toolsHash[:16])
	hashes := make([]string, len(sysMsgs))
	cumEst := make([]int, len(sysMsgs))
	runningEst := toolsEst
	for i, m := range sysMsgs {
		trimmed := compactJSONBytes(m)
		h := sha256.New()
		h.Write([]byte(prevHash))
		h.Write([]byte("|"))
		h.Write(trimmed)
		sum := hex.EncodeToString(h.Sum(nil)[:16])
		hashes[i] = sum
		prevHash = sum
		runningEst += estimateMessageTokens(trimmed)
		cumEst[i] = runningEst
	}

	now := time.Now()
	scale := 1.0
	if last := cumEst[len(cumEst)-1]; last > 0 {
		scale = float64(totalTokens) / float64(last)
	}
	r.mu.Lock()
	for i, hash := range hashes {
		tok := int(float64(cumEst[i]) * scale)
		if i == len(hashes)-1 {
			tok = totalTokens
		}
		if tok < 1 {
			tok = 1
		}
		r.prefixes[hash] = &PrefixEntry{
			Tokens:      tok,
			LastBackend: backendID,
			UpdatedAt:   now,
		}
	}
	r.mu.Unlock()
}

func estimateTextTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	cjk := 0
	ascii := 0
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			ascii++
			i++
			continue
		}
		ru, sz := utf8.DecodeRuneInString(s[i:])
		if ru >= 0x2E80 {
			cjk++
		} else {
			ascii += sz
		}
		i += sz
	}
	tok := int(float64(cjk)*0.75 + float64(ascii)*0.28)
	if tok < 1 {
		tok = 1
	}
	return tok
}

func estimateMessageTokens(raw json.RawMessage) int {
	var msg struct {
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		ToolCalls json.RawMessage `json:"tool_calls"`
		Reasoning string          `json:"reasoning_content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return len(raw) / 4
	}
	toks := 8
	if len(msg.ToolCalls) > 0 {
		toks += estimateTextTokens(string(msg.ToolCalls))
	}
	if len(msg.Reasoning) > 0 {
		toks += estimateTextTokens(msg.Reasoning)
	}
	c := bytes.TrimSpace(msg.Content)
	if len(c) == 0 || bytes.Equal(c, []byte("null")) {
		return toks
	}
	if c[0] == '"' {
		var s string
		if json.Unmarshal(c, &s) == nil {
			toks += estimateTextTokens(s)
		}
		return toks
	}
	if c[0] == '[' {
		var parts []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			ImageURL json.RawMessage `json:"image_url"`
		}
		if json.Unmarshal(c, &parts) == nil {
			for _, p := range parts {
				if p.Type == "image_url" || p.Type == "input_image" || len(p.ImageURL) > 0 {
					toks += 1200
				} else if len(p.Text) > 0 {
					toks += estimateTextTokens(p.Text)
				}
			}
			return toks
		}
	}
	return toks + len(c)/4
}

type RequestAnalysis struct {
	Hashes       []string
	CumEstTokens []int
	SessionKey   string
	TotalEst     int
	CachedEst    int
	MissEst      int
	MaxOutTokens int
}

func (r *Router) analyzeChatBody(body []byte) (*RequestAnalysis, bool) {
	var payload struct {
		Messages            []json.RawMessage `json:"messages"`
		Tools               json.RawMessage   `json:"tools"`
		MaxTokens           int               `json:"max_tokens"`
		MaxCompletionTokens int               `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Messages) == 0 {
		return nil, false
	}

	toolsEst := 0
	var toolsHash [32]byte
	if len(payload.Tools) > 0 && !bytes.Equal(bytes.TrimSpace(payload.Tools), []byte("null")) {
		t := compactJSONBytes(payload.Tools)
		toolsHash = sha256.Sum256(t)
		toolsEst = estimateTextTokens(string(t))
	}

	n := len(payload.Messages)
	hashes := make([]string, n)
	cumEst := make([]int, n)
	prevHash := hex.EncodeToString(toolsHash[:16])
	runningEst := toolsEst

	for i, m := range payload.Messages {
		trimmed := compactJSONBytes(m)
		h := sha256.New()
		h.Write([]byte(prevHash))
		h.Write([]byte("|"))
		h.Write(trimmed)
		sum := hex.EncodeToString(h.Sum(nil)[:16])
		hashes[i] = sum
		prevHash = sum

		runningEst += estimateMessageTokens(trimmed)
		cumEst[i] = runningEst
	}

	matchedIdx := -1
	cachedTokens := 0
	if r != nil {
		r.mu.RLock()
		for i := n - 1; i >= 0; i-- {
			if entry, ok := r.prefixes[hashes[i]]; ok {
				matchedIdx = i
				cachedTokens = entry.Tokens
				break
			}
		}
		r.mu.RUnlock()
	}

	var totalEst, missEst int
	if matchedIdx >= 0 {
		deltaEst := cumEst[n-1] - cumEst[matchedIdx]
		if deltaEst < 0 {
			deltaEst = 0
		}
		totalEst = cachedTokens + deltaEst
		missEst = deltaEst
	} else {
		totalEst = cumEst[n-1]
		missEst = totalEst
	}

	sessIdx := 0
	if n >= 2 {
		sessIdx = 1
	}
	maxOut := payload.MaxCompletionTokens
	if maxOut <= 0 {
		maxOut = payload.MaxTokens
	}
	return &RequestAnalysis{
		Hashes:       hashes,
		CumEstTokens: cumEst,
		SessionKey:   hashes[sessIdx],
		TotalEst:     totalEst,
		CachedEst:    cachedTokens,
		MissEst:      missEst,
		MaxOutTokens: maxOut,
	}, true
}

type resolvedBackend struct {
	cfg    BackendConfig
	target *url.URL
	state  *BackendState
}

func backendLoad(s *BackendState) (heavy, reqs, total int64) {
	if s == nil {
		return 0, 0, 0
	}
	return s.activeHeavy.Load(), s.activeReqs.Load(), s.totalRouted.Load()
}

func (r *Router) selectBackend(an *RequestAnalysis, exclude map[string]bool) (*BackendState, string, bool) {
	backends := r.getBackends()
	resolved := make([]resolvedBackend, len(backends))
	for i, b := range backends {
		cfg, target := b.snapshot()
		resolved[i] = resolvedBackend{cfg: cfg, target: target, state: b}
	}
	cfg := r.getConfig()
	rb, reason, heavy := selectResolvedBackend(r, resolved, cfg.HeavyMissThreshold, spilloverThreshold(cfg), heavyStickyDuration(cfg), an, exclude)
	if rb == nil {
		return nil, reason, heavy
	}
	return rb.state, reason, heavy
}

func selectResolvedBackend(r *Router, backends []resolvedBackend, heavyThresh, spillThresh int, stickyDur time.Duration, an *RequestAnalysis, exclude map[string]bool) (*resolvedBackend, string, bool) {
	if len(backends) == 0 {
		return nil, "no-backends", false
	}
	if heavyThresh <= 0 {
		heavyThresh = 24000
	}

	var eligible []*resolvedBackend
	var allHealthy []*resolvedBackend
	for i := range backends {
		rb := &backends[i]
		if (rb.state != nil && !rb.state.healthy.Load()) || exclude[rb.cfg.ID] {
			continue
		}
		allHealthy = append(allHealthy, rb)
		if an.TotalEst <= rb.cfg.MaxSafeInput {
			eligible = append(eligible, rb)
		}
	}
	if len(eligible) == 0 {
		if len(allHealthy) == 0 {
			for i := len(backends) - 1; i >= 0; i-- {
				if !exclude[backends[i].cfg.ID] {
					return &backends[i], "fallback-unhealthy", false
				}
			}
			return nil, "no-backends", false
		}
		topTier := allHealthy[len(allHealthy)-1].cfg.Tier
		var topCands []*resolvedBackend
		for _, rb := range allHealthy {
			if rb.cfg.Tier == topTier {
				topCands = append(topCands, rb)
			}
		}
		sort.SliceStable(topCands, func(i, j int) bool {
			hi, ri, ti := backendLoad(topCands[i].state)
			hj, rj, tj := backendLoad(topCands[j].state)
			if hi != hj {
				return hi < hj
			}
			if ri != rj {
				return ri < rj
			}
			return ti < tj
		})
		return topCands[0], "fallback-largest", an.MissEst >= heavyThresh
	}

	isHeavyMiss := an.MissEst >= heavyThresh

	if isHeavyMiss {
		var idle []*resolvedBackend
		for _, rb := range eligible {
			if rb.state == nil || (rb.state.activeReqs.Load() == 0 && rb.state.activeHeavy.Load() == 0) {
				idle = append(idle, rb)
			}
		}
		if len(idle) > 0 {
			sort.SliceStable(idle, func(i, j int) bool {
				ci, cj := idle[i].cfg, idle[j].cfg
				if ci.SpeedScore != cj.SpeedScore {
					return ci.SpeedScore > cj.SpeedScore
				}
				if ci.Tier != cj.Tier {
					return ci.Tier > cj.Tier
				}
				_, _, ti := backendLoad(idle[i].state)
				_, _, tj := backendLoad(idle[j].state)
				return ti < tj
			})
			return idle[0], fmt.Sprintf("heavy-miss-idle(miss=%d)", an.MissEst), true
		}

		sort.SliceStable(eligible, func(i, j int) bool {
			hi, ri, ti := backendLoad(eligible[i].state)
			hj, rj, tj := backendLoad(eligible[j].state)
			if hi != hj {
				return hi < hj
			}
			if ri != rj {
				return ri < rj
			}
			if eligible[i].cfg.Tier != eligible[j].cfg.Tier {
				return eligible[i].cfg.Tier < eligible[j].cfg.Tier
			}
			return ti < tj
		})
		return eligible[0], fmt.Sprintf("heavy-miss-least-busy(miss=%d)", an.MissEst), true
	}

	targetTier := backends[0].cfg.Tier
	for _, rb := range backends {
		if an.TotalEst < rb.cfg.PromoteAt {
			targetTier = rb.cfg.Tier
			break
		}
		targetTier = rb.cfg.Tier
	}

	var sessTier int
	var lastBackend, heavyWriteBackend string
	var heavyWriteAt time.Time
	var hasSess bool
	if r != nil {
		r.mu.RLock()
		if sess, ok := r.sessions[an.SessionKey]; ok {
			sessTier = sess.Tier
			lastBackend = sess.LastBackend
			heavyWriteBackend = sess.HeavyWriteBackend
			heavyWriteAt = sess.HeavyWriteAt
			hasSess = true
		}
		r.mu.RUnlock()
	}

	// Short-term stickiness after a heavy-miss write so async Mooncake L3 flush
	// completes before the session migrates back to a lower-tier backend.
	if hasSess && heavyWriteBackend != "" && stickyDur > 0 && time.Since(heavyWriteAt) < stickyDur {
		for _, rb := range eligible {
			if rb.cfg.ID == heavyWriteBackend && (rb.state == nil || rb.state.activeHeavy.Load() == 0) {
				return rb, fmt.Sprintf("heavy-write-sticky(%s)", rb.cfg.ID), false
			}
		}
	}

	reason := fmt.Sprintf("tier%d-len(%d)", targetTier, an.TotalEst)
	if hasSess && sessTier > targetTier {
		demoteFloor := 0
		for _, rb := range backends {
			if rb.cfg.Tier == sessTier {
				demoteFloor = rb.cfg.DemoteBelow
				break
			}
		}
		if an.TotalEst >= demoteFloor {
			targetTier = sessTier
			reason = fmt.Sprintf("tier%d-sticky(%d)", targetTier, an.TotalEst)
		} else {
			reason = fmt.Sprintf("tier%d-demoted(%d<%d)", targetTier, an.TotalEst, demoteFloor)
		}
	} else if hasSess && targetTier > sessTier {
		reason = fmt.Sprintf("tier%d-promoted(%d)", targetTier, an.TotalEst)
	}

	pickLeastActive := func(cands []*resolvedBackend) *resolvedBackend {
		sort.SliceStable(cands, func(i, j int) bool {
			hi, ri, ti := backendLoad(cands[i].state)
			hj, rj, tj := backendLoad(cands[j].state)
			if hi != hj {
				return hi < hj
			}
			if ri != rj {
				return ri < rj
			}
			// Prefer the session's previous backend on load ties for direct L1 VRAM hit.
			if hasSess && lastBackend != "" {
				li := cands[i].cfg.ID == lastBackend
				lj := cands[j].cfg.ID == lastBackend
				if li != lj {
					return li
				}
			}
			return ti < tj
		})
		return cands[0]
	}

	var sameTier, upperTier []*resolvedBackend
	for _, rb := range eligible {
		if rb.cfg.Tier == targetTier {
			sameTier = append(sameTier, rb)
		} else if rb.cfg.Tier > targetTier {
			upperTier = append(upperTier, rb)
		}
	}

	if len(sameTier) > 0 {
		bestSame := pickLeastActive(sameTier)
		bestHeavy, bestReqs, _ := backendLoad(bestSame.state)
		sameBusy := (spillThresh > 0 && bestReqs >= int64(spillThresh)) || bestHeavy > 0
		if sameBusy && len(upperTier) > 0 {
			var spillCands []*resolvedBackend
			for _, ub := range upperTier {
				uh, ur, _ := backendLoad(ub.state)
				if uh > 0 {
					continue
				}
				if ur == 0 || ur+2 <= bestReqs {
					spillCands = append(spillCands, ub)
				}
			}
			if len(spillCands) > 0 {
				sort.SliceStable(spillCands, func(i, j int) bool {
					_, ri, ti := backendLoad(spillCands[i].state)
					_, rj, tj := backendLoad(spillCands[j].state)
					// Prefer truly idle upper-tier backends first; then lower tiers to protect top tier.
					if (ri == 0) != (rj == 0) {
						return ri == 0
					}
					if spillCands[i].cfg.Tier != spillCands[j].cfg.Tier {
						return spillCands[i].cfg.Tier < spillCands[j].cfg.Tier
					}
					if ri != rj {
						return ri < rj
					}
					return ti < tj
				})
				return spillCands[0], fmt.Sprintf("%s-spillover(t%d_busy=%d)", reason, targetTier, bestReqs), false
			}
		}
		return bestSame, reason, false
	}

	if len(upperTier) > 0 {
		sort.SliceStable(upperTier, func(i, j int) bool {
			if upperTier[i].cfg.Tier != upperTier[j].cfg.Tier {
				return upperTier[i].cfg.Tier < upperTier[j].cfg.Tier
			}
			hi, ri, ti := backendLoad(upperTier[i].state)
			hj, rj, tj := backendLoad(upperTier[j].state)
			if hi != hj {
				return hi < hj
			}
			if ri != rj {
				return ri < rj
			}
			return ti < tj
		})
		return upperTier[0], reason + "-failover-up", false
	}
	return eligible[len(eligible)-1], reason + "-failover", false
}

func (r *Router) recordCompletion(backends []resolvedBackend, an *RequestAnalysis, backendID string, backendTier int, isHeavyWriter, isTemporaryTier bool, heavyMissThreshold int, actualPrompt, actualCached int) {
	if r == nil || an == nil || len(an.Hashes) == 0 {
		return
	}
	if heavyMissThreshold <= 0 {
		heavyMissThreshold = 24000
	}
	var cfgs []BackendConfig
	if len(backends) > 0 {
		cfgs = make([]BackendConfig, len(backends))
		for i, rb := range backends {
			cfgs[i] = rb.cfg
		}
	} else {
		bs := r.getBackends()
		cfgs = make([]BackendConfig, len(bs))
		for i, b := range bs {
			cfgs[i] = b.config()
		}
	}
	now := time.Now()
	total := an.TotalEst
	if actualPrompt > 0 {
		total = actualPrompt
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(an.Hashes)
	estLast := an.CumEstTokens[n-1]
	scale := 1.0
	if actualPrompt > 0 && estLast > 0 {
		scale = float64(actualPrompt) / float64(estLast)
	}
	for i := 0; i < n; i++ {
		tok := int(float64(an.CumEstTokens[i]) * scale)
		if i == n-1 && actualPrompt > 0 {
			tok = actualPrompt
		}
		if tok < 1 {
			tok = 1
		}
		r.prefixes[an.Hashes[i]] = &PrefixEntry{
			Tokens:      tok,
			LastBackend: backendID,
			UpdatedAt:   now,
		}
	}

	homeTier := 1
	for _, bCfg := range cfgs {
		if total < bCfg.PromoteAt {
			homeTier = bCfg.Tier
			break
		}
		homeTier = bCfg.Tier
	}
	if prev, ok := r.sessions[an.SessionKey]; ok && prev.Tier > homeTier {
		demoteFloor := 0
		for _, bCfg := range cfgs {
			if bCfg.Tier == prev.Tier {
				demoteFloor = bCfg.DemoteBelow
				break
			}
		}
		if total >= demoteFloor {
			homeTier = prev.Tier
		}
	}
	if !isHeavyWriter && !isTemporaryTier && backendTier > homeTier {
		homeTier = backendTier
	}
	var heavyWriteBackend string
	var heavyWriteAt time.Time
	if prev, ok := r.sessions[an.SessionKey]; ok {
		heavyWriteBackend = prev.HeavyWriteBackend
		heavyWriteAt = prev.HeavyWriteAt
	}
	if isHeavyWriter || (actualPrompt > 0 && actualPrompt-actualCached >= heavyMissThreshold) {
		heavyWriteBackend = backendID
		heavyWriteAt = now
	}
	r.sessions[an.SessionKey] = &SessionEntry{
		Tier:              homeTier,
		LastTokens:        total,
		LastBackend:       backendID,
		HeavyWriteBackend: heavyWriteBackend,
		HeavyWriteAt:      heavyWriteAt,
		UpdatedAt:         now,
	}
}

func (r *Router) appendLog(entry RouteLogEntry) {
	r.mu.Lock()
	r.recentLogs = append(r.recentLogs, entry)
	if len(r.recentLogs) > 50 {
		r.recentLogs = r.recentLogs[len(r.recentLogs)-50:]
	}
	r.mu.Unlock()
}

func extractUsageFromTail(tail []byte) (promptTokens int, cachedTokens int) {
	idx := bytes.LastIndex(tail, []byte(`"usage"`))
	if idx < 0 {
		return 0, 0
	}
	sub := tail[idx:]
	brace := bytes.IndexByte(sub, '{')
	if brace < 0 {
		return 0, 0
	}
	dec := json.NewDecoder(bytes.NewReader(sub[brace:]))
	var u struct {
		PromptTokens        int `json:"prompt_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CachedTokens int `json:"cached_tokens"`
	}
	if err := dec.Decode(&u); err == nil && u.PromptTokens > 0 {
		ct := u.PromptTokensDetails.CachedTokens
		if ct == 0 {
			ct = u.CachedTokens
		}
		return u.PromptTokens, ct
	}
	return 0, 0
}

func isContextLengthError(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	low := strings.ToLower(string(body))
	return strings.Contains(low, "context length") ||
		strings.Contains(low, "maximum context") ||
		strings.Contains(low, "longer than the model") ||
		strings.Contains(low, "max_context_len") ||
		strings.Contains(low, "max_completion_tokens") ||
		strings.Contains(low, "max_tokens is too large") ||
		strings.Contains(low, "completion tokens") ||
		(strings.Contains(low, "too large") && strings.Contains(low, "supports at most"))
}

func clampMaxTokensForBackend(body []byte, estInput int, maxOut int, maxCtx int, isHighestTier bool) ([]byte, int) {
	if maxCtx <= 0 || maxOut <= 0 {
		return body, 0
	}
	margin := estInput/8 + 4096
	if margin < 4096 {
		margin = 4096
	}
	safeCap := maxCtx - estInput - margin
	if maxOut <= safeCap && maxOut < maxCtx {
		return body, 0
	}
	minAcceptable := 8192
	if isHighestTier {
		minAcceptable = 1024
	}
	if safeCap < minAcceptable {
		if isHighestTier && safeCap > 256 {
			// Best-effort fit on highest tier
		} else {
			return body, 0
		}
	}
	if safeCap >= maxCtx {
		safeCap = maxCtx - 4096
	}
	if safeCap <= 0 || maxOut <= safeCap {
		return body, 0
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, 0
	}
	changed := false
	capBytes := []byte(fmt.Sprintf("%d", safeCap))
	for _, field := range []string{"max_completion_tokens", "max_tokens"} {
		if raw, ok := obj[field]; ok {
			var v int
			if json.Unmarshal(raw, &v) == nil && v > safeCap {
				obj[field] = capBytes
				changed = true
			}
		}
	}
	if !changed {
		return body, 0
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body, 0
	}
	return out, safeCap
}

func (r *Router) pickHighestHealthyBackend() *BackendState {
	backends := r.getBackends()
	if len(backends) == 0 {
		return nil
	}
	for i := len(backends) - 1; i >= 0; i-- {
		if backends[i].healthy.Load() {
			return backends[i]
		}
	}
	return backends[len(backends)-1]
}

func (r *Router) recordShadow(predictedID, predictedReason, actualID, actualReason string, an *RequestAnalysis) {
	if an == nil {
		return
	}
	r.shadowMu.Lock()
	defer r.shadowMu.Unlock()
	if actualID == "" {
		r.shadowUnknown++
		return
	}
	r.shadowCompared++
	cnt := r.shadowByActual[actualID]
	cnt.Total++
	if predictedID == actualID {
		r.shadowMatched++
		cnt.Matched++
	} else {
		r.shadowMismatches = append(r.shadowMismatches, ShadowMismatch{
			Time:            time.Now().Format(time.RFC3339),
			Predicted:       predictedID,
			PredictedReason: predictedReason,
			Actual:          actualID,
			ActualReason:    actualReason,
			TotalEst:        an.TotalEst,
			MissEst:         an.MissEst,
		})
		if len(r.shadowMismatches) > 50 {
			r.shadowMismatches = r.shadowMismatches[len(r.shadowMismatches)-50:]
		}
	}
	r.shadowByActual[actualID] = cnt
}

// ResetShadow zeroes shadow comparison statistics.
func (r *Router) ResetShadow() {
	r.shadowMu.Lock()
	r.shadowSince = time.Now()
	r.shadowCompared = 0
	r.shadowMatched = 0
	r.shadowUnknown = 0
	r.shadowByActual = make(map[string]ShadowBackendCount)
	r.shadowMismatches = nil
	r.shadowMu.Unlock()
}

// Status returns a complete snapshot of tiered router status for admin UI/API.
func (r *Router) Status() ProviderStatus {
	cfg := r.getConfig()
	backends := r.getBackends()
	bs := make([]BackendStatus, 0, len(backends))
	for _, b := range backends {
		bCfg := b.config()
		he, _ := b.lastHealthErr.Load().(string)
		bs = append(bs, BackendStatus{
			ID:               bCfg.ID,
			URL:              bCfg.URL,
			Tier:             bCfg.Tier,
			MaxContextTokens: bCfg.MaxContextTokens,
			MaxSafeInput:     bCfg.MaxSafeInput,
			PromoteAt:        bCfg.PromoteAt,
			DemoteBelow:      bCfg.DemoteBelow,
			SpeedScore:       bCfg.SpeedScore,
			Healthy:          b.healthy.Load(),
			LastHealthErr:    he,
			ActiveReqs:       b.activeReqs.Load(),
			ActiveHeavy:      b.activeHeavy.Load(),
			TotalRouted:      b.totalRouted.Load(),
			HeavyRouted:      b.heavyRouted.Load(),
		})
	}

	r.mu.RLock()
	cachedPrefixes := len(r.prefixes)
	trackedSessions := len(r.sessions)
	recent := append([]RouteLogEntry(nil), r.recentLogs...)
	r.mu.RUnlock()
	if recent == nil {
		recent = []RouteLogEntry{}
	}

	r.shadowMu.Lock()
	byActualCopy := make(map[string]ShadowBackendCount, len(r.shadowByActual))
	for k, v := range r.shadowByActual {
		byActualCopy[k] = v
	}
	mismatchesCopy := append([]ShadowMismatch(nil), r.shadowMismatches...)
	if mismatchesCopy == nil {
		mismatchesCopy = []ShadowMismatch{}
	}
	matchRate := 0.0
	if r.shadowCompared > 0 {
		matchRate = float64(r.shadowMatched) * 100.0 / float64(r.shadowCompared)
	}
	shadow := ShadowStats{
		Since:      r.shadowSince.Format(time.RFC3339),
		Compared:   r.shadowCompared,
		Matched:    r.shadowMatched,
		Unknown:    r.shadowUnknown,
		MatchRate:  matchRate,
		ByActual:   byActualCopy,
		Mismatches: mismatchesCopy,
	}
	r.shadowMu.Unlock()

	return ProviderStatus{
		Provider:           r.name,
		Mode:               cfg.Mode,
		HeavyMissThreshold: cfg.HeavyMissThreshold,
		HeavyStickySec:     cfg.HeavyStickySec,
		SpilloverThreshold: cfg.SpilloverThreshold,
		SSEKeepaliveSec:    cfg.SSEKeepaliveSec,
		HealthIntervalSec:  cfg.HealthIntervalSec,
		ZhThinkAnchor:      zhAnchorEnabled(cfg),
		Backends:           bs,
		CachedPrefixes:     cachedPrefixes,
		TrackedSessions:    trackedSessions,
		RecentRoutes:       recent,
		Shadow:             shadow,
		StarterCache:       r.starters.Summary(),
		ContextCompress:    r.compressor.Summary(),
	}
}

func (r *Router) Starters() []StarterView {
	return r.starters.List()
}

func (r *Router) PinStarter(hash string, pinned bool, label string) (StarterView, error) {
	return r.starters.Pin(hash, pinned, label)
}

func (r *Router) DeleteStarter(hash string) (string, error) {
	return r.starters.Delete(hash)
}

func (r *Router) WarmStarter(hash string, force bool) ([]WarmResult, error) {
	if r.getConfig().Mode != "active" {
		return nil, ErrShadowMode
	}
	return r.starters.Warm(hash, force), nil
}

func (r *Router) Checkpoints() ([]CheckpointView, map[string]any) {
	return r.compressor.List(), r.compressor.Summary()
}

func (r *Router) DeleteCheckpoint(id string) (string, error) {
	return r.compressor.Delete(id)
}

func (r *Router) ClearCheckpoints() int {
	return r.compressor.Clear()
}
