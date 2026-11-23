package tiered

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/keystore"
)

type ctxKey int

const (
	dataClassCtxKey   ctxKey = 1
	routeMetaCtxKey   ctxKey = 2
	callerScopeCtxKey ctxKey = 3
)

// RouteMeta records the final backend and routing reason across keepalive failovers.
type RouteMeta struct {
	mu      sync.RWMutex
	backend string
	reason  string
}

func (m *RouteMeta) Set(backend, reason string) {
	if m == nil || backend == "" {
		return
	}
	m.mu.Lock()
	m.backend = backend
	m.reason = reason
	m.mu.Unlock()
}

func (m *RouteMeta) Get() (string, string) {
	if m == nil {
		return "", ""
	}
	m.mu.RLock()
	b, r := m.backend, m.reason
	m.mu.RUnlock()
	return b, r
}

// WithRouteMeta attaches a mutable RouteMeta holder to ctx for Pipeline logging.
func WithRouteMeta(ctx context.Context, m *RouteMeta) context.Context {
	if m == nil {
		return ctx
	}
	return context.WithValue(ctx, routeMetaCtxKey, m)
}

func routeMetaFromContext(ctx context.Context) *RouteMeta {
	v, _ := ctx.Value(routeMetaCtxKey).(*RouteMeta)
	return v
}

// WithDataClass annotates ctx with the request data classification so starter
// prefix capture can exclude confidential prompts.
func WithDataClass(ctx context.Context, dataClass string) context.Context {
	return context.WithValue(ctx, dataClassCtxKey, dataClass)
}

func dataClassFromContext(ctx context.Context) string {
	v, _ := ctx.Value(dataClassCtxKey).(string)
	return v
}

// DataClassFromContext returns the Gatehouse data classification stored on ctx, if any.
func DataClassFromContext(ctx context.Context) string {
	return dataClassFromContext(ctx)
}

// WithCallerScope attaches an opaque caller/security scope key so context
// compression checkpoints never cross caller or classification boundaries.
func WithCallerScope(ctx context.Context, scope string) context.Context {
	if scope == "" {
		return ctx
	}
	return context.WithValue(ctx, callerScopeCtxKey, scope)
}

func callerScopeFromContext(ctx context.Context) string {
	v, _ := ctx.Value(callerScopeCtxKey).(string)
	return v
}

// Hub owns one Router per tiered-configured provider and manages lifecycle across config reloads.
type Hub struct {
	dataDir   string
	log       *slog.Logger
	transport *http.Transport

	mu            sync.RWMutex
	routers       map[string]*Router
	warmupFilter  WarmupFilterFunc
	warmupRestore WarmupRestoreFunc
	resolveKey    func(ref string) (string, error)
}

// SetWarmupFilter installs a filter invoked on starter warm-up payloads before dispatch.
func (h *Hub) SetWarmupFilter(fn WarmupFilterFunc) {
	h.mu.Lock()
	h.warmupFilter = fn
	list := make([]*Router, 0, len(h.routers))
	for _, r := range h.routers {
		list = append(list, r)
	}
	h.mu.Unlock()
	for _, r := range list {
		r.setWarmupFilter(fn)
	}
}

// SetWarmupRestore installs a callback invoked to restore redacted placeholders before local checkpoint freezing.
func (h *Hub) SetWarmupRestore(fn WarmupRestoreFunc) {
	h.mu.Lock()
	h.warmupRestore = fn
	list := make([]*Router, 0, len(h.routers))
	for _, r := range h.routers {
		list = append(list, r)
	}
	h.mu.Unlock()
	for _, r := range list {
		r.setWarmupRestore(fn)
	}
}

// SetKeyResolver installs a resolver for provider key_ref values used by probes and starter warmups.
func (h *Hub) SetKeyResolver(fn func(ref string) (string, error)) {
	h.mu.Lock()
	h.resolveKey = fn
	h.mu.Unlock()
}

// NewHub builds a Hub for managing tiered provider routers. No goroutines start until Activate.
func NewHub(dataDir string, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		dataDir:    dataDir,
		log:        log,
		transport:  newDefaultTransport(),
		routers:    make(map[string]*Router),
		resolveKey: keystore.Resolve,
	}
}

type planBackend struct {
	cfg      BackendConfig
	target   *url.URL
	fallback *BackendState
}

type snapshotPlan struct {
	provider string
	target   string
	cfg      RouterConfig
	backends []planBackend
}

func newSnapshotPlan(provider, target string, tc config.TieredConfig, defaultPersist string) (*snapshotPlan, error) {
	norm := normalizeRouterConfig(tc, defaultPersist)
	if target == "" {
		target = config.DefaultProviderTarget(&config.Provider{Tiered: &norm})
	}
	backends := make([]planBackend, 0, len(norm.Backends))
	for _, bc := range norm.Backends {
		u, err := url.Parse(bc.URL)
		if err != nil {
			return nil, fmt.Errorf("invalid backend url %q: %w", bc.URL, err)
		}
		fb := &BackendState{cfg: bc, target: u}
		fb.healthy.Store(true)
		backends = append(backends, planBackend{
			cfg:      bc,
			target:   u,
			fallback: fb,
		})
	}
	sort.Slice(backends, func(i, j int) bool {
		return backends[i].cfg.Tier < backends[j].cfg.Tier
	})
	return &snapshotPlan{
		provider: provider,
		target:   target,
		cfg:      norm,
		backends: backends,
	}, nil
}

