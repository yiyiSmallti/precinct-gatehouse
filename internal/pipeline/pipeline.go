// Package pipeline runs one Chat Completions request through
//
//	auth → policy → route → redact → provider → restore → meter → audit
//
// In personal mode the policy step is built in: one user, scopes come from
// the gateway token, and data class comes from the X-Gatehouse-Data-Class
// header (default internal) capped by the token.
//
// In org mode the AuthPolicy plugin resolves the bearer token into a tenant
// context (tenant, user, department, data class floor, allowed groups,
// budget). The plugin being unavailable refuses every request with 503.
package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/audit"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/keys"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/limit"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/live"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/meter"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/policy"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/route"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/tiered"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/token"
	sdk "github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

// Request headers understood by Gatehouse.
const (
	HeaderSource    = "X-Gatehouse-Source"     // caller label for routing rules (e.g. alcove)
	HeaderSession   = "X-Gatehouse-Session"    // session id: vault scope and turn key
	HeaderDataClass = "X-Gatehouse-Data-Class" // public | internal | confidential
)

// Pipeline holds the shared components.
type Pipeline struct {
	Live                  *live.Manager
	Tokens                *token.Issuer
	Keys                  *keys.Store
	Limiter               *limit.Limiter
	Meter                 *meter.Meter
	Audit                 audit.Sink
	UserKey               []byte // personal mode: the single local user's placeholder key; org mode: derivation key
	Log                   *slog.Logger
	Policy                *policy.Resolver // org mode only
	Hooks                 []Hook           // OnRequest/OnResponse plugins, in config order
	SSEKeepaliveInitDelay time.Duration    // delay before Gatehouse sends SSE keepalive during long prefill (0 = 20s, <0 disables)
	SSEKeepaliveInterval  time.Duration    // interval between SSE keepalives (0 = 15s, <0 disables)
	admitMu               sync.Mutex
	draining              bool
	inflight              atomic.Int64
}

// SetDraining toggles request admission for clean graceful update cutovers.
func (p *Pipeline) SetDraining(draining bool) {
	if p == nil {
		return
	}
	p.admitMu.Lock()
	p.draining = draining
	p.admitMu.Unlock()
}

// IsDraining reports whether new data-plane requests are currently rejected.
func (p *Pipeline) IsDraining() bool {
	if p == nil {
		return false
	}
	p.admitMu.Lock()
	d := p.draining
	p.admitMu.Unlock()
	return d
}

func (p *Pipeline) tryAdmit() bool {
	p.admitMu.Lock()
	if p.draining {
		p.admitMu.Unlock()
		return false
	}
	p.inflight.Add(1)
	p.admitMu.Unlock()
	return true
}

// Inflight returns the number of active requests currently inside the pipeline.
func (p *Pipeline) Inflight() int64 {
	if p == nil {
		return 0
	}
	p.admitMu.Lock()
	n := p.inflight.Load()
	p.admitMu.Unlock()
	return n
}

func (p *Pipeline) keepaliveInitDelay() time.Duration {
	if p.SSEKeepaliveInitDelay < 0 {
		return 0
	}
	if p.SSEKeepaliveInitDelay == 0 {
		return 20 * time.Second
	}
	return p.SSEKeepaliveInitDelay
}

func (p *Pipeline) keepaliveInterval() time.Duration {
	if p.SSEKeepaliveInterval < 0 {
		return 0
	}
	if p.SSEKeepaliveInterval == 0 {
		return 15 * time.Second
	}
	return p.SSEKeepaliveInterval
}

func (p *Pipeline) keepaliveIntervalFor(prov *provider.Provider) time.Duration {
	if prov != nil && prov.SSEKeepaliveSec < 0 {
		return 0
	}
	if p.SSEKeepaliveInterval < 0 {
		return 0
	}
	if prov != nil && prov.SSEKeepaliveSec > 0 && p.SSEKeepaliveInterval == 0 {
		return time.Duration(prov.SSEKeepaliveSec) * time.Second
	}
	return p.keepaliveInterval()
}

type principal struct {
	ID     string
	Name   string
	Scope  token.Scope
	Limits keys.Limits
	IsKey  bool
}

type reqState struct {
	id            string
	start         time.Time
	snap          *live.Snapshot
	protocol      string
	decode        func([]byte) (oai.Object, error)
	principal     principal
	claims        token.Claims
	org           *policy.Context // org mode
	rec           audit.Record
	w             http.ResponseWriter
	written       bool
	keepaliveOnly bool
	bodyDone      time.Time
	bodyRead      time.Duration
	requestBytes  int64
	ttft          time.Duration
	release       func(status int, tokens int64)
	admitted      bool
	errorCode     string
	clientStream  bool
	cachedTokens  int
	routeBackend  string
	routeReason   string
}

// ServeHTTP handles POST /v1/chat/completions.
func (p *Pipeline) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.Serve(w, r, "chat", oai.Decode)
}

