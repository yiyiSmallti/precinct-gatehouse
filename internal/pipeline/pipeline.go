// Package pipeline runs one Chat Completions request through
//
//	auth → policy → route → redact → provider → restore → meter → audit
//
// In personal mode the policy step is built in: one user, scopes come from
// the gateway token, and data class comes from the X-Gatehouse-Data-Class
// header (default internal) capped by the token.
package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/audit"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/meter"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/route"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/token"
)

// Request headers understood by Gatehouse.
const (
	HeaderSource    = "X-Gatehouse-Source"     // caller label for routing rules (e.g. alcove)
	HeaderSession   = "X-Gatehouse-Session"    // session id: vault scope and turn key
	HeaderDataClass = "X-Gatehouse-Data-Class" // public | internal | confidential
)

// MaxBody bounds request bodies.
const MaxBody = 32 << 20

// Pipeline holds the shared components.
type Pipeline struct {
	Cfg      *config.Config
	Tokens   *token.Issuer
	Meter    *meter.Meter
	Router   *route.Router
	Reg      *provider.Registry
	Redactor *redact.Engine
	Audit    audit.Sink
	UserKey  []byte // personal mode: the single local user's placeholder key
	Log      *slog.Logger
}

type reqState struct {
	id      string
	start   time.Time
	claims  token.Claims
	rec     audit.Record
	w       http.ResponseWriter
	written bool
}

// ServeHTTP handles POST /v1/chat/completions.
func (p *Pipeline) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := &reqState{id: newID(), start: time.Now(), w: w}
	st.rec = audit.Record{Time: st.start, RequestID: st.id, Source: r.Header.Get(HeaderSource)}
	w.Header().Set("X-Gatehouse-Request-Id", st.id)
	err := p.handle(r.Context(), st, r)
	if err != nil {
		var oe *oai.Error
		if !errors.As(err, &oe) {
			oe = oai.Errorf(http.StatusInternalServerError, "internal_error", "%v", err)
		}
		st.rec.Status = oe.Status
		st.rec.Error = oe.Code + ": " + oe.Message
		if !st.written {
			oai.WriteError(w, oe)
		}
	}
	st.rec.LatencyMS = time.Since(st.start).Milliseconds()
	if err := p.Audit.Write(st.rec); err != nil {
		p.Log.Error("audit write failed", "err", err)
	}
	p.Log.Info("request", "id", st.id, "status", st.rec.Status, "model", st.rec.Model, "rule", st.rec.Rule, "ms", st.rec.LatencyMS)
}