func (p *snapshotPlan) resolve(r *Router) []resolvedBackend {
	out := make([]resolvedBackend, len(p.backends))
	for i, pb := range p.backends {
		st := r.backendFor(pb.cfg.ID, pb.target)
		if st == nil {
			st = pb.fallback
		}
		out[i] = resolvedBackend{
			cfg:    pb.cfg,
			target: pb.target,
			state:  st,
		}
	}
	return out
}

func pickHighestHealthyResolved(backends []resolvedBackend) *resolvedBackend {
	if len(backends) == 0 {
		return nil
	}
	for i := len(backends) - 1; i >= 0; i-- {
		if backends[i].state == nil || backends[i].state.healthy.Load() {
			return &backends[i]
		}
	}
	return &backends[len(backends)-1]
}

// Transport returns an http.RoundTripper bound to the given config snapshot for provider.
// It is pure and safe to call during config preview/validation.
func (h *Hub) Transport(provider, target string, cfg *config.TieredConfig, upstream http.RoundTripper) (http.RoundTripper, error) {
	if upstream == nil {
		upstream = http.DefaultTransport
	}
	if cfg == nil {
		return upstream, nil
	}
	defaultPersist := filepath.Join(h.dataDir, "tiered", provider+"-starters.json")
	plan, err := newSnapshotPlan(provider, target, *cfg, defaultPersist)
	if err != nil {
		return nil, err
	}
	return &hubTransport{hub: h, provider: provider, plan: plan, upstream: upstream}, nil
}

// Activate synchronizes active tiered routers with cfg (starting loops only for the live snapshot).
func (h *Hub) Activate(cfg *config.Config) {
	if cfg == nil {
		return
	}
	type desiredSpec struct {
		tc     *config.TieredConfig
		target string
		keyRef string
	}
	desired := map[string]desiredSpec{}
	for i := range cfg.Providers {
		pc := &cfg.Providers[i]
		if pc.Tiered != nil {
			tgt := pc.Target
			if tgt == "" {
				tgt = config.DefaultProviderTarget(pc)
			}
			desired[pc.Name] = desiredSpec{tc: pc.Tiered, target: tgt, keyRef: pc.KeyRef}
		}
	}

	h.mu.Lock()
	filter := h.warmupFilter
	restore := h.warmupRestore
	resolveFn := h.resolveKey
	var toClose []*Router
	for name, r := range h.routers {
		if _, keep := desired[name]; !keep {
			toClose = append(toClose, r)
			delete(h.routers, name)
		}
	}
	existing := make(map[string]*Router, len(h.routers))
	for name, r := range h.routers {
		existing[name] = r
	}
	h.mu.Unlock()

	for _, r := range toClose {
		r.close()
	}

	for name, spec := range desired {
		apiKey := ""
		if spec.keyRef != "" && resolveFn != nil {
			k, err := resolveFn(spec.keyRef)
			if err != nil {
				h.log.Warn("tiered provider key resolve failed", "provider", name, "err", err)
			} else {
				apiKey = k
			}
		}
		if r, ok := existing[name]; ok {
			r.setWarmupFilter(filter)
			r.setWarmupRestore(restore)
			r.setKeyResolver(resolveFn)
			if err := r.apply(spec.target, apiKey, *spec.tc); err != nil {
				h.log.Error("tiered router apply failed", "provider", name, "err", err)
			}
			continue
		}
		defPath := filepath.Join(h.dataDir, "tiered", name+"-starters.json")
		r, err := newRouterWithTarget(name, spec.target, apiKey, *spec.tc, defPath, h.transport, h.log)
		if err != nil {
			h.log.Error("tiered router init failed", "provider", name, "err", err)
			continue
		}
		r.setWarmupFilter(filter)
		r.setWarmupRestore(restore)
		r.setKeyResolver(resolveFn)
		r.ensureLoops()
		h.mu.Lock()
		// double-check race
		if old, dup := h.routers[name]; dup {
			h.mu.Unlock()
			r.close()
			old.setWarmupFilter(filter)
			old.setWarmupRestore(restore)
			old.setKeyResolver(resolveFn)
			_ = old.apply(spec.target, apiKey, *spec.tc)
			continue
		}
		h.routers[name] = r
		h.mu.Unlock()
	}
}

// Router looks up an active tiered router by provider name.
func (h *Hub) Router(name string) (*Router, bool) {
	h.mu.RLock()
	r, ok := h.routers[name]
	h.mu.RUnlock()
	return r, ok
}

// Statuses returns status snapshots for all active tiered providers, sorted by name.
func (h *Hub) Statuses() []ProviderStatus {
	h.mu.RLock()
	names := make([]string, 0, len(h.routers))
	list := make([]*Router, 0, len(h.routers))
	for n, r := range h.routers {
		names = append(names, n)
		_ = r
	}
	sort.Strings(names)
	for _, n := range names {
		list = append(list, h.routers[n])
	}
	h.mu.RUnlock()

	out := make([]ProviderStatus, 0, len(list))
	for _, r := range list {
		out = append(out, r.Status())
	}
	return out
}

// Close shuts down background loops and flushes persisted starters to disk.
func (h *Hub) Close() error {
	h.mu.Lock()
	all := make([]*Router, 0, len(h.routers))
	for _, r := range h.routers {
		all = append(all, r)
	}
	h.routers = make(map[string]*Router)
	h.mu.Unlock()

	for _, r := range all {
		r.close()
	}
	h.transport.CloseIdleConnections()
	return nil
}