// Serve runs one request through the gateway pipeline using the supplied
// protocol label and request decoder.
func (p *Pipeline) Serve(w http.ResponseWriter, r *http.Request, protocol string, decode func([]byte) (oai.Object, error)) {
	if !p.tryAdmit() {
		w.Header().Set("Retry-After", "5")
		oai.WriteError(w, oai.Errorf(http.StatusServiceUnavailable, "service_restarting", "gateway is draining for update restart; please retry shortly"))
		return
	}
	defer p.inflight.Add(-1)
	if decode == nil {
		decode = oai.Decode
	}
	st := &reqState{id: newID(), start: time.Now(), snap: p.Live.Current(), protocol: protocol, decode: decode, w: w}
	st.rec = audit.Record{Time: st.start, RequestID: st.id, Source: r.Header.Get(HeaderSource)}
	w.Header().Set("X-Gatehouse-Request-Id", st.id)
	err := p.handle(r.Context(), st, r)
	if err != nil {
		var oe *oai.Error
		if !errors.As(err, &oe) {
			if isClientCanceled(r.Context()) {
				oe = oai.Errorf(StatusClientClosedRequest, CodeClientDisconnected, "%v", r.Context().Err())
			} else {
				oe = oai.Errorf(http.StatusInternalServerError, "internal_error", "%v", err)
			}
		}
		st.rec.Status = oe.Status
		st.errorCode = oe.Code
		st.rec.Error = oe.Code + ": " + oe.Message
		if !st.written {
			oai.WriteError(w, oe)
		} else if st.keepaliveOnly {
			if oe.Status != StatusClientClosedRequest {
				errBody := oai.Encode(map[string]any{
					"error": map[string]any{
						"message": oe.Message,
						"type":    "upstream_error",
						"code":    oe.Code,
					},
				})
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", errBody)
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
			st.keepaliveOnly = false
		}
	}
	if st.release != nil {
		st.release(st.rec.Status, int64(st.rec.PromptTokens+st.rec.CompletionTokens))
	}
	st.rec.LatencyMS = time.Since(st.serviceOrigin()).Milliseconds()
	st.rec.BodyReadMS = st.bodyRead.Milliseconds()
	st.rec.RequestBytes = st.requestBytes
	if p.Meter != nil {
		var redMode string
		var redCount int
		if st.rec.Redaction != nil {
			redMode = st.rec.Redaction.Mode
			redCount = st.rec.Redaction.Masked + st.rec.Redaction.Warned
			if st.rec.Redaction.Blocked {
				redCount++
			}
		}
		if err := p.Meter.LogRequest(meter.RequestLogEntry{
			RequestID:        st.id,
			Time:             st.start,
			KeyID:            st.rec.TokenID,
			KeyName:          st.rec.TokenName,
			Protocol:         st.protocol,
			ClientIP:         clientIP(r),
			RequestedModel:   st.rec.RequestedModel,
			Model:            st.rec.Model,
			Status:           st.rec.Status,
			ErrorCode:        st.errorCode,
			Stream:           st.clientStream,
			PromptTokens:     st.rec.PromptTokens,
			CompletionTokens: st.rec.CompletionTokens,
			CachedTokens:     st.cachedTokens,
			LatencyMS:        st.rec.LatencyMS,
			TTFTMS:           st.ttft.Milliseconds(),
			BodyReadMS:       st.rec.BodyReadMS,
			RequestBytes:     st.rec.RequestBytes,
			Admitted:         st.admitted,
			RedactionMode:    redMode,
			RedactionCount:   redCount,
			RouteBackend:     st.routeBackend,
			RouteReason:      st.routeReason,
		}); err != nil {
			p.Log.Error("request log write failed", "err", err)
		}
	}
	if err := p.Audit.Write(st.rec); err != nil {
		p.Log.Error("audit write failed", "err", err)
	}
	p.Log.Info("request", "id", st.id, "status", st.rec.Status, "model", st.rec.Model, "rule", st.rec.Rule, "ms", st.rec.LatencyMS, "ttft_ms", st.ttft.Milliseconds(), "body_read_ms", st.rec.BodyReadMS, "req_bytes", st.rec.RequestBytes)
}

// Bearer extracts the gateway credential from Authorization: Bearer or x-api-key.
func Bearer(r *http.Request) string {
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
		if tok := strings.TrimSpace(auth[7:]); tok != "" {
			return tok
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func (p *Pipeline) handle(ctx context.Context, st *reqState, r *http.Request) error {
	// auth
	tok := Bearer(r)
	if tok == "" {
		return oai.Errorf(http.StatusUnauthorized, "missing_token", "missing gateway token (Authorization: Bearer <token>)")
	}
	if p.Policy != nil {
		if strings.HasPrefix(tok, "sk-") {
			return oai.Errorf(http.StatusUnauthorized, "invalid_token", "api keys are not supported in org mode")
		}
		return p.handleOrg(ctx, st, r, tok)
	}
	if strings.HasPrefix(tok, "sk-") {
		if p.Keys == nil {
			return oai.Errorf(http.StatusUnauthorized, "invalid_api_key", "invalid api key")
		}
		k, err := p.Keys.Lookup(tok)
		if err != nil {
			if k.ID != "" {
				st.rec.TokenID, st.rec.TokenName = k.ID, k.Name
			}
			code := "invalid_api_key"
			if errors.Is(err, keys.ErrDisabled) {
				code = "key_disabled"
			} else if errors.Is(err, keys.ErrExpired) {
				code = "key_expired"
			}
			return oai.Errorf(http.StatusUnauthorized, code, "%v", err)
		}
		st.principal = principal{
			ID:     k.ID,
			Name:   k.Name,
			Scope:  token.Scope{Models: k.Models, Groups: k.Groups, DataClass: "internal"},
			Limits: k.Limits,
			IsKey:  true,
		}
		st.claims = token.Claims{ID: k.ID, Name: k.Name, Scope: st.principal.Scope}
		st.rec.TokenID, st.rec.TokenName = k.ID, k.Name
		p.Keys.Touch(k.ID)
	} else {
		claims, err := p.Tokens.Verify(tok)
		if err != nil {
			st.rec.TokenID = claims.ID
			code := "invalid_token"
			if errors.Is(err, token.ErrExpired) {
				code = "token_expired"
			} else if errors.Is(err, token.ErrRevoked) {
				code = "token_revoked"
			}
			return oai.Errorf(http.StatusUnauthorized, code, "%v", err)
		}
		st.claims = claims
		st.principal = principal{ID: claims.ID, Name: claims.Name, Scope: claims.Scope}
		st.rec.TokenID, st.rec.TokenName = claims.ID, claims.Name
	}

	req, err := p.readRequest(st, r)
	if err != nil {
		return err
	}
	requested := oai.Str(req, "model")

	// policy (personal mode): token scope, budget, data class
	isGroup := st.snap.Router.IsGroup(requested)
	scopeName := requested
	if isGroup {
		scopeName = st.snap.Router.ResolveGroupName(requested)
	}
	if !st.principal.Scope.AllowsModel(scopeName, isGroup) {
		return oai.Errorf(http.StatusForbidden, "model_not_allowed", "token %s may not use %q", st.principal.Name, requested)
	}
	if !st.principal.IsKey && st.principal.Scope.MaxTokens > 0 {
		// Intentionally unlocked so concurrent requests on the same token are not
		// serialized; parallel in-flight requests may exceed MaxTokens slightly.
		used, err := p.Meter.TokensUsed(st.principal.ID)
		if err != nil {
			return err
		}
		if used >= st.principal.Scope.MaxTokens {
			return oai.Errorf(http.StatusForbidden, "token_budget_exceeded", "token %s used %d of %d tokens", st.principal.Name, used, st.principal.Scope.MaxTokens)
		}
	}
	maxClass := st.principal.Scope.DataClass
	if maxClass == "" {
		maxClass = "internal"
	}
	dataClass, err := requestClass(r, "internal")
	if err != nil {
		return err
	}
	if token.ClassRank(dataClass) > token.ClassRank(maxClass) {
		return oai.Errorf(http.StatusForbidden, "data_class_not_allowed", "token %s may send at most %s data", st.principal.Name, maxClass)
	}
	st.rec.DataClass = dataClass
	if st.principal.IsKey && p.Limiter != nil {
		rel, lerr := p.Limiter.Acquire(st.principal.ID, st.principal.Limits, time.Now())
		if lerr != nil {
			if lerr.RetryAfter > 0 {
				st.w.Header().Set("Retry-After", strconv.Itoa(lerr.RetryAfter))
			}
			return lerr.Err
		}
		st.release = func(status int, tokens int64) {
			rel(status > 0 && status < 500, tokens)
		}
	}
	st.admitted = true
	return p.serve(ctx, st, r, req, dataClass, p.UserKey, nil)
}

// handleOrg authorises the request through the AuthPolicy plugin.
func (p *Pipeline) handleOrg(ctx context.Context, st *reqState, r *http.Request, tok string) error {
	req, err := p.readRequest(st, r)
	if err != nil {
		return err
	}
	requested := oai.Str(req, "model")
	group := ""
	if st.snap.Router.IsGroup(requested) {
		group = st.snap.Router.ResolveGroupName(requested)
	}
	pc, err := p.Policy.Resolve(ctx, tok, &sdk.RequestMeta{Model: requested, RouteGroup: group, ClientIp: clientIP(r), RequestId: st.id, Source: st.rec.Source})
	if err != nil {
		var pe *policy.Error
		if errors.As(err, &pe) {
			return oai.Errorf(pe.Status, pe.Code, "%s", pe.Msg)
		}
		return err
	}
	st.org = pc
	st.claims = token.Claims{ID: "org:" + pc.Tenant + "/" + pc.User, Name: pc.User}
	st.rec.TokenID, st.rec.TokenName = st.claims.ID, pc.User
	st.rec.Tenant, st.rec.User, st.rec.Department = pc.Tenant, pc.User, pc.Department
	st.rec.OnBehalfOf, st.rec.InstanceID, st.rec.WorkspaceID, st.rec.PolicyVersion = pc.OnBehalfOf, pc.InstanceID, pc.WorkspaceID, pc.PolicyVersion

	allowed := p.orgAllowed(st.snap, pc)
	if group != "" {
		if !contains(pc.AllowedGroups, group) {
			return oai.Errorf(http.StatusForbidden, "model_not_allowed", "policy does not allow group %q for %s", group, pc.User)
		}
	} else if requested != "" && !allowed[requested] {
		return oai.Errorf(http.StatusForbidden, "model_not_allowed", "policy does not allow model %q for %s", requested, pc.User)
	}
	if pc.TokensRemaining != nil && *pc.TokensRemaining <= 0 {
		return oai.Errorf(http.StatusForbidden, "token_budget_exceeded", "policy budget for %s is used up", pc.User)
	}
	if pc.DepartmentTokensRemaining != nil && *pc.DepartmentTokensRemaining <= 0 {
		return oai.Errorf(http.StatusForbidden, "department_budget_exceeded", "department budget of %s is used up", pc.Department)
	}
	// the policy's class is a floor: the client may raise it, never lower it
	dataClass, err := requestClass(r, pc.DataClass)
	if err != nil {
		return err
	}
	if token.ClassRank(dataClass) < token.ClassRank(pc.DataClass) {
		dataClass = pc.DataClass
	}
	st.rec.DataClass = dataClass
	st.admitted = true
	mac := hmac.New(sha256.New, p.UserKey)
	mac.Write([]byte(pc.Tenant + "\x00" + pc.User))
	return p.serve(ctx, st, r, req, dataClass, mac.Sum(nil), allowed)
}

// orgAllowed lists the provider/model refs a policy context may reach: the
// members of its allowed groups plus its direct model allowlist.
func (p *Pipeline) orgAllowed(snap *live.Snapshot, pc *policy.Context) map[string]bool {
	out := map[string]bool{}
	for _, g := range pc.AllowedGroups {
		for _, m := range snap.Router.Members(g) {
			out[m] = true
		}
	}
	for _, m := range pc.Models {
		out[m] = true
	}
	return out
}

const (
	// StatusClientClosedRequest is the non-standard HTTP status (499) recorded when
	// the client disconnects or cancels the request context before completion.
	StatusClientClosedRequest = 499
	CodeClientDisconnected    = "client_disconnected"
)

func isClientCanceled(ctx context.Context) bool {
	return ctx != nil && errors.Is(ctx.Err(), context.Canceled)
}

// BodyReadError classifies request-body read failures into accurate HTTP status
// and error codes so upload timeouts and client disconnects are not misreported
// as HTTP 413 payload-too-large errors.
func BodyReadError(ctx context.Context, err error) *oai.Error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return oai.Errorf(http.StatusRequestEntityTooLarge, "body_too_large", "%v", err)
	}
	if errors.Is(err, context.Canceled) || isClientCanceled(ctx) {
		return oai.Errorf(StatusClientClosedRequest, CodeClientDisconnected, "%v", err)
	}
	return oai.Errorf(http.StatusRequestTimeout, "request_body_read_error", "%v", err)
}

// serviceOrigin returns the timestamp after the full request body was read,
// so both latency_ms and ttft_ms exclude slow client uploads while mid-upload
// failures (where bodyDone remains zero) fall back to request start.
func (st *reqState) serviceOrigin() time.Time {
	if !st.bodyDone.IsZero() {
		return st.bodyDone
	}
	return st.start
}

func (p *Pipeline) readRequest(st *reqState, r *http.Request) (oai.Object, error) {
	rc := r.Body
	if maxBytes := st.snap.Cfg.MaxBodyBytes(); maxBytes > 0 {
		rc = http.MaxBytesReader(st.w, r.Body, maxBytes)
	}
	readStart := time.Now()
	body, err := io.ReadAll(rc)
	readEnd := time.Now()
	st.bodyRead = readEnd.Sub(readStart)
	st.requestBytes = int64(len(body))
	if err != nil {
		return nil, BodyReadError(r.Context(), err)
	}
	st.bodyDone = readEnd
	req, err := st.decode(body)
	if err != nil {
		var oe *oai.Error
		if errors.As(err, &oe) {
			return nil, oe
		}
		return nil, oai.Errorf(http.StatusBadRequest, "invalid_request", "%v", err)
	}
	// fields that carry plaintext outside the redaction scope never leave the gateway
	for _, f := range append([]string{"dsh_session_log"}, st.snap.Cfg.Redaction.StripFields...) {
		delete(req, f)
	}
	if len(oai.Messages(req)) == 0 {
		return nil, oai.Errorf(http.StatusBadRequest, "invalid_request", "messages is required")
	}
	if n := oai.GuardBinaryToolOutput(req); n > 0 {
		st.w.Header().Set("X-Gatehouse-Binary-Tool-Output", strconv.Itoa(n))
	}
	origModel := oai.Str(req, "model")
	st.rec.RequestedModel = origModel
	st.clientStream = oai.Bool(req, "stream")
	if origModel != "" {
		if resolved := st.snap.Cfg.ResolveAlias(origModel); resolved != origModel {
			req["model"] = resolved
		}
	}
	return req, nil
}

func requestClass(r *http.Request, def string) (string, error) {
	dataClass := strings.ToLower(strings.TrimSpace(r.Header.Get(HeaderDataClass)))
	if dataClass == "" {
		dataClass = def
	}
	if token.ClassRank(dataClass) < 0 {
		return "", oai.Errorf(http.StatusBadRequest, "invalid_data_class", "unknown data class %q", dataClass)
	}
	return dataClass, nil
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if rip := strings.TrimSpace(r.Header.Get("X-Real-IP")); rip != "" {
			return rip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
				return strings.TrimSpace(first)
			}
		}
	}
	return host
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// serve runs route → redact → provider → restore → meter for an authorised
// request. allowed, when not nil, limits the candidates (org mode).
func (p *Pipeline) serve(ctx context.Context, st *reqState, r *http.Request, req oai.Object, dataClass string, userKey []byte, allowed map[string]bool) error {
	requested := oai.Str(req, "model")
	if err := p.runRequestHooks(ctx, st, req, dataClass); err != nil {
		return err
	}

	// route
	// Without the header, fall back to the OpenAI `user` field. With neither,
	// routing still fixes decisions per turn (the turn key hashes the
	// conversation) and the vault is scoped to the token.
	session := r.Header.Get(HeaderSession)
	if session == "" {
		session = oai.Str(req, "user")
	}
	// one redaction scope for the request: the classifier's redaction and every
	// failover candidate's share it, so a detector that failed under
	// on_error: rules_only is waited for once and every report names it
	redactCtx := redact.WithRequestScope(ctx)
	dec, err := st.snap.Router.Decide(redactCtx, req, route.Meta{Source: st.rec.Source, Session: session, DataClass: dataClass, Now: time.Now()})
	if err != nil {
		var um route.ErrUnknownModel
		if errors.As(err, &um) {
			return oai.Errorf(http.StatusNotFound, "model_not_found", "%v", err)
		}
		return err
	}
	st.rec.Group, st.rec.Rule, st.rec.RuleReason, st.rec.Effort = dec.Group, dec.Rule, dec.Reason, dec.Effort
	if dec.ClassifierModel != "" {
		p.meterClassifier(st, dec)
	}
	cands := p.orderCandidates(st.snap, dec.Candidates, dataClass)
	if allowed != nil {
		var keep []string
		for _, c := range cands {
			if allowed[c] {
				keep = append(keep, c)
			}
		}
		if len(keep) == 0 && len(cands) > 0 {
			return oai.Errorf(http.StatusForbidden, "model_not_allowed", "routing for %q selected only models outside the policy's allowed groups", requested)
		}
		cands = keep
	}
	if len(cands) == 0 {
		return oai.Errorf(http.StatusForbidden, "no_allowed_target", "data class %s may only use local models and none is configured for %q; refusing rather than sending it outside", dataClass, requested)
	}
	h := st.w.Header()
	h.Set("X-Gatehouse-Group", ascii(dec.Group))
	h.Set("X-Gatehouse-Rule", ascii(dec.Rule))
	h.Set("X-Gatehouse-Effort", dec.Effort)

	clientStream := oai.Bool(req, "stream")
	clientUsage := false
	if so, ok := req["stream_options"].(map[string]any); ok {
		clientUsage = oai.Bool(so, "include_usage")
	}
	vaultKey := st.claims.ID + "\x00" + session

	var lastErr *provider.UpstreamError
	for _, ref := range cands {
		prov, model, _ := st.snap.Reg.Lookup(ref)
		mode := st.snap.Redactor.Mode(prov.Target, dataClass)
		out := oai.Clone(req).(map[string]any)
		out["model"] = model
		if e := effortParam(dec.Effort); e != "" {
			out["reasoning_effort"] = e
		}
		if prov.SystemPrompt != "" {
			injectSystemPrompt(out, prov.SystemPrompt)
		}
		for k, v := range prov.ExtraBody {
			out[k] = oai.Clone(v)
		}
		res, rerr := st.snap.Redactor.Apply(redactCtx, out, mode, userKey, vaultKey)
		st.rec.Redaction = res.Report
		st.rec.Model, st.rec.Target = ref, prov.Target
		if rerr != nil {
			var re *redact.Error
			if errors.As(rerr, &re) {
				return oai.Errorf(re.Status, re.Code, "%s", re.Msg)
			}
			return rerr
		}
		if !st.written {
			h.Set("X-Gatehouse-Model", ascii(ref))
			h.Set("X-Gatehouse-Redaction", redactionHeader(res.Report))
		}

		upStream := clientStream && prov.Target == "local" && (mode == redact.ModeOff || mode == redact.ModeAudit)
		if upStream {
			out["stream_options"] = map[string]any{"include_usage": true}
		} else {
			delete(out, "stream")
			delete(out, "stream_options")
		}
		callStart := time.Now()
		routeMeta := &tiered.RouteMeta{}
		reqCtx := provider.WithCompressHeaders(
			tiered.WithRouteMeta(
				tiered.WithCallerScope(tiered.WithDataClass(ctx, dataClass), st.claims.ID+"\x00"+dataClass),
				routeMeta,
			),
			r.Header.Get("X-No-Compress"),
			r.Header.Get("X-Context-Compress"),
		)
		resp, err := p.callProviderWithKeepalive(reqCtx, st, prov, oai.Encode(out), clientStream, upStream)
		if err != nil {
			var ue *provider.UpstreamError
			if !errors.As(err, &ue) {
				ue = &provider.UpstreamError{Err: err}
			}
			stCode := statusOf(ctx, ue)
			attemptStatus := ue.Status
			if attemptStatus == 0 && stCode == StatusClientClosedRequest {
				attemptStatus = StatusClientClosedRequest
			}
			st.rec.Attempts = append(st.rec.Attempts, audit.Attempt{Model: ref, Status: attemptStatus, Error: ue.Error()})
			p.meterRecord(st, ref, prov.Target, oai.Usage{}, 0, stCode, time.Since(callStart))
			if ue.Status == 0 && isClientCanceled(ctx) {
				return oai.Errorf(StatusClientClosedRequest, CodeClientDisconnected, "%s: %s", ref, ue.Error())
			}
			if ue.Retryable {
				st.snap.Reg.MarkFailed(ref)
				lastErr = ue
				continue
			}
			if ue.Status/100 == 3 {
				// redirects are never followed (they could leave the provider's trust boundary)
				return oai.Errorf(http.StatusBadGateway, "upstream_redirect", "%s: provider answered with a redirect (%d); redirects are not followed", ref, ue.Status)
			}
			if ue.Status == 0 {
				// no upstream response (e.g. dial failure or upstream timeout)
				return oai.Errorf(http.StatusBadGateway, "upstream_unavailable", "%s: %s", ref, ue.Error())
			}
			if st.written {
				return oai.Errorf(stCode, "upstream_error", "%s", ue.Error())
			}
			// a client error from the provider is returned as is
			copyRouterHeaders(st, ue.Header)
			st.rec.Status = ue.Status
			st.errorCode = "upstream_error"
			st.w.Header().Set("Content-Type", "application/json")
			st.w.WriteHeader(ue.Status)
			st.w.Write(ue.Body)
			st.written = true
			return nil
		}
		st.snap.Reg.MarkOK(ref)
		copyRouterHeaders(st, resp.Header)
		applyRouteMeta(st, routeMeta)
		st.rec.Attempts = append(st.rec.Attempts, audit.Attempt{Model: ref, Status: resp.StatusCode})
		var usage oai.Usage
		if upStream {
			usage, err = p.relayStream(reqCtx, st, resp, clientUsage, p.keepaliveIntervalFor(prov))
		} else {
			usage, err = p.relayBuffered(reqCtx, st, resp, vaultKey, clientStream, clientUsage)
		}
		resp.Body.Close()
		applyRouteMeta(st, routeMeta)
		if err != nil {
			status := http.StatusBadGateway
			var oe *oai.Error
			if errors.As(err, &oe) {
				status = oe.Status
			}
			st.rec.PromptTokens, st.rec.CompletionTokens = usage.PromptTokens, usage.CompletionTokens
			st.cachedTokens = usage.CachedTokens
			p.meterRecord(st, ref, prov.Target, usage, prov.Cost(model, usage.PromptTokens, usage.CompletionTokens), status, time.Since(callStart))
			return err
		}
		st.rec.Status = http.StatusOK
		st.rec.PromptTokens, st.rec.CompletionTokens = usage.PromptTokens, usage.CompletionTokens
		st.cachedTokens = usage.CachedTokens
		cost := prov.Cost(model, usage.PromptTokens, usage.CompletionTokens)
		p.meterRecord(st, ref, prov.Target, usage, cost, http.StatusOK, time.Since(callStart))
		st.snap.Router.Account(ref, usage.PromptTokens+usage.CompletionTokens)
		return nil
	}
	msg := "all candidate models failed"
	if lastErr != nil {
		msg += ": last error: " + lastErr.Error()
	}
	if dataClass == "confidential" {
		return oai.Errorf(http.StatusServiceUnavailable, "local_targets_unavailable", "%s (tried %s); confidential data is never sent to external models, so the request is refused", msg, strings.Join(cands, ", "))
	}
	return oai.Errorf(http.StatusBadGateway, "upstream_unavailable", "%s (tried %s)", msg, strings.Join(cands, ", "))
}

