// Package api assembles the HTTP server: the OpenAI-compatible endpoints
// plus a health check.
package api

import (
	"context"
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
	"github.com/yiyiSmallti/precinct-gatehouse/internal/meter"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/pipeline"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/plugin"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/route"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/store"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/token"
)

// Server is a running gateway.
type Server struct {
	HTTP     *http.Server
	Pipeline *pipeline.Pipeline
	Missing  []string // required detectors that are unavailable
	closers  []func()
	otlp     *meter.OTLP
}

// New wires every component from a validated config.
func New(cfg *config.Config, version string, log *slog.Logger) (*Server, error) {
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{closers: []func(){func() { db.Close() }}}
	issuer, err := token.NewIssuer(cfg.DataDir, db)
	if err != nil {
		return nil, err
	}
	reg, err := provider.NewRegistry(cfg)
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
	sink, err := audit.NewFileSink(cfg.Audit.Dir)
	if err != nil {
		return nil, err
	}
	host := &plugin.Host{}
	s.closers = append(s.closers, host.Close)
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
	rt.Prepare = func(text, target string) (string, error) {
		// the classifier sees the last user message: redact it like any other outbound text
		mode := red.Mode(target, "internal")
		req := oai.Object{"messages": []any{map[string]any{"role": "user", "content": text}}}
		if _, err := red.Apply(context.Background(), req, mode, userKey, "classifier"); err != nil {
			return "", err
		}
		return oai.Text(oai.Messages(req)[0]), nil
	}
	p := &pipeline.Pipeline{Cfg: cfg, Tokens: issuer, Meter: m, Router: rt, Reg: reg, Redactor: red, Audit: sink, UserKey: userKey, Log: log}
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
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	claims, err := s.Pipeline.Tokens.Verify(tok)
	if err != nil {
		code := "invalid_token"
		if errors.Is(err, token.ErrExpired) {
			code = "token_expired"
		}
		oai.WriteError(w, oai.Errorf(http.StatusUnauthorized, code, "%v", err))
		return
	}
	cfg := s.Pipeline.Cfg
	var ids []string
	if cfg.DefaultGroup != "" && claims.Scope.AllowsModel(cfg.DefaultGroup, true) {
		ids = append(ids, "auto")
	}
	var groups []string
	for g := range cfg.RoutingGroups {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		if claims.Scope.AllowsModel(g, true) {
			ids = append(ids, g)
		}
	}
	for _, pc := range cfg.Providers {
		for _, m := range pc.Models {
			ref := pc.Name + "/" + m
			if claims.Scope.AllowsModel(ref, false) {
				ids = append(ids, ref)
			}
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
	w.Write(oai.Encode(map[string]any{"status": status, "mode": s.Pipeline.Cfg.Mode, "detectors_unavailable": s.Missing}))
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