type hubTransport struct {
	hub      *Hub
	provider string
	plan     *snapshotPlan
	upstream http.RoundTripper
}

func (t *hubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.plan == nil {
		return t.upstream.RoundTrip(req)
	}
	r, _ := t.hub.Router(t.provider)
	if t.plan.cfg.Mode == "active" {
		return t.roundTripActive(req, r)
	}
	return t.roundTripShadow(req, r)
}

func readAndRestoreBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxBodyBytes))
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	return body, err
}

func targetChatURL(target *url.URL, reqURL *url.URL) string {
	u := *target
	if strings.HasSuffix(reqURL.Path, "/chat/completions") {
		base := strings.TrimRight(target.Path, "/")
		if strings.HasSuffix(base, "/v1") {
			u.Path = base + "/chat/completions"
		} else {
			u.Path = base + "/v1/chat/completions"
		}
	} else {
		base := strings.TrimRight(target.Path, "/")
		if strings.HasSuffix(base, "/v1") && (reqURL.Path == "/v1" || strings.HasPrefix(reqURL.Path, "/v1/")) {
			u.Path = base + strings.TrimPrefix(reqURL.Path, "/v1")
		} else {
			u.Path = base + reqURL.Path
		}
	}
	if reqURL.RawQuery != "" {
		u.RawQuery = reqURL.RawQuery
	}
	return u.String()
}

func backendChatCompletionsURL(target *url.URL, rawURL string) string {
	if target == nil {
		u, err := url.Parse(rawURL)
		if err != nil {
			base := strings.TrimRight(rawURL, "/")
			if strings.HasSuffix(base, "/v1") {
				return base + "/chat/completions"
			}
			return base + "/v1/chat/completions"
		}
		target = u
	}
	return targetChatURL(target, &url.URL{Path: "/v1/chat/completions"})
}