// orderCandidates applies the data-class hard constraint and puts healthy
// models first (keeping configured order within each half).
func (p *Pipeline) orderCandidates(snap *live.Snapshot, cands []string, dataClass string) []string {
	var healthy, cooling []string
	for _, ref := range cands {
		prov, _, ok := snap.Reg.Lookup(ref)
		if !ok {
			continue
		}
		if dataClass == "confidential" && prov.Target != "local" {
			continue
		}
		if snap.Reg.Healthy(ref) {
			healthy = append(healthy, ref)
		} else {
			cooling = append(cooling, ref)
		}
	}
	return append(healthy, cooling...)
}

func (p *Pipeline) meterRecord(st *reqState, ref, target string, u oai.Usage, cost float64, status int, lat time.Duration) {
	err := p.Meter.Add(meter.Record{
		RequestID: st.id, Time: time.Now(), TokenID: st.claims.ID, TokenName: st.claims.Name,
		RequestedModel: st.rec.RequestedModel, Model: ref, Target: target,
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, CachedTokens: u.CachedTokens, Cost: cost, Status: status, Latency: lat,
	})
	if err != nil {
		p.Log.Error("meter write failed", "err", err)
	}
}

func (p *Pipeline) meterClassifier(st *reqState, dec route.Decision) {
	prov, model, _ := st.snap.Reg.Lookup(dec.ClassifierModel)
	status := http.StatusOK
	if dec.ClassifierError != "" {
		status = http.StatusGatewayTimeout
	}
	cost := prov.Cost(model, dec.ClassifierUsage.PromptTokens, dec.ClassifierUsage.CompletionTokens)
	err := p.Meter.Add(meter.Record{
		RequestID: st.id, Time: time.Now(), TokenID: st.claims.ID, TokenName: st.claims.Name,
		RequestedModel: "classifier:" + dec.Group, Model: dec.ClassifierModel, Target: prov.Target,
		PromptTokens: dec.ClassifierUsage.PromptTokens, CompletionTokens: dec.ClassifierUsage.CompletionTokens, Cost: cost, Status: status,
	})
	if err != nil {
		p.Log.Error("meter write failed", "err", err)
	}
}

