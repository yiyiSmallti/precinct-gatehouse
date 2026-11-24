// Package api assembles the HTTP server: the OpenAI-compatible endpoints
// plus a health check.
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/audit"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/compat"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/keys"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/keystore"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/limit"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/live"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/meter"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/pipeline"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/plugin"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/policy"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/route"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/store"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/tiered"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/token"
	sdk "github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

// Server is a running gateway.
type Server struct {
	HTTP       *http.Server
	Pipeline   *pipeline.Pipeline
	Live       *live.Manager
	Tiered     *tiered.Hub
	DB         *sql.DB
	Keys       *keys.Store
	Limiter    *limit.Limiter
	UserKey    []byte
	Missing    []string // required detectors that are unavailable
	PolicyDown bool     // org mode: the policy plugin did not start
	closers    []func()
	otlp       *meter.OTLP
}

// ResolveKey resolves a provider key_ref, reading db:NAME from settings and
// delegating other references (env:, keychain:, file:) to keystore.Resolve.
func ResolveKey(db *sql.DB, ref string) (string, error) {
	if name, ok := strings.CutPrefix(ref, "db:"); ok {
		if db == nil {
			return "", fmt.Errorf("database unavailable for key_ref %q", ref)
		}
		var v string
		err := db.QueryRow(`SELECT v FROM settings WHERE k=?`, "provider_key:"+name).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("provider key %q is not set in database", name)
		}
		if err != nil {
			return "", err
		}
		return v, nil
	}
	return keystore.Resolve(ref)
}