func syntheticJSONResp(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func (t *hubTransport) roundTripActive(req *http.Request, r *Router) (*http.Response, error) {
	start := time.Now()
	body, err := readAndRestoreBody(req)
	if err != nil {
		return nil, err
	}
	resolved := t.plan.resolve(r)
	passHighest := func(reqBody []byte) (*http.Response, error) {
		rb := pickHighestHealthyResolved(resolved)
		if rb == nil {
			return syntheticJSONResp(req, http.StatusServiceUnavailable, `{"error":{"message":"no healthy backend available","type":"tiered_unavailable"}}`), nil
		}
		outReq, rerr := http.NewRequestWithContext(req.Context(), req.Method, targetChatURL(rb.target, req.URL), bytes.NewReader(reqBody))
		if rerr != nil {
			return nil, rerr
		}
		outReq.Header = req.Header.Clone()
		outReq.ContentLength = int64(len(reqBody))
		return t.hub.transport.RoundTrip(outReq)
	}

	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return passHighest(body)
	}

	scCfg := t.plan.cfg.StarterCache
	toolsReordered := false
	if scCfg != nil && scCfg.CanonicalizeTools {
		if canonBody, changed, _ := canonicalizeTools(body); changed {
			body = canonBody
			toolsReordered = true
			if r != nil {
				r.starters.toolsReordered.Add(1)
			}
		}
	}

	var starterTmpl *StarterTemplate
	if scCfg != nil && scCfg.Enabled && dataClassFromContext(req.Context()) != "confidential" {
		starterTmpl = extractStarterTemplate(body, req.Header.Get("User-Agent"), scCfg.MinTokens, scCfg.MaxTokens)
		if starterTmpl != nil {
			starterTmpl.OriginTarget = t.plan.target
		}
	}

	rawAn, ok := r.analyzeChatBody(body)
	if !ok {
		return passHighest(body)
	}

	effectiveBody := body
	an := rawAn
	var compRes CompressResult
	compEnabled := t.plan.cfg.ContextCompress != nil && t.plan.cfg.ContextCompress.Enabled
	if compEnabled && r != nil && r.compressor != nil {
		compRes = r.compressor.MaybeCompressWithScope(
			body, rawAn, req.Header, *t.plan.cfg.ContextCompress,
			t.plan.cfg.HeavyMissThreshold, t.plan.target,
			dataClassFromContext(req.Context()) == "confidential",
			callerScopeFromContext(req.Context()),
		)
		if compRes.Compressed && len(compRes.Body) > 0 {
			effectiveBody = compRes.Body
			if newAn, ok2 := r.analyzeChatBody(effectiveBody); ok2 {
				an = newAn
			}
		}
	}

	sendBody := effectiveBody
	zhAnchored := false
	if zhAnchorEnabled(t.plan.cfg) {
		if anchoredBody, changed := injectZhThinkAnchor(effectiveBody); changed {
			sendBody = anchoredBody
			zhAnchored = true
		}
	}

	if len(resolved) == 0 {
		return syntheticJSONResp(req, http.StatusServiceUnavailable, `{"error":{"message":"no healthy backend available","type":"tiered_unavailable"}}`), nil
	}
	highestTier := resolved[len(resolved)-1].cfg.Tier
	stickyDur := heavyStickyDuration(t.plan.cfg)
	keepaliveEvery := sseKeepaliveInterval(t.plan.cfg.SSEKeepaliveSec)
	initDelay := 20 * time.Second
	if r != nil {
		keepaliveEvery = r.getSSEKeepaliveInterval(t.plan.cfg.SSEKeepaliveSec)
		initDelay = r.getSSEKeepaliveInitDelay()
	}
	exclude := make(map[string]bool)
	routeMeta := routeMetaFromContext(req.Context())
	logger := t.hub.log
	if r != nil {
		logger = r.log
	}

	attemptIdx := 0
	maxAttempts := len(resolved)
	nextAttempt := func() (*activeAttempt, bool, error) {
		for attemptIdx < maxAttempts {
			if cerr := req.Context().Err(); cerr != nil {
				return nil, false, cerr
			}
			rb, reason, isHeavy := selectResolvedBackend(r, resolved, t.plan.cfg.HeavyMissThreshold, stickyDur, an, exclude)
			if rb == nil {
				return nil, true, nil
			}
			bCfg, targetURL, backend := rb.cfg, rb.target, rb.state

			if backend != nil {
				backend.activeReqs.Add(1)
				backend.totalRouted.Add(1)
			}
			var heavyReleased atomic.Bool
			var activeReleased atomic.Bool
			if backend != nil && isHeavy {
				backend.activeHeavy.Add(1)
				backend.heavyRouted.Add(1)
			}
			attemptCtx, attemptCancel := context.WithCancel(req.Context())
			releaseHeavy := func() {
				if backend != nil && isHeavy && heavyReleased.CompareAndSwap(false, true) {
					backend.activeHeavy.Add(-1)
				}
			}
			releaseActive := func() {
				releaseHeavy()
				if backend != nil && activeReleased.CompareAndSwap(false, true) {
					backend.activeReqs.Add(-1)
				}
				attemptCancel()
			}

			isHighestTier := bCfg.Tier >= highestTier
			attemptBody, clampedMaxOut := clampMaxTokensForBackend(sendBody, an.TotalEst, an.MaxOutTokens, bCfg.MaxContextTokens, isHighestTier)

			outReq, rerr := http.NewRequestWithContext(attemptCtx, req.Method, targetChatURL(targetURL, req.URL), bytes.NewReader(attemptBody))
			if rerr != nil {
				releaseActive()
				exclude[bCfg.ID] = true
				attemptIdx++
				continue
			}
			outReq.Header = req.Header.Clone()
			outReq.Header.Del("Transfer-Encoding")
			outReq.ContentLength = int64(len(attemptBody))

			finishActive := func(completed bool, tail []byte, statusCode int) {
				releaseActive()
				actualPrompt, actualCached := extractUsageFromTail(tail)
				if r != nil && completed && statusCode >= 200 && statusCode < 300 {
					isStickyWrite := strings.HasPrefix(reason, "heavy-write-sticky(")
					r.recordCompletion(resolved, an, bCfg.ID, bCfg.Tier, isHeavy, isStickyWrite, t.plan.cfg.HeavyMissThreshold, actualPrompt, actualCached)
					if starterTmpl != nil {
						r.starters.Observe(starterTmpl)
					}
					if compRes.PendingJob != nil && r.compressor != nil {
						r.compressor.ScheduleStage2(compRes.PendingJob)
					}
				}
				durMs := time.Since(start).Milliseconds()
				logger.Info("tiered route", "provider", t.provider, "mode", "active", "backend", bCfg.ID, "reason", reason,
					"compress", compRes.Mode, "orig_est", compRes.OriginalTokens,
					"est", an.TotalEst, "cached_est", an.CachedEst, "miss_est", an.MissEst,
					"actual_prompt", actualPrompt, "actual_cached", actualCached, "status", statusCode, "ms", durMs)
				if r != nil {
					r.appendLog(RouteLogEntry{
						Time:         time.Now().Format("15:04:05"),
						Backend:      bCfg.ID,
						Reason:       reason,
						CompressMode: compRes.Mode,
						OrigEst:      compRes.OriginalTokens,
						TotalEst:     an.TotalEst,
						CachedEst:    an.CachedEst,
						MissEst:      an.MissEst,
						ActualPrompt: actualPrompt,
						ActualCache:  actualCached,
						Status:       statusCode,
						DurationMs:   durMs,
					})
				}
			}

			attemptIdx++
			return &activeAttempt{
				bCfg:          bCfg,
				reason:        reason,
				clampedMaxOut: clampedMaxOut,
				attemptBody:   attemptBody,
				outReq:        outReq,
				cancel:        attemptCancel,
				releaseHeavy:  releaseHeavy,
				releaseActive: releaseActive,
				finishActive:  finishActive,
			}, false, nil
		}
		return nil, false, nil
	}

	firstTry := true
	for {
		cur, noHealthy, ctxErr := nextAttempt()
		if ctxErr != nil {
			return nil, ctxErr
		}
		if cur == nil {
			if firstTry && noHealthy {
				return syntheticJSONResp(req, http.StatusServiceUnavailable, `{"error":{"message":"no healthy backend available","type":"tiered_unavailable"}}`), nil
			}
			break
		}
		firstTry = false

		var resp *http.Response
		var rerr error
		if isStreamRequest(cur.attemptBody) && keepaliveEvery > 0 {
			call := t.startRTCall(cur.outReq, cur.cancel)
			timer := time.NewTimer(initDelay)
			select {
			case res := <-call.ch:
				timer.Stop()
				resp, rerr = res.resp, res.err
			case <-req.Context().Done():
				timer.Stop()
				call.abort()
				cur.releaseActive()
				return nil, req.Context().Err()
			case <-timer.C:
				routeMeta.Set(cur.bCfg.ID, cur.reason)
				return t.startKeepaliveStream(&keepaliveFailoverState{
					req:            req,
					logger:         logger,
					an:             an,
					toolsReordered: toolsReordered,
					zhAnchored:     zhAnchored,
					compRes:        compRes,
					compEnabled:    compEnabled,
					highestTier:    highestTier,
					maxAttempts:    maxAttempts,
					resolved:       resolved,
					exclude:        exclude,
					routeMeta:      routeMeta,
					keepaliveEvery: keepaliveEvery,
					canRetry:       func() bool { return attemptIdx < maxAttempts },
					nextAttempt:    nextAttempt,
				}, cur, call), nil
			}
		} else {
			resp, rerr = t.hub.transport.RoundTrip(cur.outReq)
		}

		if rerr != nil {
			cur.releaseActive()
			if req.Context().Err() != nil || errors.Is(rerr, context.Canceled) {
				logger.Info("tiered request canceled by client, skipping failover", "provider", t.provider, "backend", cur.bCfg.ID, "err", rerr)
				return nil, rerr
			}
			logger.Warn("tiered backend roundtrip failed, failing over", "provider", t.provider, "attempt", attemptIdx-1, "backend", cur.bCfg.ID, "err", rerr)
			exclude[cur.bCfg.ID] = true
			continue
		}

		if resp.StatusCode == http.StatusBadRequest && cur.bCfg.Tier < highestTier {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
			if isContextLengthError(resp.StatusCode, errBody) {
				cur.releaseActive()
				logger.Info("backend returned context length error, retrying larger tier", "provider", t.provider, "backend", cur.bCfg.ID, "est", an.TotalEst)
				excludeLowerOrEqualTiers(exclude, resolved, cur.bCfg.Tier)
				continue
			}
			cur.releaseActive()
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
			resp.ContentLength = int64(len(errBody))
			setRouterHeaders(resp.Header, cur.bCfg.ID, cur.reason, an, toolsReordered, cur.clampedMaxOut, zhAnchored, compRes, compEnabled)
			routeMeta.Set(cur.bCfg.ID, cur.reason)
			return resp, nil
		}

		if shouldFailoverBadGateway(resp.StatusCode) && attemptIdx < maxAttempts {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			cur.releaseActive()
			logger.Warn("tiered backend bad gateway status, failing over", "provider", t.provider, "backend", cur.bCfg.ID, "status", resp.StatusCode)
			exclude[cur.bCfg.ID] = true
			continue
		}

		setRouterHeaders(resp.Header, cur.bCfg.ID, cur.reason, an, toolsReordered, cur.clampedMaxOut, zhAnchored, compRes, compEnabled)
		routeMeta.Set(cur.bCfg.ID, cur.reason)
		resp.Body = &trackedBody{
			inner:        resp.Body,
			releaseHeavy: cur.releaseHeavy,
			onDone: func(completed bool, tail []byte) {
				cur.finishActive(completed, tail, resp.StatusCode)
			},
		}
		return resp, nil
	}

	return syntheticJSONResp(req, http.StatusBadGateway, `{"error":{"message":"all backends failed","type":"tiered_bad_gateway"}}`), nil
}