// relayBuffered reads a full JSON response, restores placeholders and writes
// it either as JSON or, when the client asked for a stream, as SSE.
func (p *Pipeline) relayBuffered(ctx context.Context, st *reqState, resp *http.Response, vaultKey string, clientStream, clientUsage bool) (oai.Usage, error) {
	if ctx != nil {
		stop := context.AfterFunc(ctx, func() { _ = resp.Body.Close() })
		defer stop()
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		if isClientCanceled(ctx) {
			return oai.Usage{}, oai.Errorf(StatusClientClosedRequest, CodeClientDisconnected, "%v", err)
		}
		return oai.Usage{}, oai.Errorf(http.StatusBadGateway, "upstream_read", "%v", err)
	}
	o, err := oai.Decode(raw)
	if err != nil {
		return oai.Usage{}, oai.Errorf(http.StatusBadGateway, "upstream_bad_json", "provider returned invalid JSON")
	}
	if st.rec.Redaction != nil {
		st.snap.Redactor.Restore(o, vaultKey, st.rec.Redaction)
	}
	usage, _ := oai.UsageOf(o)
	if len(p.Hooks) > 0 {
		body := oai.Encode(o)
		go p.runResponseHooks(p.hookCtx(st, st.rec.DataClass), body, http.StatusOK)
	}
	alreadyWritten := st.written
	st.written = true
	st.keepaliveOnly = false
	st.ttft = time.Since(st.serviceOrigin())
	if !clientStream {
		st.w.Header().Set("Content-Type", "application/json")
		st.w.WriteHeader(http.StatusOK)
		st.w.Write(oai.Encode(o))
		return usage, nil
	}
	if !alreadyWritten {
		st.w.Header().Set("Content-Type", "text/event-stream")
		st.w.Header().Set("Cache-Control", "no-cache")
		st.w.WriteHeader(http.StatusOK)
	}
	for _, ev := range toChunks(o, clientUsage) {
		fmt.Fprintf(st.w, "data: %s\n\n", ev)
	}
	io.WriteString(st.w, "data: [DONE]\n\n")
	if f, ok := st.w.(http.Flusher); ok {
		f.Flush()
	}
	return usage, nil
}