// New wires every component from a validated config.
func New(cfg *config.Config, version string, log *slog.Logger) (*Server, error) {
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{DB: db, closers: []func(){func() { db.Close() }}}
	cleanupOnErr := true
	defer func() {
		if cleanupOnErr {
			s.Shutdown(context.Background())
		}
	}()

	keyStore := keys.NewStore(db)
	s.Keys = keyStore
	s.closers = append(s.closers, keyStore.Close)
	limiter := limit.New(db)
	s.Limiter = limiter
	issuer, err := token.NewIssuer(cfg.DataDir, db)
	if err != nil {
		return nil, err
	}
	m := meter.New(db)
	if cfg.Meter.OTLP != nil {
		o, err := meter.NewOTLP(cfg.Meter.OTLP, version)
		if err != nil {
			return nil, fmt.Errorf("meter.otlp: %w", err)
		}
		m.AddExporter(o)
		s.otlp = o
	}
	fileSink, err := audit.NewFileSink(cfg.Audit.Dir)
	if err != nil {
		return nil, err
	}
	host := &plugin.Host{}
	s.closers = append(s.closers, host.Close)
	sink := audit.NewForwarder(fileSink, log)
	s.closers = append(s.closers, func() { sink.Close(5 * time.Second) })
	sinks, failedSinks := host.StartKind(cfg, sdk.KindAuditSink, log)
	for _, as := range sinks {
		sink.Add(as.Name, as.Impl.(sdk.AuditSink))
	}
	if len(failedSinks) > 0 {
		log.Warn("audit sink plugins unavailable; records stay in the local audit file", "plugins", failedSinks)
	}
	provPlugins, _ := host.StartKind(cfg, sdk.KindProvider, log)
	var hooks []pipeline.Hook
	hookPlugins, failedHooks := host.StartKind(cfg, sdk.KindHooks, log)
	for _, hp := range hookPlugins {
		hooks = append(hooks, pipeline.Hook{Name: hp.Name, Impl: hp.Impl.(sdk.Hooks), Exited: hp.Exited})
	}
	for _, n := range failedHooks {
		// a hook that did not start refuses requests rather than being skipped
		hooks = append(hooks, pipeline.Hook{Name: n, Exited: func() bool { return true }})
	}
	var resolver *policy.Resolver
	if cfg.Mode == "org" {
		pols, failed := host.StartKind(cfg, sdk.KindAuthPolicy, log)
		switch {
		case len(pols) == 1:
			resolver = policy.New(cfg, pols[0].Name, pols[0])
		case len(failed) == 1:
			resolver = policy.New(cfg, failed[0], nil)
			s.PolicyDown = true
		default:
			return nil, fmt.Errorf("org mode needs exactly one auth_policy plugin")
		}
	}
	detectors, missing := host.StartDetectors(cfg, log)
	s.Missing = missing
	userKey, err := loadOrCreateKey(filepath.Join(cfg.DataDir, "user.key"))
	if err != nil {
		return nil, err
	}
	s.UserKey = userKey

	hub := tiered.NewHub(cfg.DataDir, log)
	s.Tiered = hub
	s.closers = append(s.closers, func() { _ = hub.Close() })

	resolveKey := func(ref string) (string, error) {
		return ResolveKey(db, ref)
	}
	hub.SetKeyResolver(resolveKey)
	buildSnapshot := func(nextCfg *config.Config, prev *live.Snapshot) (*live.Snapshot, error) {
		reg, err := provider.NewRegistry(nextCfg, resolveKey)
		if err != nil {
			return nil, err
		}
		for _, pc := range nextCfg.Providers {
			if pc.Tiered != nil {
				tr, err := hub.Transport(pc.Name, pc.Target, pc.Tiered, nil)
				if err != nil {
					return nil, fmt.Errorf("provider %s tiered: %w", pc.Name, err)
				}
				reg.SetTransport(pc.Name, tr)
			}
		}
		for _, pc := range nextCfg.Providers {
			if pc.Plugin == "" {
				continue
			}
			var found *plugin.Started
			for _, pp := range provPlugins {
				if pp.Name == pc.Plugin {
					found = pp
					break
				}
			}
			if found == nil {
				if prev != nil {
					return nil, fmt.Errorf("provider 插件 %s 未运行，需重启 gatehouse", pc.Plugin)
				}
				reg.SetPlugin(pc.Name, plugin.Unavailable(pc.Plugin))
				continue
			}
			reg.SetPlugin(pc.Name, plugin.ProviderChat(found, pc.Timeout.D(300*time.Second)))
		}
		snapMissing := computeMissingDetectors(nextCfg.Redaction.Detectors, detectors)
		red, err := redact.New(nextCfg.Redaction, detectors, snapMissing)
		if err != nil {
			return nil, fmt.Errorf("redaction: %w", err)
		}
		if prev != nil && prev.Redactor != nil {
			red.Vault = prev.Redactor.Vault
		}
		rt := route.New(nextCfg, reg)
		rt.Prepare = func(text, target, dataClass string) (string, error) {
			// the classifier sees the last user message: redact it like any other outbound text
			mode := red.Mode(target, dataClass)
			req := oai.Object{"messages": []any{map[string]any{"role": "user", "content": text}}}
			if _, err := red.Apply(context.Background(), req, mode, userKey, "classifier"); err != nil {
				return "", err
			}
			return oai.Text(oai.Messages(req)[0]), nil
		}
		return &live.Snapshot{Cfg: nextCfg, Reg: reg, Router: rt, Redactor: red}, nil
	}

	liveMgr, err := live.New(cfg, buildSnapshot)
	if err != nil {
		return nil, err
	}
	s.Live = liveMgr
	if r := liveMgr.Current().Redactor; r != nil {
		s.Missing = r.Missing()
	}
	pruneCh := make(chan struct{}, 1)
	if days := liveMgr.Current().Cfg.LogRetentionDays(); days > 0 {
		if err := m.PruneRequestLog(days, time.Now()); err != nil {
			log.Warn("request_log prune failed", "err", err)
		}
	}
	pruneCtx, cancelPrune := context.WithCancel(context.Background())
	s.closers = append(s.closers, cancelPrune)
	go func() {
		tk := time.NewTicker(24 * time.Hour)
		defer tk.Stop()
		for {
			var now time.Time
			select {
			case <-pruneCtx.Done():
				return
			case now = <-tk.C:
			case <-pruneCh:
				now = time.Now()
			}
			if days := liveMgr.Current().Cfg.LogRetentionDays(); days > 0 {
				if err := m.PruneRequestLog(days, now); err != nil {
					log.Warn("request_log prune failed", "err", err)
				}
			}
		}
	}()
	liveMgr.OnSwap = func(snap *live.Snapshot) {
		if snap.Redactor != nil && snap.Redactor.Vault != nil {
			snap.Redactor.Vault.SetTTL(snap.Redactor.VaultTTL())
		}
		hub.Activate(snap.Cfg)
		if snap.Cfg.LogRetentionDays() > 0 {
			select {
			case pruneCh <- struct{}{}:
			default:
			}
		}
	}
	hub.SetWarmupFilter(func(ctx context.Context, providerName, backendURL string, payload []byte) ([]byte, error) {
		snap := liveMgr.Current()
		if snap == nil || snap.Redactor == nil {
			return payload, nil
		}
		p := snap.Cfg.Provider(providerName)
		target := "external"
		if p != nil && p.Target != "" {
			target = p.Target
		}
		if backendURL != "" && !config.IsLoopbackURL(backendURL) {
			target = "external"
		}
		dc := tiered.DataClassFromContext(ctx)
		if dc == "" {
			dc = "internal"
		}
		mode := snap.Redactor.Mode(target, dc)
		if mode == redact.ModeOff {
			return payload, nil
		}
		obj, err := oai.Decode(payload)
		if err != nil {
			return nil, err
		}
		res, err := snap.Redactor.Apply(ctx, obj, mode, userKey, "tiered-warmup:"+providerName)
		if err != nil {
			return nil, err
		}
		if !res.Changed {
			return payload, nil
		}
		return oai.Encode(obj), nil
	})
	hub.SetWarmupRestore(func(providerName, text string) string {
		if !strings.Contains(text, "{{") {
			return text
		}
		snap := liveMgr.Current()
		if snap == nil || snap.Redactor == nil || snap.Redactor.Vault == nil {
			return text
		}
		bucket := snap.Redactor.Vault.BucketWithTTL("tiered-warmup:"+providerName, snap.Redactor.VaultTTL())
		if bucket.Len() == 0 {
			return text
		}
		restored, _ := redact.RestoreText(text, bucket, nil)
		return restored
	})
	hub.Activate(liveMgr.Current().Cfg)
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	s.closers = append(s.closers, cancelWatch)
	go liveMgr.Watch(watchCtx, log)

	p := &pipeline.Pipeline{Live: liveMgr, Tokens: issuer, Keys: keyStore, Limiter: limiter, Meter: m, Audit: sink, UserKey: userKey, Log: log, Policy: resolver, Hooks: hooks}
	s.Pipeline = p

	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", p)
	mux.HandleFunc("POST /v1/responses", s.openaiResponses)
	mux.HandleFunc("POST /v1/messages", s.anthropicMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.anthropicCountTokens)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /api/usage/token", s.newapiTokenUsage)
	mux.HandleFunc("GET /api/usage/token/", s.newapiTokenUsage)
	mux.HandleFunc("GET /healthz", s.healthz)
	s.HTTP = &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	cleanupOnErr = false
	return s, nil
}