func (p *Pipeline) handle(ctx context.Context, st *reqState, r *http.Request) error {
	// auth
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if tok == "" {
		return oai.Errorf(http.StatusUnauthorized, "missing_token", "missing gateway token (Authorization: Bearer <token>)")
	}
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
	st.rec.TokenID, st.rec.TokenName = claims.ID, claims.Name

	body, err := io.ReadAll(http.MaxBytesReader(st.w, r.Body, MaxBody))
	if err != nil {
		return oai.Errorf(http.StatusRequestEntityTooLarge, "body_too_large", "%v", err)
	}
	req, err := oai.Decode(body)
	if err != nil {
		return oai.Errorf(http.StatusBadRequest, "invalid_json", "%v", err)
	}
	// fields that carry plaintext outside the redaction scope never leave the gateway
	for _, f := range append([]string{"dsh_session_log"}, p.Cfg.Redaction.StripFields...) {
		delete(req, f)
	}
	if len(oai.Messages(req)) == 0 {
		return oai.Errorf(http.StatusBadRequest, "invalid_request", "messages is required")
	}
	requested := oai.Str(req, "model")
	st.rec.RequestedModel = requested

	// policy (personal mode): token scope, budget, data class
	isGroup := p.Router.IsGroup(requested)
	scopeName := requested
	if isGroup {
		scopeName = p.Router.ResolveGroupName(requested)
	}
	if !claims.Scope.AllowsModel(scopeName, isGroup) {
		return oai.Errorf(http.StatusForbidden, "model_not_allowed", "token %s may not use %q", claims.Name, requested)
	}
	if claims.Scope.MaxTokens > 0 {
		used, err := p.Meter.TokensUsed(claims.ID)
		if err != nil {
			return err
		}
		if used >= claims.Scope.MaxTokens {
			return oai.Errorf(http.StatusForbidden, "token_budget_exceeded", "token %s used %d of %d tokens", claims.Name, used, claims.Scope.MaxTokens)
		}
	}
	dataClass := strings.ToLower(strings.TrimSpace(r.Header.Get(HeaderDataClass)))
	if dataClass == "" {
		dataClass = "internal"
	}
	if token.ClassRank(dataClass) < 0 {
		return oai.Errorf(http.StatusBadRequest, "invalid_data_class", "unknown data class %q", dataClass)
	}
	maxClass := claims.Scope.DataClass
	if maxClass == "" {
		maxClass = "internal"
	}
	if token.ClassRank(dataClass) > token.ClassRank(maxClass) {
		return oai.Errorf(http.StatusForbidden, "data_class_not_allowed", "token %s may send at most %s data", claims.Name, maxClass)
	}
	st.rec.DataClass = dataClass

	// route
	// Without the header, fall back to the OpenAI `user` field. With neither,
	// routing still fixes decisions per turn (the turn key hashes the
	// conversation) and the vault is scoped to the token.
	session := r.Header.Get(HeaderSession)
	if session == "" {
		session = oai.Str(req, "user")
	}
	dec, err := p.Router.Decide(ctx, req, route.Meta{Source: st.rec.Source, Session: session, DataClass: dataClass, Now: time.Now()})
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
	cands := p.orderCandidates(dec.Candidates, dataClass)
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
	vaultKey := claims.ID + "\x00" + session

	var lastErr *provider.UpstreamError
	for _, ref := range cands {
		prov, model, _ := p.Reg.Lookup(ref)
		mode := p.Redactor.Mode(prov.Target, dataClass)
		out := oai.Clone(req).(map[string]any)
		out["model"] = model
		if e := effortParam(dec.Effort); e != "" {
			out["reasoning_effort"] = e
		}
		res, rerr := p.Redactor.Apply(ctx, out, mode, p.UserKey, vaultKey)
		st.rec.Redaction = res.Report
		st.rec.Model, st.rec.Target = ref, prov.Target
		if rerr != nil {
			var re *redact.Error
			if errors.As(rerr, &re) {
				return oai.Errorf(re.Status, re.Code, "%s", re.Msg)
			}
			return rerr
		}
		h.Set("X-Gatehouse-Model", ascii(ref))
		h.Set("X-Gatehouse-Redaction", redactionHeader(res.Report))

		upStream := clientStream && prov.Target == "local" && (mode == redact.ModeOff || mode == redact.ModeAudit)
		if upStream {
			out["stream_options"] = map[string]any{"include_usage": true}
		} else {
			delete(out, "stream")
			delete(out, "stream_options")
		}
		callStart := time.Now()
		resp, err := prov.Chat(ctx, oai.Encode(out))
		if err != nil {
			var ue *provider.UpstreamError
			if !errors.As(err, &ue) {
				ue = &provider.UpstreamError{Err: err}
			}
			st.rec.Attempts = append(st.rec.Attempts, audit.Attempt{Model: ref, Status: ue.Status, Error: ue.Error()})
			p.meterRecord(st, ref, prov.Target, oai.Usage{}, 0, statusOf(ue), time.Since(callStart))
			if ue.Retryable {
				p.Reg.MarkFailed(ref)
				lastErr = ue
				continue
			}
			// a client error from the provider is returned as is
			st.rec.Status = ue.Status
			st.w.Header().Set("Content-Type", "application/json")
			st.w.WriteHeader(ue.Status)
			st.w.Write(ue.Body)
			st.written = true
			return nil
		}
		p.Reg.MarkOK(ref)
		st.rec.Attempts = append(st.rec.Attempts, audit.Attempt{Model: ref, Status: resp.StatusCode})
		var usage oai.Usage
		if upStream {
			usage, err = p.relayStream(st, resp, clientUsage)
		} else {
			usage, err = p.relayBuffered(st, resp, vaultKey, clientStream, clientUsage)
		}
		resp.Body.Close()
		if err != nil {
			return err
		}
		st.rec.Status = http.StatusOK
		st.rec.PromptTokens, st.rec.CompletionTokens = usage.PromptTokens, usage.CompletionTokens
		cost := prov.Cost(model, usage.PromptTokens, usage.CompletionTokens)
		p.meterRecord(st, ref, prov.Target, usage, cost, http.StatusOK, time.Since(callStart))
		p.Router.Account(ref, usage.PromptTokens+usage.CompletionTokens)
		return nil
	}
	msg := "all candidate models failed"
	if lastErr != nil {
		msg += ": last error: " + lastErr.Error()
	}
	return oai.Errorf(http.StatusBadGateway, "upstream_unavailable", "%s (tried %s)", msg, strings.Join(cands, ", "))
}