// toChunks converts a chat.completion into chat.completion.chunk events.
func toChunks(o oai.Object, withUsage bool) [][]byte {
	base := func() map[string]any {
		return map[string]any{"id": o["id"], "object": "chat.completion.chunk", "created": o["created"], "model": o["model"]}
	}
	var out [][]byte
	choices, _ := o["choices"].([]any)
	for _, c := range choices {
		co, _ := c.(map[string]any)
		msg, _ := co["message"].(map[string]any)
		delta := map[string]any{"role": "assistant"}
		for k, v := range msg {
			if k == "tool_calls" {
				tcs, _ := v.([]any)
				var withIdx []any
				for i, tc := range tcs {
					m, _ := oai.Clone(tc).(map[string]any)
					if m != nil {
						m["index"] = i
						withIdx = append(withIdx, m)
					}
				}
				delta["tool_calls"] = withIdx
				continue
			}
			if k != "role" {
				delta[k] = v
			}
		}
		ch := base()
		ch["choices"] = []any{map[string]any{"index": co["index"], "delta": delta, "finish_reason": co["finish_reason"]}}
		out = append(out, oai.Encode(ch))
	}
	if withUsage && o["usage"] != nil {
		ch := base()
		ch["choices"] = []any{}
		ch["usage"] = o["usage"]
		out = append(out, oai.Encode(ch))
	}
	return out
}