func (t *hubTransport) roundTripShadow(req *http.Request, r *Router) (*http.Response, error) {
	start := time.Now()
	origBody, err := readAndRestoreBody(req)
	if err != nil {
		return nil, err
	}

	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return t.upstream.RoundTrip(req)
	}

	scCfg := t.plan.cfg.StarterCache
	workBody := append([]byte(nil), origBody...)
	if scCfg != nil && scCfg.CanonicalizeTools {
		if canonBody, changed, _ := canonicalizeTools(workBody); changed {
			workBody = canonBody
			if r != nil {
				r.starters.toolsReordered.Add(1)
			}
		}
	}
	var starterTmpl *StarterTemplate
	if scCfg != nil && scCfg.Enabled && dataClassFromContext(req.Context()) != "confidential" {
		starterTmpl = extractStarterTemplate(workBody, req.Header.Get("User-Agent"), scCfg.MinTokens, scCfg.MaxTokens)
		if starterTmpl != nil {
			starterTmpl.OriginTarget = t.plan.target
		}
	}
	an, ok := r.analyzeChatBody(workBody)
	resolved := t.plan.resolve(r)
	var predID, predReason string
	if ok {
		if pb, pr, _ := selectResolvedBackend(r, resolved, t.plan.cfg.HeavyMissThreshold, heavyStickyDuration(t.plan.cfg), an, nil); pb != nil {
			predID = pb.cfg.ID
			predReason = pr
		}
	}

	// Ensure upstream receives exact ORIGINAL bytes unmodified.
	req.Body = io.NopCloser(bytes.NewReader(origBody))
	req.ContentLength = int64(len(origBody))

	resp, err := t.upstream.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if !ok {
		return resp, nil
	}

	actualID := resp.Header.Get("X-Router-Backend")
	actualReason := resp.Header.Get("X-Router-Reason")
	if actualID != "" {
		routeMetaFromContext(req.Context()).Set(actualID, actualReason)
	}
	if predID != "" {
		resp.Header.Set("X-Router-Shadow-Backend", predID)
		resp.Header.Set("X-Router-Shadow-Reason", predReason)
	}
	if r != nil {
		r.recordShadow(predID, predReason, actualID, actualReason, an)
	}

	var actualBackend *BackendState
	actualTier := 0
	for i := range resolved {
		if resolved[i].cfg.ID == actualID {
			actualBackend = resolved[i].state
			actualTier = resolved[i].cfg.Tier
			break
		}
	}
	if actualBackend == nil && r != nil {
		if b := r.backendByID(actualID); b != nil {
			actualBackend = b
			actualTier = b.config().Tier
		}
	}
	actualHeavy := strings.HasPrefix(actualReason, "heavy-miss")
	var heavyReleased atomic.Bool
	if actualBackend != nil {
		actualBackend.activeReqs.Add(1)
		actualBackend.totalRouted.Add(1)
		if actualHeavy {
			actualBackend.activeHeavy.Add(1)
			actualBackend.heavyRouted.Add(1)
		}
	}
	releaseHeavy := func() {
		if actualBackend != nil && actualHeavy && heavyReleased.CompareAndSwap(false, true) {
			actualBackend.activeHeavy.Add(-1)
		}
	}

	resp.Body = &trackedBody{
		inner:        resp.Body,
		releaseHeavy: releaseHeavy,
		onDone: func(completed bool, tail []byte) {
			releaseHeavy()
			if actualBackend != nil {
				actualBackend.activeReqs.Add(-1)
			}
			actualPrompt, actualCached := extractUsageFromTail(tail)
			if r != nil && completed && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if actualID != "" {
					isStickyWrite := strings.HasPrefix(actualReason, "heavy-write-sticky(")
					r.recordCompletion(resolved, an, actualID, actualTier, actualHeavy, isStickyWrite, t.plan.cfg.HeavyMissThreshold, actualPrompt, actualCached)
				}
				if starterTmpl != nil {
					r.starters.Observe(starterTmpl)
				}
			}
			durMs := time.Since(start).Milliseconds()
			if r != nil && actualID != "" {
				r.appendLog(RouteLogEntry{
					Time:         time.Now().Format("15:04:05"),
					Backend:      actualID,
					Reason:       actualReason,
					TotalEst:     an.TotalEst,
					CachedEst:    an.CachedEst,
					MissEst:      an.MissEst,
					ActualPrompt: actualPrompt,
					ActualCache:  actualCached,
					Status:       resp.StatusCode,
					DurationMs:   durMs,
				})
			}
		},
	}
	return resp, nil
}

