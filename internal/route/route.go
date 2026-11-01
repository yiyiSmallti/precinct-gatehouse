// Package route turns a requested model (a routing group, "auto", or a
// concrete <provider>/<model>) into an ordered candidate list, applying
// intent rules and order/usage groups. A decision is fixed for the rest of a
// turn: later calls in the same agent turn reuse it without classifying again.
package route

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
)

// Meta is request metadata that rules can match on.
type Meta struct {
	Source  string // X-Gatehouse-Source
	Session string // X-Gatehouse-Session (optional; the turn key also hashes the messages)
	Now     time.Time
}

// Decision is the routing outcome for one request.
type Decision struct {
	Requested  string   // model as requested
	Group      string   // routing group used, if any
	Rule       string   // matched rule name or "rules[i]"; empty when the group fallback applied
	Reason     string   // why: which conditions matched, classifier result, "turn-cached"
	Effort     string   // useEffort of the matched rule
	Candidates []string // <provider>/<model> in try order
	// Classifier usage to be metered (zero when the classifier was not called).
	ClassifierModel string
	ClassifierUsage oai.Usage
	ClassifierError string
}

// PrepareFunc may rewrite text before it is sent to the classifier (redaction
// when the classifier is an external model). An error aborts the request.
type PrepareFunc func(text, target string) (string, error)

// Router routes requests.
type Router struct {
	cfg     *config.Config
	reg     *provider.Registry
	Prepare PrepareFunc

	mu    sync.Mutex
	turns map[string]turnEntry
	used  map[string]float64 // usage groups: member ref -> tokens routed (in-memory, since start)
}

type turnEntry struct {
	d       Decision
	expires time.Time
}

// TurnTTL bounds how long a turn decision is remembered.
const TurnTTL = 30 * time.Minute

// New builds a router.
func New(cfg *config.Config, reg *provider.Registry) *Router {
	return &Router{cfg: cfg, reg: reg, turns: map[string]turnEntry{}, used: map[string]float64{}}
}

// ErrUnknownModel is returned for a model that is neither a group nor a provider model.
type ErrUnknownModel struct{ Model string }

func (e ErrUnknownModel) Error() string {
	return fmt.Sprintf("model %q is not a routing group or <provider>/<model>", e.Model)
}

// IsGroup reports whether name is a routing group ("auto" counts when a default group exists).
func (r *Router) IsGroup(name string) bool {
	if name == "auto" || name == "" {
		return r.cfg.DefaultGroup != ""
	}
	return r.cfg.RoutingGroups[name] != nil
}

// ResolveGroupName maps "auto"/"" to the default group.
func (r *Router) ResolveGroupName(name string) string {
	if name == "auto" || name == "" {
		return r.cfg.DefaultGroup
	}
	return name
}

