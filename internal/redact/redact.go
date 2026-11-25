// Package redact is the redaction stage. It runs after routing (so it knows
// where the request is going) and before the provider call:
//
//	detect → decide → mask → (send) → restore → audit
//
// Placeholders are {{KIND_xxxxxxxx}} where the hash is an HMAC of the
// normalised value under a per-user key, so the same value always gets the
// same placeholder for the same user, across turns. The mapping lives in the
// in-memory Vault and, with redaction.vault_persist, encrypted in gatehouse.db.
package redact

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/audit"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact/rules"
)

// Modes.
const (
	ModeOff   = "off"
	ModeAudit = "audit"
	ModeMask  = "mask"
	ModeWarn  = "warn"
	ModeBlock = "block"
)

// Term is the dictionary category.
const Term = "TERM"

// Finding is one detected value.
type Finding struct {
	Kind   string
	Start  int // byte offsets
	End    int
	Source string // rules | dictionary | <plugin name>
	// Confidence is the plugin's score; 0 means unscored (rules, dictionary,
	// or a plugin that reports none), which min_confidence never drops.
	Confidence float32
}

// ExternalDetector is a detector plugin. Offsets are byte offsets after
// conversion. The engine bounds each call with the source's timeout.
type ExternalDetector interface {
	Name() string
	Detect(ctx context.Context, text string) ([]Finding, error)
}

// DefaultDetectTimeout bounds one Detect call unless redaction.sources.<name>.timeout is set.
const DefaultDetectTimeout = 5 * time.Second

// OnErrorRulesOnly is the redaction.sources.<name>.on_error value that lets a
// request go on without that detector.
const OnErrorRulesOnly = "rules_only"

// Engine holds the configured detectors.
type Engine struct {
	cfg      config.Redaction
	rules    *rules.Detector
	terms    []string
	never    map[string]bool
	external []ExternalDetector
	missing  []string // required detectors that are declared but unavailable
	vaultTTL time.Duration
	Vault    *Vault
	// Cache holds detector plugin results; callers that rebuild the engine on
	// reload pass the previous one on, like Vault.
	Cache *DetectCache
}