// LocalToken returns the personal-mode token, issuing it on first start.
func (s *Server) LocalToken() (string, bool, error) {
	return s.Pipeline.Tokens.LocalToken(s.Live.Current().Cfg.DataDir)
}

// Shutdown stops serving, flushes metrics and kills plugins.
func (s *Server) Shutdown(ctx context.Context) {
	if s.HTTP != nil {
		_ = s.HTTP.Shutdown(ctx)
	}
	if s.otlp != nil {
		_ = s.otlp.Shutdown(ctx)
	}
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
}

func (s *Server) openaiResponses(w http.ResponseWriter, r *http.Request) {
	tr := compat.NewResponsesTranslator("")
	cw := compat.NewWriter(w, tr)
	s.Pipeline.Serve(cw, r, "responses", func(b []byte) (oai.Object, error) {
		req, meta, err := compat.DecodeResponses(b)
		tr.Model, tr.CustomTools = meta.Model, meta.CustomTools
		if len(meta.DroppedTools) > 0 {
			cw.Header().Set("X-Gatehouse-Dropped-Tools", strings.Join(meta.DroppedTools, ", "))
		}
		if len(meta.DroppedItems) > 0 {
			cw.Header().Set("X-Gatehouse-Dropped-Items", strings.Join(meta.DroppedItems, ", "))
		}
		return req, err
	})
	cw.Finish()
}

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	tr := compat.NewAnthropicTranslator("")
	cw := compat.NewWriter(w, tr)
	s.Pipeline.Serve(cw, r, "messages", func(b []byte) (oai.Object, error) {
		req, model, err := compat.DecodeAnthropic(b)
		tr.Model = model
		return req, err
	})
	cw.Finish()
}