type sseEventItem struct {
	ev  []byte
	err error
}

type clientByteCounter interface {
	ClientBytesWritten() int64
}

// relayStream passes an SSE stream through, reading usage from the final
// chunk and emitting idle `: keepalive` comments whenever client-visible output
// stalls (e.g., during upstream tool buffering or translator custom-tool
// buffering). The usage-only chunk is dropped unless the client asked for it.
func (p *Pipeline) relayStream(ctx context.Context, st *reqState, resp *http.Response, clientUsage bool, keepaliveEvery time.Duration) (oai.Usage, error) {
	if !st.written {
		st.w.Header().Set("Content-Type", "text/event-stream")
		st.w.Header().Set("Cache-Control", "no-cache")
		st.w.WriteHeader(http.StatusOK)
		st.written = true
	}
	st.keepaliveOnly = false
	fl, _ := st.w.(http.Flusher)
	counter, hasCounter := st.w.(clientByteCounter)

	evCh := make(chan sseEventItem, 1)
	stopRead := make(chan struct{})
	var stopOnce sync.Once
	stopReader := func() {
		stopOnce.Do(func() { close(stopRead) })
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(evCh)
		br := bufio.NewReaderSize(resp.Body, 64<<10)
		var event bytes.Buffer
		send := func(ev []byte, err error) bool {
			var cp []byte
			if len(ev) > 0 {
				cp = append([]byte(nil), ev...)
			}
			select {
			case evCh <- sseEventItem{ev: cp, err: err}:
				return true
			case <-stopRead:
				return false
			}
		}
		for {
			line, rerr := br.ReadBytes('\n')
			if len(line) > 0 {
				event.Write(line)
				if len(bytes.TrimSpace(line)) == 0 {
					if !send(event.Bytes(), nil) {
						return
					}
					event.Reset()
				}
			}
			if rerr != nil {
				_ = send(event.Bytes(), rerr)
				return
			}
		}
	}()
	defer func() {
		stopReader()
		wg.Wait()
	}()

	var idleTimer *time.Timer
	var idleC <-chan time.Time
	if keepaliveEvery > 0 {
		idleTimer = time.NewTimer(keepaliveEvery)
		idleC = idleTimer.C
		defer idleTimer.Stop()
	}
	resetIdle := func() {
		if idleTimer == nil {
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(keepaliveEvery)
	}

	var usage oai.Usage
	done := false // saw [DONE] or a finish_reason
	sawStreamErr := false

	for {
		select {
		case item, ok := <-evCh:
			if !ok {
				if sawStreamErr {
					return usage, oai.Errorf(http.StatusBadGateway, "upstream_error", "upstream stream error")
				}
				if done {
					return usage, nil
				}
				if isClientCanceled(ctx) {
					return usage, oai.Errorf(StatusClientClosedRequest, CodeClientDisconnected, "%v", ctx.Err())
				}
				return usage, oai.Errorf(http.StatusBadGateway, "upstream_stream_interrupted", "stream ended before completion")
			}
			if len(item.ev) > 0 {
				var before int64
				if hasCounter {
					before = counter.ClientBytesWritten()
				}
				hasContent, wrote := p.flushEvent(st.w, item.ev, clientUsage, &usage, &done, &sawStreamErr)
				if hasContent && st.ttft == 0 {
					st.ttft = time.Since(st.serviceOrigin())
				}
				if fl != nil {
					fl.Flush()
				}
				clientProgressed := wrote
				if hasCounter {
					clientProgressed = counter.ClientBytesWritten() > before
				}
				if clientProgressed {
					resetIdle()
				}
			}
			if item.err != nil {
				if errors.Is(item.err, io.EOF) {
					if sawStreamErr {
						return usage, oai.Errorf(http.StatusBadGateway, "upstream_error", "upstream stream error")
					}
					if done {
						return usage, nil
					}
					if isClientCanceled(ctx) {
						return usage, oai.Errorf(StatusClientClosedRequest, CodeClientDisconnected, "%v", ctx.Err())
					}
					return usage, oai.Errorf(http.StatusBadGateway, "upstream_stream_interrupted", "stream ended before completion")
				}
				if isClientCanceled(ctx) {
					return usage, oai.Errorf(StatusClientClosedRequest, CodeClientDisconnected, "%v", item.err)
				}
				// the client already has a partial stream; report the failure so it is metered and audited
				return usage, oai.Errorf(http.StatusBadGateway, "upstream_stream_interrupted", "%v", item.err)
			}
		case <-idleC:
			_, _ = io.WriteString(st.w, ": keepalive\n\n")
			if fl != nil {
				fl.Flush()
			}
			idleTimer.Reset(keepaliveEvery)
		case <-ctx.Done():
			stopReader()
			_ = resp.Body.Close()
			if isClientCanceled(ctx) {
				return usage, oai.Errorf(StatusClientClosedRequest, CodeClientDisconnected, "%v", ctx.Err())
			}
			return usage, oai.Errorf(http.StatusBadGateway, "upstream_stream_interrupted", "%v", ctx.Err())
		}
	}
}

func (p *Pipeline) flushEvent(w io.Writer, ev []byte, clientUsage bool, usage *oai.Usage, done *bool, sawStreamErr *bool) (bool, bool) {
	hasContent := false
	for _, l := range bytes.Split(ev, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(l), []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if string(data) == "[DONE]" {
			*done = true
			continue
		}
		if len(data) == 0 || data[0] != '{' {
			continue
		}
		o, err := oai.Decode(data)
		if err != nil {
			continue
		}
		if errVal, hasErr := o["error"]; hasErr && errVal != nil {
			*sawStreamErr = true
			*done = true
			continue
		}
		if ch, _ := o["choices"].([]any); len(ch) > 0 {
			for _, c := range ch {
				if cm, _ := c.(map[string]any); cm != nil && cm["finish_reason"] != nil {
					*done = true
				}
			}
		}
		if u, ok := oai.UsageOf(o); ok {
			*usage = u
			if ch, _ := o["choices"].([]any); len(ch) == 0 && !clientUsage {
				return false, false
			}
		}
		hasContent = true
	}
	_, _ = w.Write(ev)
	return hasContent, true
}

func injectSystemPrompt(req oai.Object, prompt string) {
	raw, _ := req["messages"].([]any)
	if len(raw) > 0 {
		if first, ok := raw[0].(map[string]any); ok && oai.Str(first, "role") == "system" {
			switch c := first["content"].(type) {
			case string:
				if c == "" {
					first["content"] = prompt
				} else {
					first["content"] = prompt + "\n\n" + c
				}
				return
			case []any:
				part := map[string]any{"type": "text", "text": prompt}
				first["content"] = append([]any{part}, c...)
				return
			default:
				first["content"] = prompt
				return
			}
		}
	}
	sys := map[string]any{"role": "system", "content": prompt}
	req["messages"] = append([]any{sys}, raw...)
}

func effortParam(e string) string {
	switch e {
	case "low", "medium", "high":
		return e
	case "xhigh", "max":
		return "high"
	}
	return ""
}

func statusOf(ctx context.Context, e *provider.UpstreamError) int {
	if e.Status != 0 {
		return e.Status
	}
	if isClientCanceled(ctx) {
		return StatusClientClosedRequest
	}
	return http.StatusBadGateway
}

func redactionHeader(r *audit.Redaction) string {
	if r == nil {
		return ""
	}
	keys := make([]string, 0, len(r.Counts))
	for k := range r.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{r.Mode}
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Itoa(r.Counts[k]))
	}
	return strings.Join(parts, "; ")
}