// New builds the engine. `external` are the running detector plugins and
// `missing` names required detectors that could not be started.
func New(cfg config.Redaction, external []ExternalDetector, missing []string) (*Engine, error) {
	vaultTTL := cfg.VaultTTL.D(24 * time.Hour)
	e := &Engine{cfg: cfg, rules: rules.New(cfg.InternalDomains), never: map[string]bool{}, external: external, missing: missing,
		vaultTTL: vaultTTL, Vault: NewVault(vaultTTL), Cache: NewDetectCache(DefaultDetectCacheEntries)}
	var terms []string
	for _, t := range cfg.Terms {
		if t = strings.TrimSpace(t); t != "" {
			terms = append(terms, t)
		}
	}
	if cfg.Dictionary != "" {
		f, err := os.Open(cfg.Dictionary)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			t := strings.TrimSpace(sc.Text())
			if t != "" && !strings.HasPrefix(t, "#") {
				terms = append(terms, t)
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	// longest first so "张三丰" wins over "张三"
	sort.SliceStable(terms, func(i, j int) bool { return len(terms[i]) > len(terms[j]) })
	e.terms = terms
	for _, v := range cfg.NeverMask {
		e.never[strings.ToLower(strings.TrimSpace(v))] = true
	}
	return e, nil
}

// VaultTTL returns the vault expiration duration configured for this engine snapshot.
func (e *Engine) VaultTTL() time.Duration {
	return e.vaultTTL
}

// Missing returns the names of required detectors unavailable in this engine snapshot.
func (e *Engine) Missing() []string {
	if len(e.missing) == 0 {
		return nil
	}
	return append([]string(nil), e.missing...)
}

// Mode returns the redaction mode for a target and data class. A
// confidential request bound for an external target is always blocked: the
// second gate behind data-class routing.
func (e *Engine) Mode(target, dataClass string) string {
	if dataClass == "confidential" && target == "external" {
		return ModeBlock
	}
	m := e.cfg.ModeByTarget[target]
	if m == "" {
		return ModeMask
	}
	return m
}

// action returns what to do with a finding of `kind` under `mode`.
func (e *Engine) action(kind, mode string) string {
	switch mode {
	case ModeAudit:
		return "audit"
	case ModeWarn:
		return "warn"
	case ModeBlock:
		return "block"
	}
	a := e.cfg.Categories[strings.ToLower(kind)]
	if a == "" {
		a = "mask"
	}
	return a
}

// Built-in detector sources; redaction.sources never applies to them.
const (
	SourceRules      = "rules"
	SourceDictionary = "dictionary"
)

// source returns the redaction.sources settings of f's detector plugin.
// Built-in findings get none, even if a plugin were named like them.
func (e *Engine) source(f Finding) config.RedactionSource {
	if f.Source == SourceRules || f.Source == SourceDictionary {
		return config.RedactionSource{}
	}
	return e.cfg.Sources[f.Source]
}

// findingAction is action for f, capped by its source's max_action: a
// model-based detector's findings can be kept from blocking a request.
func (e *Engine) findingAction(f Finding, mode string) string {
	a := e.action(f.Kind, mode)
	if c := e.source(f).MaxAction; c != "" && a != "keep" && actionRank[a] > actionRank[c] {
		a = c
	}
	return a
}

// confident reports whether f meets its source's min_confidence. Unscored
// findings always do.
func (e *Engine) confident(f Finding) bool {
	mc := e.source(f).MinConfidence
	if len(mc) == 0 || f.Confidence == 0 {
		return true
	}
	min, ok := mc[strings.ToLower(f.Kind)]
	if !ok {
		if min, ok = mc["default"]; !ok {
			return true
		}
	}
	// compare in float32, the plugin's precision: a 0.9 score must meet a 0.9 threshold
	return f.Confidence >= float32(min)
}

// actionRank orders actions for overlapping findings.
var actionRank = map[string]int{"audit": 1, "warn": 2, "mask": 3, "block": 4}

// Error is a redaction-stage failure with an HTTP status.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// inlineImageMin is the smallest base64 payload of a `data:image/...;base64,`
// URI that DetectText skips (line-wrapped payloads included). Such payloads hold no readable PII, and running
// every detector over a multi-megabyte image stalled requests for minutes.
const inlineImageMin = 1024

// detectRun is the detector state of one client request: a source that
// failed under on_error: rules_only is skipped for the rest of the request,
// so a request with many texts, or redacted again for each failover
// candidate, waits for its timeout once. It is used sequentially.
type detectRun struct {
	degraded map[string]string // source -> alert
	order    []string
}

func (r *detectRun) degrade(name string, err error) {
	if _, ok := r.degraded[name]; ok {
		return
	}
	r.degraded[name] = fmt.Sprintf("detector %s: %v; continued without it (on_error: rules_only)", name, err)
	r.order = append(r.order, name)
}

type detectRunKey struct{}

// WithRequestScope returns a context whose Apply calls share detector
// degradation: pass it to every Apply of one client request (one per
// failover candidate).
func WithRequestScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, detectRunKey{}, &detectRun{degraded: map[string]string{}})
}

func runFrom(ctx context.Context) *detectRun {
	if r, ok := ctx.Value(detectRunKey{}).(*detectRun); ok {
		return r
	}
	return &detectRun{degraded: map[string]string{}}
}

// detect runs every detector on one string and reports whether it skipped an
// inline base64 image. Detectors run on the text between such images, with
// offsets mapped back to text. The findings are sorted by start (longer
// first) and may overlap.
func (e *Engine) detect(ctx context.Context, text string, run *detectRun) ([]Finding, bool, error) {
	if text == "" {
		return nil, false, nil
	}
	spans := oai.DataImageSpans(text, inlineImageMin, true)
	if len(spans) == 0 {
		fs, err := e.detectSegment(ctx, text, run)
		return fs, false, err
	}
	var out []Finding
	last := 0
	for i := 0; i <= len(spans); i++ {
		end := len(text)
		if i < len(spans) {
			end = spans[i][0]
		}
		if end > last {
			fs, err := e.detectSegment(ctx, text[last:end], run)
			if err != nil {
				return nil, true, err
			}
			for _, f := range fs {
				f.Start += last
				f.End += last
				out = append(out, f)
			}
		}
		if i < len(spans) {
			last = spans[i][1]
		}
	}
	return out, true, nil
}