func (s *Server) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	tok := pipeline.Bearer(r)
	if tok == "" {
		compat.WriteAnthropicError(w, http.StatusUnauthorized, "missing gateway token (Authorization: Bearer <token> or x-api-key)")
		return
	}
	if pol := s.Pipeline.Policy; pol != nil {
		if strings.HasPrefix(tok, "sk-") {
			compat.WriteAnthropicError(w, http.StatusUnauthorized, "api keys are not supported in org mode")
			return
		}
		if _, err := pol.Resolve(r.Context(), tok, &sdk.RequestMeta{}); err != nil {
			var pe *policy.Error
			if errors.As(err, &pe) {
				compat.WriteAnthropicError(w, pe.Status, pe.Msg)
			} else {
				compat.WriteAnthropicError(w, http.StatusServiceUnavailable, err.Error())
			}
			return
		}
	} else if strings.HasPrefix(tok, "sk-") {
		if _, err := s.Keys.Lookup(tok); err != nil {
			compat.WriteAnthropicError(w, http.StatusUnauthorized, err.Error())
			return
		}
	} else {
		if _, err := s.Pipeline.Tokens.Verify(tok); err != nil {
			compat.WriteAnthropicError(w, http.StatusUnauthorized, err.Error())
			return
		}
	}
	snap := s.Live.Current()
	rc := r.Body
	if maxBytes := snap.Cfg.MaxBodyBytes(); maxBytes > 0 {
		rc = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		be := pipeline.BodyReadError(r.Context(), err)
		compat.WriteAnthropicError(w, be.Status, be.Message)
		return
	}
	req, _, err := compat.DecodeAnthropic(body)
	if err != nil {
		compat.WriteAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(oai.Messages(req)) == 0 {
		compat.WriteAnthropicError(w, http.StatusBadRequest, "messages is required")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(oai.Encode(map[string]any{"input_tokens": oai.EstimateTokens(req)}))
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	snap := s.Live.Current()
	tok := pipeline.Bearer(r)
	var claims token.Claims
	if pol := s.Pipeline.Policy; pol != nil {
		if strings.HasPrefix(tok, "sk-") {
			oai.WriteError(w, oai.Errorf(http.StatusUnauthorized, "invalid_token", "api keys are not supported in org mode"))
			return
		}
		pc, err := pol.Resolve(r.Context(), tok, &sdk.RequestMeta{})
		if err != nil {
			var pe *policy.Error
			if errors.As(err, &pe) {
				oai.WriteError(w, oai.Errorf(pe.Status, pe.Code, "%s", pe.Msg))
			} else {
				oai.WriteError(w, oai.Errorf(http.StatusServiceUnavailable, "policy_unavailable", "%v", err))
			}
			return
		}
		// empty lists mean "nothing" in org mode, unlike personal token scopes
		claims.Scope = token.Scope{Groups: append([]string{"\x00"}, pc.AllowedGroups...), Models: append([]string{"\x00"}, pc.Models...)}
		for _, g := range pc.AllowedGroups {
			claims.Scope.Models = append(claims.Scope.Models, snap.Router.Members(g)...)
		}
	} else if strings.HasPrefix(tok, "sk-") {
		k, err := s.Keys.Lookup(tok)
		if err != nil {
			code := "invalid_api_key"
			if errors.Is(err, keys.ErrDisabled) {
				code = "key_disabled"
			} else if errors.Is(err, keys.ErrExpired) {
				code = "key_expired"
			}
			oai.WriteError(w, oai.Errorf(http.StatusUnauthorized, code, "%v", err))
			return
		}
		claims.Scope = token.Scope{Models: k.Models, Groups: k.Groups}
	} else {
		c, err := s.Pipeline.Tokens.Verify(tok)
		if err != nil {
			code := "invalid_token"
			if errors.Is(err, token.ErrExpired) {
				code = "token_expired"
			}
			oai.WriteError(w, oai.Errorf(http.StatusUnauthorized, code, "%v", err))
			return
		}
		claims = c
	}
	cfg := snap.Cfg
	var ids []string
	seen := map[string]bool{}
	addID := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if cfg.DefaultGroup != "" && claims.Scope.AllowsModel(cfg.DefaultGroup, true) {
		addID("auto")
	}
	var groups []string
	for g := range cfg.RoutingGroups {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		if claims.Scope.AllowsModel(g, true) {
			addID(g)
		}
	}
	for _, pc := range cfg.Providers {
		for _, m := range pc.Models {
			ref := pc.Name + "/" + m
			if claims.Scope.AllowsModel(ref, false) {
				addID(ref)
			}
		}
	}
	for _, a := range cfg.ModelAliases {
		if strings.ContainsAny(a.Match, "*?[\\") {
			continue
		}
		allowed := false
		switch {
		case a.Use == "auto":
			allowed = cfg.DefaultGroup != "" && claims.Scope.AllowsModel(cfg.DefaultGroup, true)
		case cfg.RoutingGroups[a.Use] != nil:
			allowed = claims.Scope.AllowsModel(a.Use, true)
		default:
			allowed = claims.Scope.AllowsModel(a.Use, false)
		}
		if allowed {
			addID(a.Match)
		}
	}
	data := make([]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "gatehouse"})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(oai.Encode(map[string]any{"object": "list", "data": data}))
}