// orderCandidates applies the data-class hard constraint and puts healthy
// models first (keeping configured order within each half).
func (p *Pipeline) orderCandidates(cands []string, dataClass string) []string {
	var healthy, cooling []string
	for _, ref := range cands {
		prov, _, ok := p.Reg.Lookup(ref)
		if !ok {
			continue
		}
		if dataClass == "confidential" && prov.Target != "local" {
			continue
		}
		if p.Reg.Healthy(ref) {
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
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, Cost: cost, Status: status, Latency: lat,
	})
	if err != nil {
		p.Log.Error("meter write failed", "err", err)
	}
}

func (p *Pipeline) meterClassifier(st *reqState, dec route.Decision) {
	prov, model, _ := p.Reg.Lookup(dec.ClassifierModel)
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
func (p *Pipeline) relayBuffered(st *reqState, resp *http.Response, vaultKey string, clientStream, clientUsage bool) (oai.Usage, error) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return oai.Usage{}, oai.Errorf(http.StatusBadGateway, "upstream_read", "%v", err)
	}
	o, err := oai.Decode(raw)
	if err != nil {
		return oai.Usage{}, oai.Errorf(http.StatusBadGateway, "upstream_bad_json", "provider returned invalid JSON")
	}
	if st.rec.Redaction != nil {
		p.Redactor.Restore(o, vaultKey, st.rec.Redaction)
	}
	usage, _ := oai.UsageOf(o)
	st.written = true
	if !clientStream {
		st.w.Header().Set("Content-Type", "application/json")
		st.w.WriteHeader(http.StatusOK)
		st.w.Write(oai.Encode(o))
		return usage, nil
	}
	st.w.Header().Set("Content-Type", "text/event-stream")
	st.w.Header().Set("Cache-Control", "no-cache")
	st.w.WriteHeader(http.StatusOK)
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

// relayStream passes an SSE stream through, reading usage from the final
// chunk. The usage-only chunk is dropped unless the client asked for it.
func (p *Pipeline) relayStream(st *reqState, resp *http.Response, clientUsage bool) (oai.Usage, error) {
	st.w.Header().Set("Content-Type", "text/event-stream")
	st.w.Header().Set("Cache-Control", "no-cache")
	st.w.WriteHeader(http.StatusOK)
	st.written = true
	fl, _ := st.w.(http.Flusher)
	var usage oai.Usage
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	var event bytes.Buffer
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			event.Write(line)
			if len(bytes.TrimSpace(line)) == 0 {
				p.flushEvent(st.w, event.Bytes(), clientUsage, &usage)
				event.Reset()
				if fl != nil {
					fl.Flush()
				}
			}
		}
		if err != nil {
			if event.Len() > 0 {
				p.flushEvent(st.w, event.Bytes(), clientUsage, &usage)
			}
			if fl != nil {
				fl.Flush()
			}
			if errors.Is(err, io.EOF) {
				return usage, nil
			}
			return usage, nil // client already has a partial stream; record what we have
		}
	}
}

func (p *Pipeline) flushEvent(w io.Writer, ev []byte, clientUsage bool, usage *oai.Usage) {
	for _, l := range bytes.Split(ev, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(l), []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 || data[0] != '{' {
			continue
		}
		o, err := oai.Decode(data)
		if err != nil {
			continue
		}
		if u, ok := oai.UsageOf(o); ok {
			*usage = u
			if ch, _ := o["choices"].([]any); len(ch) == 0 && !clientUsage {
				return
			}
		}
	}
	w.Write(ev)
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

func statusOf(e *provider.UpstreamError) int {
	if e.Status != 0 {
		return e.Status
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