func setContextHeaders(h http.Header, cr CompressResult, enabled bool) {
	if !enabled && !cr.Compressed && cr.CheckpointID == "" {
		return
	}
	if cr.Compressed {
		h.Set("X-Context-Compressed", cr.Mode)
		h.Set("X-Context-Compressed-Tokens", fmt.Sprintf("%d", cr.CompressedTokens))
		saved := cr.OriginalTokens - cr.CompressedTokens
		if saved < 0 {
			saved = 0
		}
		h.Set("X-Context-Saved-Tokens", fmt.Sprintf("%d", saved))
	} else {
		h.Set("X-Context-Compressed", "0")
		if cr.SkipReason != "" && cr.SkipReason != "below-threshold" && cr.SkipReason != "disabled" {
			h.Set("X-Context-Skip-Reason", cr.SkipReason)
		}
	}
	if cr.OriginalTokens > 0 {
		h.Set("X-Context-Original-Tokens", fmt.Sprintf("%d", cr.OriginalTokens))
	}
	if cr.CheckpointID != "" {
		h.Set("X-Context-Checkpoint-Id", shortID(cr.CheckpointID))
	}
	if cr.Stage2Status != "" && cr.Stage2Status != "none" {
		h.Set("X-Context-Stage2-Status", cr.Stage2Status)
	}
}

func setRouterHeaders(h http.Header, backendID, reason string, an *RequestAnalysis, toolsReordered bool, clampedMaxOut int, zhAnchored bool, cr CompressResult, compressEnabled bool) {
	h.Set("X-Router-Backend", backendID)
	h.Set("X-Router-Reason", reason)
	h.Set("X-Router-Est-Tokens", fmt.Sprintf("%d", an.TotalEst))
	h.Set("X-Router-Miss-Tokens", fmt.Sprintf("%d", an.MissEst))
	if toolsReordered {
		h.Set("X-Router-Tools-Canonicalized", "1")
	}
	if clampedMaxOut > 0 {
		h.Set("X-Router-Max-Tokens-Clamped", fmt.Sprintf("%d->%d", an.MaxOutTokens, clampedMaxOut))
	}
	if zhAnchored {
		h.Set("X-Router-Zh-Anchor", "1")
	}
	setContextHeaders(h, cr, compressEnabled)
}

type activeRTRes struct {
	resp *http.Response
	err  error
}

type rtCall struct {
	mu       sync.Mutex
	canceled bool
	cancel   context.CancelFunc
	ch       chan activeRTRes
}

func (t *hubTransport) startRTCall(outReq *http.Request, cancel context.CancelFunc) *rtCall {
	call := &rtCall{
		cancel: cancel,
		ch:     make(chan activeRTRes, 1),
	}
	go func() {
		rr, ee := t.hub.transport.RoundTrip(outReq)
		call.mu.Lock()
		if call.canceled {
			call.mu.Unlock()
			if rr != nil && rr.Body != nil {
				_ = rr.Body.Close()
			}
			return
		}
		call.ch <- activeRTRes{resp: rr, err: ee}
		call.mu.Unlock()
	}()
	return call
}