// ascii keeps header values ASCII (rule names may be Chinese).
func ascii(s string) string {
	q := strconv.QuoteToASCII(s)
	return q[1 : len(q)-1]
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

func copyRouterHeaders(st *reqState, hdr http.Header) {
	if len(hdr) == 0 {
		return
	}
	if !st.written {
		for k, vs := range hdr {
			if len(vs) == 0 {
				continue
			}
			lk := strings.ToLower(k)
			if strings.HasPrefix(lk, "x-router-") || strings.HasPrefix(lk, "x-context-") {
				st.w.Header().Set(k, vs[0])
			}
		}
	}
	if b := hdr.Get("X-Router-Backend"); b != "" {
		st.routeBackend = b
	}
	if rs := hdr.Get("X-Router-Reason"); rs != "" {
		st.routeReason = rs
	}
}

func applyRouteMeta(st *reqState, m *tiered.RouteMeta) {
	if m == nil {
		return
	}
	if b, r := m.Get(); b != "" {
		st.routeBackend = b
		st.routeReason = r
	}
}

type chatCallRes struct {
	resp *http.Response
	err  error
}

func (p *Pipeline) callProviderWithKeepalive(ctx context.Context, st *reqState, prov *provider.Provider, body []byte, clientStream, upStream bool) (*http.Response, error) {
	initDelay := p.keepaliveInitDelay()
	keepaliveEvery := p.keepaliveIntervalFor(prov)
	if !clientStream || initDelay <= 0 || keepaliveEvery <= 0 {
		return prov.Chat(ctx, body)
	}
	// Let active tiered routers emit keepalives first at initDelay (with X-Router-* headers);
	// Pipeline still acts as a safety net for non-tiered upstreams or buffered streaming masks.
	firstWait := initDelay
	if st.written {
		firstWait = keepaliveEvery
	} else if upStream {
		firstWait += initDelay / 10
	}
	var callMu sync.Mutex
	canceled := false
	ch := make(chan chatCallRes, 1)
	go func() {
		r, e := prov.Chat(ctx, body)
		callMu.Lock()
		if canceled {
			callMu.Unlock()
			if r != nil && r.Body != nil {
				_ = r.Body.Close()
			}
			return
		}
		ch <- chatCallRes{resp: r, err: e}
		callMu.Unlock()
	}()

	timer := time.NewTimer(firstWait)
	defer timer.Stop()
	var tk *time.Ticker
	var tickC <-chan time.Time
	fl, _ := st.w.(http.Flusher)

	for {
		select {
		case res := <-ch:
			if tk != nil {
				tk.Stop()
			}
			return res.resp, res.err
		case <-ctx.Done():
			if tk != nil {
				tk.Stop()
			}
			callMu.Lock()
			canceled = true
			var late *http.Response
			select {
			case res := <-ch:
				late = res.resp
			default:
			}
			callMu.Unlock()
			if late != nil && late.Body != nil {
				_ = late.Body.Close()
			}
			return nil, &provider.UpstreamError{Err: ctx.Err()}
		case <-timer.C:
			if !st.written {
				st.w.Header().Set("Content-Type", "text/event-stream")
				st.w.Header().Set("Cache-Control", "no-cache")
				st.w.WriteHeader(http.StatusOK)
				st.written = true
				st.keepaliveOnly = true
			}
			_, _ = io.WriteString(st.w, ": keepalive\n\n")
			if fl != nil {
				fl.Flush()
			}
			tk = time.NewTicker(keepaliveEvery)
			tickC = tk.C
		case <-tickC:
			_, _ = io.WriteString(st.w, ": keepalive\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}
}