// Decide routes one request.
func (r *Router) Decide(ctx context.Context, req oai.Object, meta Meta) (Decision, error) {
	requested := oai.Str(req, "model")
	d := Decision{Requested: requested}
	if !r.IsGroup(requested) {
		if _, _, ok := r.reg.Lookup(requested); !ok {
			return d, ErrUnknownModel{requested}
		}
		d.Candidates = []string{requested}
		d.Reason = "direct model"
		return d, nil
	}
	name := r.ResolveGroupName(requested)
	g := r.cfg.RoutingGroups[name]
	d.Group = name

	key := turnKey(name, meta.Session, req)
	r.mu.Lock()
	r.gcLocked(meta.Now)
	if e, ok := r.turns[key]; ok {
		r.mu.Unlock()
		c := e.d
		c.Requested = requested
		c.Reason = "turn-cached: " + e.d.Reason
		c.ClassifierModel, c.ClassifierUsage, c.ClassifierError = "", oai.Usage{}, ""
		c.Candidates = r.expandWithRule(name, g, c.Rule)
		return c, nil
	}
	r.mu.Unlock()

	tokens := oai.EstimateTokens(req)
	hasImage := oai.HasImage(req)
	reasoning := oai.Str(req, "reasoning_effort") != "" || req["reasoning"] != nil || req["thinking"] != nil
	classified := -2 // -2: not yet asked; -1: no intent matched / classifier failed; else rule index

	var hit *config.Rule
	for i := range g.Rules {
		rule := &g.Rules[i]
		var why []string
		ok := true
		if rule.TokensGT > 0 {
			ok = ok && tokens > rule.TokensGT
			why = append(why, fmt.Sprintf("tokens≈%d>%d", tokens, rule.TokensGT))
		}
		if rule.TokensLT > 0 {
			ok = ok && tokens < rule.TokensLT
			why = append(why, fmt.Sprintf("tokens≈%d<%d", tokens, rule.TokensLT))
		}
		if rule.HasImage != nil {
			ok = ok && hasImage == *rule.HasImage
			why = append(why, fmt.Sprintf("has_image=%v", hasImage))
		}
		if rule.Reasoning != nil {
			ok = ok && reasoning == *rule.Reasoning
			why = append(why, fmt.Sprintf("reasoning=%v", reasoning))
		}
		if rule.Source != "" {
			ok = ok && meta.Source == rule.Source
			why = append(why, "source="+meta.Source)
		}
		if rule.Time != "" {
			ok = ok && inWindow(rule.Time, meta.Now)
			why = append(why, "time in "+rule.Time)
		}
		if ok && rule.Intent != "" {
			if classified == -2 {
				classified = r.classify(ctx, g, req, &d)
			}
			ok = classified == i
			why = append(why, fmt.Sprintf("intent %q", rule.Intent))
		}
		if ok {
			hit = rule
			d.Rule = ruleName(rule, i)
			d.Reason = strings.Join(why, ", ")
			d.Effort = rule.UseEffort
			break
		}
	}
	if hit == nil {
		d.Reason = "no rule matched; group " + g.Type + " order"
		if d.ClassifierError != "" {
			d.Reason += " (classifier: " + d.ClassifierError + ")"
		}
	}
	d.Candidates = r.expandWithRule(name, g, d.Rule)

	r.mu.Lock()
	r.turns[key] = turnEntry{d: d, expires: meta.Now.Add(TurnTTL)}
	r.mu.Unlock()
	return d, nil
}