func (c *rtCall) abort() {
	c.mu.Lock()
	c.canceled = true
	cancel := c.cancel
	var late activeRTRes
	select {
	case late = <-c.ch:
	default:
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if late.resp != nil && late.resp.Body != nil {
		_ = late.resp.Body.Close()
	}
}

type bodyChunk struct {
	data []byte
	err  error
}

type activeAttempt struct {
	bCfg          BackendConfig
	reason        string
	clampedMaxOut int
	attemptBody   []byte
	outReq        *http.Request
	cancel        context.CancelFunc
	releaseHeavy  func()
	releaseActive func()
	finishActive  func(completed bool, tail []byte, statusCode int)
}

type keepaliveFailoverState struct {
	req            *http.Request
	logger         *slog.Logger
	an             *RequestAnalysis
	toolsReordered bool
	zhAnchored     bool
	compRes        CompressResult
	compEnabled    bool
	highestTier    int
	maxAttempts    int
	resolved       []resolvedBackend
	exclude        map[string]bool
	routeMeta      *RouteMeta
	keepaliveEvery time.Duration
	canRetry       func() bool
	nextAttempt    func() (*activeAttempt, bool, error)
}

func shouldFailoverBadGateway(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func excludeLowerOrEqualTiers(exclude map[string]bool, resolved []resolvedBackend, tier int) {
	for _, other := range resolved {
		if other.cfg.Tier <= tier {
			exclude[other.cfg.ID] = true
		}
	}
}

type cancelPipeReader struct {
	*io.PipeReader
	closed chan struct{}
	once   sync.Once
}

func (c *cancelPipeReader) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.PipeReader.Close()
}

func isStreamRequest(body []byte) bool {
	if !bytes.Contains(body, []byte(`"stream"`)) {
		return false
	}
	var hdr struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &hdr) == nil && hdr.Stream
}

func marshalSSEErrorChunk(msg string) []byte {
	payload, _ := marshalNoEscapeHTML(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "upstream_error",
		},
	})
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(payload)
	buf.WriteString("\n\ndata: [DONE]\n\n")
	return buf.Bytes()
}

func appendTail(tail, chunk []byte) []byte {
	if len(chunk) >= maxBodyTail {
		return append(tail[:0], chunk[len(chunk)-maxBodyTail:]...)
	}
	tail = append(tail, chunk...)
	if len(tail) > maxBodyTail {
		return append([]byte(nil), tail[len(tail)-maxBodyTail:]...)
	}
	return tail
}

