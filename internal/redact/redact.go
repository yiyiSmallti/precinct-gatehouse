// Package redact is the redaction stage. It runs after routing (so it knows
// where the request is going) and before the provider call:
//
//	detect → decide → mask → (send) → restore → audit
//
// Placeholders are {{KIND_xxxxxxxx}} where the hash is an HMAC of the
// normalised value under a per-user key, so the same value always gets the
// same placeholder for the same user, across turns. The mapping lives only in
// the in-memory Vault.
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
}

// ExternalDetector is a detector plugin. Offsets are byte offsets after conversion.
type ExternalDetector interface {
	Name() string
	Detect(ctx context.Context, text string) ([]Finding, error)
}

// Engine holds the configured detectors.
type Engine struct {
	cfg      config.Redaction
	rules    *rules.Detector
	terms    []string
	never    map[string]bool
	external []ExternalDetector
	missing  []string // required detectors that are declared but unavailable
	Vault    *Vault
}

// New builds the engine. `external` are the running detector plugins and
// `missing` names required detectors that could not be started.
func New(cfg config.Redaction, external []ExternalDetector, missing []string) (*Engine, error) {
	e := &Engine{cfg: cfg, rules: rules.New(cfg.InternalDomains), never: map[string]bool{}, external: external, missing: missing,
		Vault: NewVault(cfg.VaultTTL.D(24 * 3600e9))}
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

// Action returns what to do with a finding of `kind` under `mode`.
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

// Error is a redaction-stage failure with an HTTP status.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// DetectText runs every detector on one string.
func (e *Engine) DetectText(ctx context.Context, text string) ([]Finding, error) {
	if text == "" {
		return nil, nil
	}
	var all []Finding
	for _, f := range e.rules.Detect(text) {
		all = append(all, Finding{Kind: f.Kind, Start: f.Start, End: f.End, Source: "rules"})
	}
	// case-insensitive for ASCII; fall back to exact matching when lower-casing
	// would shift byte offsets
	lower := strings.ToLower(text)
	fold := len(lower) == len(text)
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
			all = append(all, Finding{Kind: Term, Start: s, End: s + len(lt), Source: "dictionary"})
			off = s + len(lt)
		}
	}
	for _, d := range e.external {
		fs, err := d.Detect(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("detector %s: %w", d.Name(), err)
		}
		all = append(all, fs...)
	}
	// resolve overlaps: earlier start wins, then longer span, then source order
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Start != all[j].Start {
			return all[i].Start < all[j].Start
		}
		return all[i].End-all[i].Start > all[j].End-all[j].Start
	})
	var out []Finding
	end := -1
	for _, f := range all {
		if f.Start < 0 || f.End > len(text) || f.Start >= f.End || f.Start < end {
			continue
		}
		v := text[f.Start:f.End]
		if e.never[strings.ToLower(v)] || e.never[strings.ToLower(rules.Normalize(f.Kind, v))] {
			continue
		}
		out = append(out, f)
		end = f.End
	}
	return out, nil
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
	if len(e.missing) > 0 {
		msg := fmt.Sprintf("required detector unavailable: %s", strings.Join(e.missing, ", "))
		if mode != ModeAudit && e.cfg.FailClosedOn() {
			return res, &Error{Status: http.StatusServiceUnavailable, Code: "redaction_unavailable", Msg: msg + "; request not sent (fail-closed)"}
		}
		res.Report.Alerts = append(res.Report.Alerts, msg)
	}
	bucket := e.Vault.Bucket(vaultKey)
	masked := map[string]string{} // value -> placeholder, for thinking text
	var blocked []string

	process := func(text string) (string, error) {
		fs, err := e.DetectText(ctx, text)
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
		for _, f := range fs {
			v := text[f.Start:f.End]
			act := e.action(f.Kind, mode)
			if act == "keep" {
				continue
			}
			res.Report.Counts[strings.ToLower(f.Kind)]++
			if len(res.Report.Samples) < 50 {
				res.Report.Samples = append(res.Report.Samples, audit.Sample{Kind: f.Kind, Length: utf8.RuneCountInString(v), Action: act})
			}
			switch act {
			case "block":
				blocked = append(blocked, fmt.Sprintf("%s(%d)", strings.ToLower(f.Kind), utf8.RuneCountInString(v)))
			case "warn":
				res.Report.Warned++
			case "mask":
				ph := Placeholder(userKey, f.Kind, v)
				bucket.Put(ph, v)
				masked[v] = ph
				b.WriteString(text[last:f.Start])
				b.WriteString(ph)
				last = f.End
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

	if err := walkRequest(req, process); err != nil {
		return res, err
	}
	if oai.HasImage(req) {
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
func walkRequest(req oai.Object, fn func(string) (string, error)) error {
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
				fnObj, _ := tc.(map[string]any)["function"].(map[string]any)
				if fnObj == nil {
					continue
				}
				if args, ok := fnObj["arguments"].(string); ok {
					s, err := redactJSONText(args, fn)
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
		}
	}
	return nil
}

// redactJSONText redacts a JSON document (tool-call arguments) value by
// value, so escaped strings are seen decoded and the result stays valid
// JSON. A number that contains a finding becomes a string placeholder.
// Text that is not JSON is redacted as plain text.
func redactJSONText(s string, fn func(string) (string, error)) (string, error) {
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
					out, err := fn(prefix + sv)
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
	bucket := e.Vault.Bucket(vaultKey)
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
			fnObj, _ := tc.(map[string]any)["function"].(map[string]any)
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