// expandWithRule builds candidates: the matched rule's target first, then the
// group's own members (order or usage), without duplicates.
func (r *Router) expandWithRule(name string, g *config.RoutingGroup, ruleName string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(refs []string) {
		for _, x := range refs {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	if ruleName != "" {
		for i := range g.Rules {
			if ruleNameOf(g, i) == ruleName {
				add(r.expand(g.Rules[i].Use))
				break
			}
		}
	}
	add(r.expand(name))
	return out
}

func ruleNameOf(g *config.RoutingGroup, i int) string { return ruleName(&g.Rules[i], i) }

func ruleName(rule *config.Rule, i int) string {
	if rule.Name != "" {
		return rule.Name
	}
	return fmt.Sprintf("rules[%d]", i)
}

// expand turns a model ref or group name into model refs.
func (r *Router) expand(ref string) []string {
	g, ok := r.cfg.RoutingGroups[ref]
	if !ok {
		return []string{ref}
	}
	members := append([]string(nil), g.Members...)
	if g.Type == "usage" {
		r.mu.Lock()
		load := func(m string) float64 {
			w := g.Weights[m]
			if w <= 0 {
				w = 1
			}
			return r.used[m] / w
		}
		sort.SliceStable(members, func(a, b int) bool { return load(members[a]) < load(members[b]) })
		r.mu.Unlock()
	}
	var out []string
	for _, m := range members {
		out = append(out, r.expand(m)...)
	}
	return out
}

// Account records tokens routed to a member (for usage groups). Members that
// are groups get credited as well, so nested usage groups balance too.
func (r *Router) Account(model string, tokens int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.used[model] += float64(tokens)
	for name, g := range r.cfg.RoutingGroups {
		for _, m := range g.Members {
			if sub, ok := r.cfg.RoutingGroups[m]; ok && m != name {
				for _, sm := range sub.Members {
					if sm == model {
						r.used[m] += float64(tokens)
					}
				}
			}
		}
	}
}

func (r *Router) gcLocked(now time.Time) {
	if len(r.turns) < 1024 {
		return
	}
	for k, e := range r.turns {
		if now.After(e.expires) {
			delete(r.turns, k)
		}
	}
}

// turnKey identifies an agent turn: the conversation up to and including the
// last user message. Tool-call round trips after it belong to the same turn.
func turnKey(group, session string, req oai.Object) string {
	h := sha256.New()
	io.WriteString(h, group+"\x00"+session+"\x00")
	ms := oai.Messages(req)
	last := -1
	for i, m := range ms {
		if oai.Str(m, "role") == "user" {
			last = i
		}
	}
	for i := 0; i <= last; i++ {
		h.Write(oai.Encode(map[string]any{"r": ms[i]["role"], "c": ms[i]["content"]}))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func inWindow(w string, now time.Time) bool {
	from, to, _ := strings.Cut(w, "-")
	m := now.Hour()*60 + now.Minute()
	f, t := hm(from), hm(to)
	if f <= t {
		return m >= f && m < t
	}
	return m >= f || m < t // wraps midnight
}

func hm(s string) int {
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	return h*60 + m
}

var firstInt = regexp.MustCompile(`\d+`)

// classify asks the group's classifier which intent the last user message
// matches. It returns the rule index, or -1 when nothing matched or the
// classifier failed or timed out (failure is not fatal: the group fallback applies).
func (r *Router) classify(ctx context.Context, g *config.RoutingGroup, req oai.Object, d *Decision) int {
	var idx []int
	var b strings.Builder
	b.WriteString("你是请求路由分类器。判断用户最后一条消息属于下列哪一类意图，只回答类别编号；都不符合则回答 0。\n")
	for i, rule := range g.Rules {
		if rule.Intent != "" {
			idx = append(idx, i)
			fmt.Fprintf(&b, "%d. %s\n", len(idx), rule.Intent)
		}
	}
	text := oai.LastUserText(req)
	if rs := []rune(text); len(rs) > 2000 {
		text = string(rs[:2000])
	}
	p, model, _ := r.reg.Lookup(g.Classifier)
	d.ClassifierModel = g.Classifier
	if r.Prepare != nil {
		t, err := r.Prepare(text, p.Target)
		if err != nil {
			d.ClassifierError = "skipped: " + err.Error()
			return -1
		}
		text = t
	}
	body := oai.Encode(map[string]any{
		"model": model, "temperature": 0, "max_tokens": 8, "stream": false,
		"messages": []any{
			map[string]any{"role": "system", "content": b.String()},
			map[string]any{"role": "user", "content": text},
		},
	})
	cctx, cancel := context.WithTimeout(ctx, g.ClassifierTimeout.D(8*time.Second))
	defer cancel()
	resp, err := p.Chat(cctx, body)
	if err != nil {
		if cctx.Err() != nil {
			d.ClassifierError = "timeout"
		} else {
			d.ClassifierError = err.Error()
		}
		return -1
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		d.ClassifierError = err.Error()
		return -1
	}
	o, err := oai.Decode(raw)
	if err != nil {
		d.ClassifierError = "bad classifier response"
		return -1
	}
	if u, ok := oai.UsageOf(o); ok {
		d.ClassifierUsage = u
	}
	answer := ""
	if ch, ok := o["choices"].([]any); ok && len(ch) > 0 {
		if c0, ok := ch[0].(map[string]any); ok {
			if msg, ok := c0["message"].(map[string]any); ok {
				answer = oai.Str(msg, "content")
			}
		}
	}
	n, err := strconv.Atoi(firstInt.FindString(answer))
	if err != nil || n < 1 || n > len(idx) {
		return -1
	}
	return idx[n-1]
}
