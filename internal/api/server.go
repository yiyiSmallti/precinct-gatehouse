// Package api assembles the HTTP server: the OpenAI-compatible endpoints
// plus a health check.
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/audit"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/keys"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/limit"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/meter"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/pipeline"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/plugin"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/policy"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/route"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/store"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/token"
	sdk "github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

// Server is a running gateway.
type Server struct {
	HTTP       *http.Server
	Pipeline   *pipeline.Pipeline
	DB         *sql.DB
	Keys       *keys.Store
	Limiter    *limit.Limiter
	Reg        *provider.Registry
	Missing    []string // required detectors that are unavailable
	PolicyDown bool     // org mode: the policy plugin did not start
	closers    []func()
	otlp       *meter.OTLP
}

// New wires every component from a validated config.
func New(cfg *config.Config, version string, log *slog.Logger) (*Server, error) {
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{DB: db, closers: []func(){func() { db.Close() }}}
	keyStore := keys.NewStore(db)
	s.Keys = keyStore
	s.closers = append(s.closers, keyStore.Close)
	limiter := limit.New(db)
	s.Limiter = limiter
	issuer, err := token.NewIssuer(cfg.DataDir, db)
	if err != nil {
		return nil, err
	}
	reg, err := provider.NewRegistry(cfg)
	if err != nil {
		return nil, err
	}
	s.Reg = reg
	m := meter.New(db)
	if days := cfg.LogRetentionDays(); days > 0 {
		if err := m.PruneRequestLog(days, time.Now()); err != nil {
			log.Warn("request_log prune failed", "err", err)
		}
		pruneCtx, cancelPrune := context.WithCancel(context.Background())
		s.closers = append(s.closers, cancelPrune)
		go func() {
			tk := time.NewTicker(24 * time.Hour)
			defer tk.Stop()
			for {
				select {
				case <-pruneCtx.Done():
					return
				case now := <-tk.C:
					if err := m.PruneRequestLog(days, now); err != nil {
						log.Warn("request_log prune failed", "err", err)
					}
				}
			}
		}()
	}
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
	for _, pc := range cfg.Providers {
		if pc.Plugin == "" {
			continue
		}
		fn := plugin.Unavailable(pc.Plugin)
		for _, pp := range provPlugins {
			if pp.Name == pc.Plugin {
				fn = plugin.ProviderChat(pp, pc.Timeout.D(300*time.Second))
			}
		}
		reg.SetPlugin(pc.Name, fn)
	}
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
	red, err := redact.New(cfg.Redaction, detectors, missing)
	if err != nil {
		return nil, fmt.Errorf("redaction: %w", err)
	}
	userKey, err := loadOrCreateKey(filepath.Join(cfg.DataDir, "user.key"))
	if err != nil {
		return nil, err
	}
	rt := route.New(cfg, reg)
	rt.Prepare = func(text, target, dataClass string) (string, error) {
		// the classifier sees the last user message: redact it like any other outbound text
		mode := red.Mode(target, dataClass)
		req := oai.Object{"messages": []any{map[string]any{"role": "user", "content": text}}}
		if _, err := red.Apply(context.Background(), req, mode, userKey, "classifier"); err != nil {
			return "", err
		}
		return oai.Text(oai.Messages(req)[0]), nil
	}
	p := &pipeline.Pipeline{Cfg: cfg, Tokens: issuer, Keys: keyStore, Limiter: limiter, Meter: m, Router: rt, Reg: reg, Redactor: red, Audit: sink, UserKey: userKey, Log: log, Policy: resolver, Hooks: hooks}
	s.Pipeline = p

	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", p)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /healthz", s.healthz)
	s.HTTP = &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

// LocalToken returns the personal-mode token, issuing it on first start.
func (s *Server) LocalToken() (string, bool, error) {
	return s.Pipeline.Tokens.LocalToken(s.Pipeline.Cfg.DataDir)
}

// Shutdown stops serving, flushes metrics and kills plugins.
func (s *Server) Shutdown(ctx context.Context) {
	_ = s.HTTP.Shutdown(ctx)
	if s.otlp != nil {
		_ = s.otlp.Shutdown(ctx)
	}
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
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
			claims.Scope.Models = append(claims.Scope.Models, s.Pipeline.Router.Members(g)...)
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
	cfg := s.Pipeline.Cfg
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

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := "ok"
	if len(s.Missing) > 0 {
		status = "degraded"
	}
	out := map[string]any{"status": status, "mode": s.Pipeline.Cfg.Mode, "detectors_unavailable": s.Missing}
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