func (e *Engine) detectSegment(ctx context.Context, text string, run *detectRun) ([]Finding, error) {
	var all []Finding
	for _, f := range e.rules.Detect(text) {
		all = append(all, Finding{Kind: f.Kind, Start: f.Start, End: f.End, Source: SourceRules})
	}
	// case-insensitive for ASCII; fall back to exact matching when lower-casing
	// would shift byte offsets
	lower, fold := rules.LowerSameOffsets(text)
	if !fold {
		lower = text
	}
	for _, t := range e.terms {
		lt := t
		if fold && len(strings.ToLower(t)) == len(t) {
			lt = strings.ToLower(t)
		}
		for off := 0; ; {
			i := strings.Index(lower[off:], lt)
			if i < 0 {
				break
			}
			s := off + i
			all = append(all, Finding{Kind: Term, Start: s, End: s + len(lt), Source: SourceDictionary})
			off = s + len(lt)
		}
	}
	for _, d := range e.external {
		name := d.Name()
		if _, ok := run.degraded[name]; ok {
			continue
		}
		fs, err := e.detectExternal(ctx, d, text)
		if err != nil {
			if e.cfg.Sources[name].OnError == OnErrorRulesOnly {
				run.degrade(name, err)
				continue
			}
			return nil, fmt.Errorf("detector %s: %w", name, err)
		}
		for _, f := range fs {
			if e.confident(f) {
				all = append(all, f)
			}
		}
	}
	// Findings may overlap; Apply decides per overlapping cluster so that a
	// blocking finding is never dropped and masking covers the union.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Start != all[j].Start {
			return all[i].Start < all[j].Start
		}
		return all[i].End-all[i].Start > all[j].End-all[j].Start
	})
	var out []Finding
	for _, f := range all {
		if f.Start < 0 || f.End > len(text) || f.Start >= f.End {
			continue
		}
		v := text[f.Start:f.End]
		if e.never[strings.ToLower(v)] || e.never[strings.ToLower(rules.Normalize(f.Kind, v))] {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// detectExternal calls one detector plugin, through the result cache.
func (e *Engine) detectExternal(ctx context.Context, d ExternalDetector, text string) ([]Finding, error) {
	name := d.Name()
	var key [32]byte
	if e.Cache != nil {
		key = e.Cache.key(name, text)
		if fs, ok := e.Cache.get(key); ok {
			return fs, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, e.cfg.Sources[name].Timeout.D(DefaultDetectTimeout))
	defer cancel()
	fs, err := d.Detect(ctx, text)
	if err != nil {
		return nil, err
	}
	if e.Cache != nil {
		e.Cache.put(key, fs)
	}
	return fs, nil
}

// Result is the outcome of Apply.
type Result struct {
	Mode    string
	Report  *audit.Redaction
	Changed bool // the request was rewritten (placeholders present)
}

// Apply redacts req in place under `mode`, storing mappings in the vault
// bucket `vaultKey` and hashing with `userKey`. It returns *Error for a block
// (400) or a fail-closed detector failure (503).
func (e *Engine) Apply(ctx context.Context, req oai.Object, mode string, userKey []byte, vaultKey string) (*Result, error) {
	res := &Result{Mode: mode, Report: &audit.Redaction{Mode: mode, Counts: map[string]int{}}}
	if mode == ModeOff {
		return res, nil
	}
	var hard []string
	for _, m := range e.missing {
		if e.cfg.Sources[m].OnError == OnErrorRulesOnly {
			res.Report.Alerts = append(res.Report.Alerts, fmt.Sprintf("required detector unavailable: %s; continued without it (on_error: rules_only)", m))
		} else {
			hard = append(hard, m)
		}
	}
	if len(hard) > 0 {
		msg := fmt.Sprintf("required detector unavailable: %s", strings.Join(hard, ", "))
		if mode != ModeAudit && e.cfg.FailClosedOn() {
			return res, &Error{Status: http.StatusServiceUnavailable, Code: "redaction_unavailable", Msg: msg + "; request not sent (fail-closed)"}
		}
		res.Report.Alerts = append(res.Report.Alerts, msg)
	}
	run := runFrom(ctx)
	defer func() {
		// every report of the request names the sources it went without,
		// including those that failed during an earlier failover candidate
		for _, name := range run.order {
			res.Report.Alerts = append(res.Report.Alerts, run.degraded[name])
		}
	}()
	bucket := e.Vault.BucketWithTTL(vaultKey, e.vaultTTL)
	masked := map[string]string{} // value -> placeholder, for thinking text
	var blocked []string

	// processKeep redacts text but never rewrites its first `keep` bytes (a
	// synthetic "key: " context prefix); masks reaching into it are clipped.
	inlineImage := false
	processKeep := func(text string, keep int) (string, error) {
		fs, skipped, err := e.detect(ctx, text, run)
		inlineImage = inlineImage || skipped
		if err != nil {
			if mode == ModeAudit || !e.cfg.FailClosedOn() {
				res.Report.Alerts = append(res.Report.Alerts, err.Error())
				return text, nil
			}
			return "", &Error{Status: http.StatusServiceUnavailable, Code: "redaction_unavailable", Msg: err.Error() + "; request not sent (fail-closed)"}
		}
		if len(fs) == 0 {
			return text, nil
		}
		var b strings.Builder
		last := 0
		// cluster overlapping findings; the cluster takes its strongest action
		// and a mask covers the whole cluster span
		for i := 0; i < len(fs); {
			type hit struct {
				f   Finding
				act string
			}
			var hits []hit
			start, end := fs[i].Start, fs[i].End
			for ; i < len(fs) && fs[i].Start < end; i++ {
				if fs[i].End > end {
					end = fs[i].End
				}
				if act := e.findingAction(fs[i], mode); act != "keep" {
					hits = append(hits, hit{fs[i], act})
				}
			}
			if len(hits) == 0 {
				continue
			}
			act, kind := hits[0].act, hits[0].f.Kind
			for _, h := range hits[1:] {
				if actionRank[h.act] > actionRank[act] {
					act, kind = h.act, h.f.Kind
				}
			}
			for _, h := range hits {
				v := text[h.f.Start:h.f.End]
				res.Report.Counts[strings.ToLower(h.f.Kind)]++
				if len(res.Report.Samples) < 50 {
					res.Report.Samples = append(res.Report.Samples, audit.Sample{Kind: h.f.Kind, Length: utf8.RuneCountInString(v), Action: act, Source: h.f.Source, Confidence: h.f.Confidence})
				}
				if h.act == "block" {
					blocked = append(blocked, fmt.Sprintf("%s(%d)", strings.ToLower(h.f.Kind), utf8.RuneCountInString(v)))
				}
			}
			switch act {
			case "warn":
				res.Report.Warned++
			case "mask":
				if len(hits) > 1 {
					// mask the union of the masked spans
					start, end = len(text), 0
					for _, h := range hits {
						if h.act != "mask" {
							continue
						}
						start, end = min(start, h.f.Start), max(end, h.f.End)
					}
				} else {
					start, end = hits[0].f.Start, hits[0].f.End
				}
				if start < keep {
					start = keep
				}
				if start >= end {
					continue
				}
				v := text[start:end]
				ph := Placeholder(userKey, kind, v)
				bucket.Put(ph, v)
				masked[v] = ph
				b.WriteString(text[last:start])
				b.WriteString(ph)
				last = end
				res.Report.Masked++
			}
		}
		if last == 0 {
			return text, nil
		}
		b.WriteString(text[last:])
		res.Changed = true
		return b.String(), nil
	}

	if err := walkRequest(req, processKeep); err != nil {
		return res, err
	}
	if inlineImage || oai.HasImage(req) {
		if act := e.action("image", mode); act != "keep" && mode != ModeAudit {
			res.Report.Counts["image"]++
			if act == "block" {
				blocked = append(blocked, "image")
			} else if act == "warn" || act == "mask" {
				res.Report.Warned++ // images cannot be masked; they are sent as-is
				res.Report.Alerts = append(res.Report.Alerts, "image input sent unredacted")
			}
		}
	}
	if len(blocked) > 0 {
		res.Report.Blocked = true
		return res, &Error{Status: http.StatusBadRequest, Code: "redaction_blocked", Msg: "request blocked by redaction policy: " + strings.Join(dedup(blocked), ", ")}
	}
	// thinking text in history: only replace values already masked elsewhere
	if len(masked) > 0 {
		vals := make([]string, 0, len(masked))
		for v := range masked {
			vals = append(vals, v)
		}
		sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
		for _, m := range oai.Messages(req) {
			for _, k := range thinkingFields {
				if s, ok := m[k].(string); ok && s != "" {
					for _, v := range vals {
						s = strings.ReplaceAll(s, v, masked[v])
					}
					m[k] = s
				}
			}
		}
	}
	return res, nil
}

var thinkingFields = []string{"reasoning_content", "reasoning", "thinking"}

// walkRequest applies fn to every text the redaction scope covers: message
// text (system, user, assistant history, tool results), tool-call arguments in
// history, and tool descriptions. Model ids, message ids, tool names and image
// data are left alone.
func walkRequest(req oai.Object, fnKeep func(string, int) (string, error)) error {
	fn := func(s string) (string, error) { return fnKeep(s, 0) }
	for _, m := range oai.Messages(req) {
		switch c := m["content"].(type) {
		case string:
			s, err := fn(c)
			if err != nil {
				return err
			}
			m["content"] = s
		case []any:
			for _, p := range c {
				po, ok := p.(map[string]any)
				if !ok || oai.Str(po, "type") != "text" {
					continue
				}
				s, err := fn(oai.Str(po, "text"))
				if err != nil {
					return err
				}
				po["text"] = s
			}
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tco, ok := tc.(map[string]any)
				if !ok {
					return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Msg: "tool_calls entries must be objects"}
				}
				fnObj, _ := tco["function"].(map[string]any)
				if fnObj == nil {
					continue
				}
				if args, ok := fnObj["arguments"].(string); ok {
					s, err := redactJSONText(args, fnKeep)
					if err != nil {
						return err
					}
					fnObj["arguments"] = s
				}
			}
		}
	}
	if tools, ok := req["tools"].([]any); ok {
		for _, t := range tools {
			to, _ := t.(map[string]any)
			fnObj, _ := to["function"].(map[string]any)
			if fnObj == nil {
				continue
			}
			if d, ok := fnObj["description"].(string); ok {
				s, err := fn(d)
				if err != nil {
					return err
				}
				fnObj["description"] = s
			}
			if params, ok := fnObj["parameters"].(map[string]any); ok {
				if err := redactSchemaText(params, fn); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// redactSchemaText redacts the human-readable parts of a JSON Schema
// (description, title, examples, default) at any depth, leaving property
// names, types and other identifiers alone.
func redactSchemaText(node any, fn func(string) (string, error)) error {
	switch t := node.(type) {
	case map[string]any:
		for k, v := range t {
			switch k {
			case "description", "title":
				if s, ok := v.(string); ok {
					out, err := fn(s)
					if err != nil {
						return err
					}
					t[k] = out
					continue
				}
			case "examples", "default", "example":
				out, err := redactValues(v, fn)
				if err != nil {
					return err
				}
				t[k] = out
				continue
			}
			if err := redactSchemaText(v, fn); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range t {
			if err := redactSchemaText(v, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// redactValues redacts every string inside a JSON value.
func redactValues(v any, fn func(string) (string, error)) (any, error) {
	switch t := v.(type) {
	case string:
		return fn(t)
	case map[string]any:
		for k, e := range t {
			out, err := redactValues(e, fn)
			if err != nil {
				return nil, err
			}
			t[k] = out
		}
	case []any:
		for i, e := range t {
			out, err := redactValues(e, fn)
			if err != nil {
				return nil, err
			}
			t[i] = out
		}
	}
	return v, nil
}

// redactJSONText redacts a JSON document (tool-call arguments) value by
// value, so escaped strings are seen decoded and the result stays valid
// JSON. A number that contains a finding becomes a string placeholder.
// Text that is not JSON is redacted as plain text.
func redactJSONText(s string, fnKeep func(string, int) (string, error)) (string, error) {
	fn := func(s string) (string, error) { return fnKeep(s, 0) }
	v, err := oai.DecodeValue([]byte(s))
	if err != nil {
		return fn(s)
	}
	changed := false
	var walk func(any) (any, error)
	walk = func(x any) (any, error) {
		switch t := x.(type) {
		case string:
			out, err := fn(t)
			if out != t {
				changed = true
			}
			return out, err
		case json.Number:
			out, err := fn(t.String())
			if err != nil || out == t.String() {
				return t, err
			}
			changed = true
			return out, nil
		case map[string]any:
			for k, e := range t {
				if sv, ok := e.(string); ok {
					// detect with the key in front so contextual rules
					// ("password": ...) still see the field name
					prefix := k + ": "
					out, err := fnKeep(prefix+sv, len(prefix))
					if err != nil {
						return nil, err
					}
					if rest, ok := strings.CutPrefix(out, prefix); ok {
						if rest != sv {
							changed = true
						}
						t[k] = rest
						continue
					}
				}
				nv, err := walk(e)
				if err != nil {
					return nil, err
				}
				t[k] = nv
			}
		case []any:
			for i, e := range t {
				nv, err := walk(e)
				if err != nil {
					return nil, err
				}
				t[i] = nv
			}
		}
		return x, nil
	}
	v, err = walk(v)
	if err != nil || !changed {
		return s, err
	}
	return string(oai.Encode(v)), nil
}

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Placeholder returns {{KIND_hash8}} for a value.
func Placeholder(userKey []byte, kind, value string) string {
	m := hmac.New(sha256.New, userKey)
	m.Write([]byte(kind + "\x00" + rules.Normalize(kind, value)))
	return "{{" + kind + "_" + b32.EncodeToString(m.Sum(nil))[:8] + "}}"
}

var placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z]+(?:_[A-Za-z]+)*)_([A-Za-z2-7]{8})\s*\}\}`)

func canonical(kind, hash string) string {
	return "{{" + strings.ToUpper(kind) + "_" + strings.ToLower(hash) + "}}"
}

// RestoreText replaces placeholders in s, tolerating whitespace inside the
// braces and changed case. escape is applied to each restored value.
func RestoreText(s string, bucket *Bucket, escape func(string) string) (string, int) {
	n := 0
	out := placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		sm := placeholderRe.FindStringSubmatch(m)
		v, ok := bucket.Get(canonical(sm[1], sm[2]))
		if !ok {
			return m
		}
		n++
		if escape != nil {
			return escape(v)
		}
		return v
	})
	return out, n
}

func jsonEscape(v string) string {
	b, _ := json.Marshal(v)
	return string(b[1 : len(b)-1])
}

// Restore fills placeholders back into a non-streaming response: message
// content, tool_calls arguments (kept valid JSON) and thinking text.
func (e *Engine) Restore(resp oai.Object, vaultKey string, rep *audit.Redaction) {
	bucket := e.Vault.BucketWithTTL(vaultKey, e.vaultTTL)
	if bucket.Len() == 0 {
		return
	}
	choices, _ := resp["choices"].([]any)
	for _, c := range choices {
		co, _ := c.(map[string]any)
		msg, _ := co["message"].(map[string]any)
		if msg == nil {
			continue
		}
		if s, ok := msg["content"].(string); ok {
			out, n := RestoreText(s, bucket, nil)
			msg["content"] = out
			rep.Restored += n
		}
		for _, k := range thinkingFields {
			if s, ok := msg[k].(string); ok {
				out, n := RestoreText(s, bucket, nil)
				msg[k] = out
				rep.Restored += n
			}
		}
		tcs, _ := msg["tool_calls"].([]any)
		for _, tc := range tcs {
			tco, _ := tc.(map[string]any)
			fnObj, _ := tco["function"].(map[string]any)
			if fnObj == nil {
				continue
			}
			args, ok := fnObj["arguments"].(string)
			if !ok {
				continue
			}
			out, n := RestoreText(args, bucket, jsonEscape)
			if n == 0 {
				continue
			}
			if json.Valid([]byte(args)) && !json.Valid([]byte(out)) {
				rep.Alerts = append(rep.Alerts, fmt.Sprintf("tool call %s: restoring placeholders would break its JSON arguments; placeholders kept", oai.Str(fnObj, "name")))
				continue
			}
			fnObj["arguments"] = out
			rep.Restored += n
		}
	}
}

func dedup(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