func (t *hubTransport) startKeepaliveStream(
	st *keepaliveFailoverState,
	cur *activeAttempt,
	curCall *rtCall,
) *http.Response {
	pr, pw := io.Pipe()
	cpr := &cancelPipeReader{PipeReader: pr, closed: make(chan struct{})}
	hdr := make(http.Header)
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Router-SSE-Keepalive", "1")
	setRouterHeaders(hdr, cur.bCfg.ID, cur.reason, st.an, st.toolsReordered, cur.clampedMaxOut, st.zhAnchored, st.compRes, st.compEnabled)

	go func() {
		if _, err := pw.Write([]byte(": keepalive\n\n")); err != nil {
			curCall.abort()
			cur.releaseActive()
			_ = pw.Close()
			return
		}
		tk := time.NewTicker(st.keepaliveEvery)
		defer tk.Stop()

		var winResp *http.Response
		retryNext := func(excludeLower bool, fallbackStatus int, fallbackMsg string) bool {
			if excludeLower {
				excludeLowerOrEqualTiers(st.exclude, st.resolved, cur.bCfg.Tier)
			} else {
				st.exclude[cur.bCfg.ID] = true
			}
			if st.canRetry() {
				nxt, _, nextErr := st.nextAttempt()
				if nextErr != nil {
					_ = pw.CloseWithError(nextErr)
					return false
				}
				if nxt != nil {
					cur = nxt
					st.routeMeta.Set(cur.bCfg.ID, cur.reason)
					curCall = t.startRTCall(cur.outReq, cur.cancel)
					return true
				}
			}
			cur.finishActive(false, nil, fallbackStatus)
			_, _ = pw.Write(marshalSSEErrorChunk(fallbackMsg))
			_ = pw.Close()
			return false
		}

		for {
			var up activeRTRes
		waitUp:
			for {
				select {
				case up = <-curCall.ch:
					break waitUp
				case <-cpr.closed:
					curCall.abort()
					cur.releaseActive()
					_ = pw.Close()
					return
				case <-st.req.Context().Done():
					curCall.abort()
					cur.releaseActive()
					_ = pw.CloseWithError(st.req.Context().Err())
					return
				case <-tk.C:
					if _, err := pw.Write([]byte(": keepalive\n\n")); err != nil {
						curCall.abort()
						cur.releaseActive()
						_ = pw.Close()
						return
					}
				}
			}

			if up.err != nil {
				cur.releaseActive()
				if st.req.Context().Err() != nil || errors.Is(up.err, context.Canceled) {
					st.logger.Info("tiered request canceled by client, skipping failover", "provider", t.provider, "backend", cur.bCfg.ID, "err", up.err)
					_ = pw.CloseWithError(up.err)
					return
				}
				st.logger.Warn("tiered backend roundtrip failed after keepalive, failing over", "provider", t.provider, "backend", cur.bCfg.ID, "err", up.err)
				if retryNext(false, http.StatusBadGateway, "all backends failed") {
					continue
				}
				return
			}

			if up.resp.StatusCode == http.StatusBadRequest && cur.bCfg.Tier < st.highestTier {
				errBody, _ := io.ReadAll(io.LimitReader(up.resp.Body, 64*1024))
				_ = up.resp.Body.Close()
				if isContextLengthError(http.StatusBadRequest, errBody) {
					cur.releaseActive()
					st.logger.Info("backend returned context length error after keepalive, retrying larger tier", "provider", t.provider, "backend", cur.bCfg.ID, "est", st.an.TotalEst)
					if retryNext(true, http.StatusBadRequest, "upstream status 400") {
						continue
					}
					return
				}
				cur.finishActive(false, nil, http.StatusBadRequest)
				_, _ = pw.Write(marshalSSEErrorChunk("upstream status 400"))
				_ = pw.Close()
				return
			}

			if shouldFailoverBadGateway(up.resp.StatusCode) && st.canRetry() {
				_, _ = io.Copy(io.Discard, up.resp.Body)
				_ = up.resp.Body.Close()
				cur.releaseActive()
				st.logger.Warn("tiered backend bad gateway status after keepalive, failing over", "provider", t.provider, "backend", cur.bCfg.ID, "status", up.resp.StatusCode)
				if retryNext(false, up.resp.StatusCode, fmt.Sprintf("upstream status %d", up.resp.StatusCode)) {
					continue
				}
				return
			}

			if up.resp.StatusCode >= 400 {
				_, _ = io.Copy(io.Discard, io.LimitReader(up.resp.Body, 64*1024))
				_ = up.resp.Body.Close()
				cur.finishActive(false, nil, up.resp.StatusCode)
				_, _ = pw.Write(marshalSSEErrorChunk(fmt.Sprintf("upstream status %d", up.resp.StatusCode)))
				_ = pw.Close()
				return
			}

			winResp = up.resp
			break
		}

		chunkCh := make(chan bodyChunk, 1)
		stopRead := make(chan struct{})
		var stopOnce sync.Once
		stopReader := func() {
			stopOnce.Do(func() { close(stopRead) })
		}
		go func() {
			defer func() {
				_ = winResp.Body.Close()
				cur.cancel()
				close(chunkCh)
			}()
			buf := make([]byte, 32*1024)
			for {
				n, rerr := winResp.Body.Read(buf)
				var payload []byte
				if n > 0 {
					payload = append([]byte(nil), buf[:n]...)
				}
				if n > 0 || rerr != nil {
					select {
					case chunkCh <- bodyChunk{data: payload, err: rerr}:
					case <-stopRead:
						return
					}
				}
				if rerr != nil {
					return
				}
			}
		}()

		var tail []byte
		firstSeen := false
		for {
			select {
			case ch, ok := <-chunkCh:
				if !ok {
					cur.finishActive(true, tail, winResp.StatusCode)
					_ = pw.Close()
					return
				}
				if len(ch.data) > 0 {
					if !firstSeen {
						firstSeen = true
						tk.Stop()
						cur.releaseHeavy()
					}
					tail = appendTail(tail, ch.data)
					if _, werr := pw.Write(ch.data); werr != nil {
						stopReader()
						cur.cancel()
						cur.finishActive(false, tail, winResp.StatusCode)
						_ = pw.CloseWithError(werr)
						return
					}
				}
				if ch.err != nil {
					completed := errors.Is(ch.err, io.EOF)
					cur.finishActive(completed, tail, winResp.StatusCode)
					if completed {
						_ = pw.Close()
					} else {
						_ = pw.CloseWithError(ch.err)
					}
					return
				}
			case <-cpr.closed:
				stopReader()
				cur.cancel()
				cur.finishActive(false, tail, winResp.StatusCode)
				_ = pw.Close()
				return
			case <-st.req.Context().Done():
				stopReader()
				cur.cancel()
				cur.finishActive(false, tail, winResp.StatusCode)
				_ = pw.CloseWithError(st.req.Context().Err())
				return
			case <-tk.C:
				if !firstSeen {
					if _, err := pw.Write([]byte(": keepalive\n\n")); err != nil {
						stopReader()
						cur.cancel()
						cur.finishActive(false, tail, winResp.StatusCode)
						_ = pw.Close()
						return
					}
				}
			}
		}
	}()

	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     hdr,
		Body:       cpr,
		Request:    st.req,
	}
}

const maxBodyTail = 16384

type trackedBody struct {
	inner        io.ReadCloser
	releaseHeavy func()
	onDone       func(completed bool, tail []byte)

	mu        sync.Mutex
	tail      []byte
	firstSeen bool
	doneOnce  sync.Once
}

func (tb *trackedBody) Read(p []byte) (int, error) {
	n, err := tb.inner.Read(p)
	if n > 0 {
		tb.mu.Lock()
		if !tb.firstSeen {
			tb.firstSeen = true
			if tb.releaseHeavy != nil {
				tb.releaseHeavy()
			}
		}
		tb.tail = appendTail(tb.tail, p[:n])
		tb.mu.Unlock()
	}
	if err == io.EOF {
		tb.finish(true)
	} else if err != nil {
		tb.finish(false)
	}
	return n, err
}

func (tb *trackedBody) Close() error {
	err := tb.inner.Close()
	tb.finish(false)
	return err
}

func (tb *trackedBody) finish(completed bool) {
	tb.doneOnce.Do(func() {
		tb.mu.Lock()
		tailCopy := append([]byte(nil), tb.tail...)
		tb.mu.Unlock()
		if tb.onDone != nil {
			tb.onDone(completed, tailCopy)
		}
	})
}