func (s *Server) newapiTokenUsage(w http.ResponseWriter, r *http.Request) {
	tok := pipeline.Bearer(r)
	if tok == "" {
		if raw := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(raw, "sk-") {
			tok = raw
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if s.Keys == nil || !strings.HasPrefix(tok, "sk-") {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(oai.Encode(map[string]any{"code": false, "message": "invalid token", "data": nil}))
		return
	}
	k, err := s.Keys.Lookup(tok)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(oai.Encode(map[string]any{"code": false, "message": "invalid token", "data": nil}))
		return
	}
	modelLimits := map[string]bool{}
	for _, m := range k.Models {
		modelLimits[m] = true
	}
	unlimitedQuota := true
	totalGranted := 500000000
	totalUsed := 0
	if k.RPD > 0 {
		unlimitedQuota = false
		totalGranted = k.RPD
		if s.Limiter != nil {
			totalUsed = s.Limiter.Snapshot(k.ID, time.Now()).TodayUsed
		}
	}
	totalAvailable := max(0, totalGranted-totalUsed)
	w.Write(oai.Encode(map[string]any{
		"code":    true,
		"message": "ok",
		"data": map[string]any{
			"expires_at":           k.ExpiresAt,
			"model_limits":         modelLimits,
			"model_limits_enabled": len(k.Models) > 0,
			"name":                 k.Name,
			"object":               "token_usage",
			"total_available":      totalAvailable,
			"total_granted":        totalGranted,
			"total_used":           totalUsed,
			"unlimited_quota":      unlimitedQuota,
			"user_quota": map[string]any{
				"total_available": totalAvailable,
				"total_used":      totalUsed,
			},
		},
	}))
}

// PruneRequestLogNow enforces the live snapshot's request_log_days retention immediately.
func (s *Server) PruneRequestLogNow(now time.Time) error {
	if s.Live == nil || s.Pipeline == nil || s.Pipeline.Meter == nil {
		return nil
	}
	days := s.Live.Current().Cfg.LogRetentionDays()
	if days <= 0 {
		return nil
	}
	return s.Pipeline.Meter.PruneRequestLog(days, now)
}

func computeMissingDetectors(required []string, running []redact.ExternalDetector) []string {
	if len(required) == 0 {
		return nil
	}
	active := make(map[string]bool, len(running))
	for _, d := range running {
		if ex, ok := d.(interface{ Exited() bool }); ok && ex.Exited() {
			continue
		}
		active[d.Name()] = true
	}
	seen := make(map[string]bool, len(required))
	var missing []string
	for _, name := range required {
		if seen[name] || active[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	return missing
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	snap := s.Live.Current()
	var missing []string
	if snap.Redactor != nil {
		missing = snap.Redactor.Missing()
	} else {
		missing = s.Missing
	}
	status := "ok"
	if len(missing) > 0 {
		status = "degraded"
	}
	out := map[string]any{"status": status, "mode": snap.Cfg.Mode, "config_version": snap.Version, "detectors_unavailable": missing}
	if pol := s.Pipeline.Policy; pol != nil {
		out["policy"] = map[string]any{"plugin": pol.Name(), "available": pol.Available()}
		if !pol.Available() {
			out["status"] = "unavailable"
		}
	}
	w.Write(oai.Encode(out))
}

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	k := make([]byte, 32)
	if _, err := randRead(k); err != nil {
		return nil, err
	}
	return k, os.WriteFile(path, k, 0o600)
}
