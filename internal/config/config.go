// Package config loads and validates the Gatehouse YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultMaxBodyMB is the default maximum request body size in MiB.
const DefaultMaxBodyMB = 32

// Config is the top-level configuration file (~/.gatehouse/config.yaml).
type Config struct {
	Listen        string                   `yaml:"listen"`
	Mode          string                   `yaml:"mode"`     // personal | org (org: an auth_policy plugin authorises every request)
	DataDir       string                   `yaml:"data_dir"` // default: directory of the config file
	MaxBodyMB      *int                     `yaml:"max_body_mb"`
	RequestLogDays *int                     `yaml:"request_log_days"`
	Providers      []Provider               `yaml:"providers"`
	RoutingGroups map[string]*RoutingGroup `yaml:"routing_groups"`
	DefaultGroup  string                   `yaml:"default_group"` // used when the request model is "auto" or empty
	ModelAliases  []ModelAlias             `yaml:"model_aliases"`
	Redaction     Redaction                `yaml:"redaction"`
	Meter         Meter                    `yaml:"meter"`
	Audit         Audit                    `yaml:"audit"`
	Plugins       []Plugin                 `yaml:"plugins"`
	Tokens        TokenDefaults            `yaml:"tokens"`
	Org           Org                      `yaml:"org"`
	Admin         Admin                    `yaml:"admin"`

	path string
}

// Admin configures the optional web control plane.
type Admin struct {
	Listen     string   `yaml:"listen"`
	SessionTTL Duration `yaml:"session_ttl"` // default 12h
}

// ModelAlias maps a requested model name (exact or path.Match pattern) to a
// routing target ("auto", a routing group, or "<provider>/<model>").
type ModelAlias struct {
	Match string `yaml:"match" json:"match"`
	Use   string `yaml:"use" json:"use"`
}

// Org configures org mode.
type Org struct {
	PolicyTimeout Duration `yaml:"policy_timeout"` // per Resolve call, default 3s
	PolicyCache   Duration `yaml:"policy_cache"`   // max time a resolved context is reused, default 5s (never past its expiry); also how long a revoked token keeps working
}

// Provider is one OpenAI-compatible upstream, or a provider plugin.
type Provider struct {
	Name         string             `yaml:"name"`
	BaseURL      string             `yaml:"base_url"`
	Plugin       string             `yaml:"plugin"`  // name of a provider plugin; replaces base_url
	KeyRef       string             `yaml:"key_ref"` // env:NAME | keychain:NAME | file:PATH | empty (no key)
	Target       string             `yaml:"target"`  // local | external; default: local for loopback base_url, otherwise external
	Models       []string           `yaml:"models"`  // model ids exposed as <provider>/<model>
	Pricing      map[string]Pricing `yaml:"pricing"` // per model id, currency units per 1M tokens
	Timeout      Duration           `yaml:"timeout"` // per upstream request, default 300s
	SystemPrompt string             `yaml:"system_prompt"`
	ExtraBody    map[string]any     `yaml:"extra_body"`
}

// Pricing is the price per one million tokens.
type Pricing struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
}

// RoutingGroup is an order/usage group with optional intent rules.
type RoutingGroup struct {
	Type              string             `yaml:"type"`    // order | usage
	Members           []string           `yaml:"members"` // <provider>/<model> or another group name (one level)
	Weights           map[string]float64 `yaml:"weights"` // usage groups: member -> weight (default 1)
	Classifier        string             `yaml:"classifier"`
	ClassifierTimeout Duration           `yaml:"classifier_timeout"` // default 8s
	Rules             []Rule             `yaml:"rules"`
}

// Rule is an intent-routing rule. All set conditions must hold.
type Rule struct {
	Name      string `yaml:"name"`
	Intent    string `yaml:"intent"`    // natural-language intent, judged by the classifier (<=200 chars)
	TokensGT  int    `yaml:"tokens_gt"` // estimated prompt tokens greater than
	TokensLT  int    `yaml:"tokens_lt"` // estimated prompt tokens less than
	HasImage  *bool  `yaml:"has_image"` // request contains image input
	Reasoning *bool  `yaml:"reasoning"` // request asks for reasoning (reasoning_effort set)
	Source    string `yaml:"source"`    // X-Gatehouse-Source header equals
	Time      string `yaml:"time"`      // local time window "HH:MM-HH:MM"
	Use       string `yaml:"use"`       // <provider>/<model> or group name
	UseEffort string `yaml:"useEffort"` // low | medium | high | xhigh | max
}

// Redaction configures the redaction stage.
type Redaction struct {
	ModeByTarget    map[string]string `yaml:"mode_by_target"` // local|external -> off|audit|mask|warn|block
	Categories      map[string]string `yaml:"categories"`     // category -> keep|mask|warn|block (default mask)
	Dictionary      string            `yaml:"dictionary"`     // file, one term per line
	Terms           []string          `yaml:"terms"`          // inline dictionary
	NeverMask       []string          `yaml:"never_mask"`
	InternalDomains []string          `yaml:"internal_domains"` // suffixes, default .internal .corp .lan .local .intra
	Detectors       []string          `yaml:"detectors"`        // names of detector plugins that must be available
	FailClosed      *bool             `yaml:"fail_closed"`      // default true
	StripFields     []string          `yaml:"strip_fields"`     // extra top-level request fields removed before anything else (dsh_session_log always is)
	VaultTTL        Duration          `yaml:"vault_ttl"`        // default 24h of inactivity
}

// Meter configures usage metering.
type Meter struct {
	OTLP *OTLP `yaml:"otlp"`
}

// OTLP configures the OTLP/HTTP metric exporter.
type OTLP struct {
	Endpoint string            `yaml:"endpoint"` // e.g. http://127.0.0.1:4318
	Interval Duration          `yaml:"interval"` // default 30s
	Headers  map[string]string `yaml:"headers"`
}

// Audit configures the local audit sink.
type Audit struct {
	Dir string `yaml:"dir"` // default <data_dir>/audit
}

// Plugin declares an extension-point plugin executable.
type Plugin struct {
	Name    string   `yaml:"name"`
	Kind    string   `yaml:"kind"` // detector | auth_policy | audit_sink | provider | hooks
	Path    string   `yaml:"path"`
	Args    []string `yaml:"args"`
	Enabled *bool    `yaml:"enabled"`
}

// TokenDefaults configures token issuing.
type TokenDefaults struct {
	DefaultTTL Duration `yaml:"default_ttl"` // default 24h for `token issue` without --ttl
}

// Duration is a time.Duration that unmarshals from "8s", "24h".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// D returns the value as time.Duration or def when zero.
func (d Duration) D(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

// Path returns the file this config was loaded from.
func (c *Config) Path() string { return c.path }

// DefaultPath is ~/.gatehouse/config.yaml.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".gatehouse", "config.yaml")
	}
	return filepath.Join(home, ".gatehouse", "config.yaml")
}

// ErrNotFound is returned by Load when the config file does not exist.
var ErrNotFound = errors.New("config file not found")

// Load reads, defaults and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.path = path
	c.applyDefaults()
	if errs := c.Validate(); len(errs) > 0 {
		return &c, &ValidationError{Path: path, Problems: errs}
	}
	return &c, nil
}

// ValidationError lists every problem found in a config file.
type ValidationError struct {
	Path     string
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %d problem(s):\n  - %s", e.Path, len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7788"
	}
	if c.Mode == "" {
		c.Mode = "personal"
	}
	if c.DataDir == "" {
		c.DataDir = filepath.Dir(c.path)
	}
	c.DataDir = ExpandHome(c.DataDir)
	if c.Audit.Dir == "" {
		c.Audit.Dir = filepath.Join(c.DataDir, "audit")
	}
	c.Audit.Dir = ExpandHome(c.Audit.Dir)
	for i := range c.Providers {
		p := &c.Providers[i]
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		if p.Target == "" {
			if isLoopbackURL(p.BaseURL) {
				p.Target = "local"
			} else {
				p.Target = "external"
			}
		}
		if p.ExtraBody != nil {
			if m, ok := normalizeYAMLValue(p.ExtraBody).(map[string]any); ok {
				p.ExtraBody = m
			}
		}
	}
	for _, g := range c.RoutingGroups {
		if g.Type == "" {
			g.Type = "order"
		}
	}
	if c.Redaction.ModeByTarget == nil {
		c.Redaction.ModeByTarget = map[string]string{}
	}
	if _, ok := c.Redaction.ModeByTarget["local"]; !ok {
		c.Redaction.ModeByTarget["local"] = "audit"
	}
	if _, ok := c.Redaction.ModeByTarget["external"]; !ok {
		c.Redaction.ModeByTarget["external"] = "mask"
	}
	if c.Redaction.Dictionary != "" {
		c.Redaction.Dictionary = ExpandHome(c.Redaction.Dictionary)
	}
	for i := range c.Plugins {
		c.Plugins[i].Path = ExpandHome(c.Plugins[i].Path)
	}
}

var loopbackRe = regexp.MustCompile(`^https?://(127\.\d+\.\d+\.\d+|localhost|\[::1\])(:\d+)?(/|$)`)

func isLoopbackURL(u string) bool { return loopbackRe.MatchString(u) }

// ExpandHome expands a leading "~/".
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

var (
	modes      = set("off", "audit", "mask", "warn", "block")
	actions    = set("keep", "mask", "warn", "block")
	efforts    = set("low", "medium", "high", "xhigh", "max")
	timeWindow = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d-([01]\d|2[0-3]):[0-5]\d$`)
)

func set(v ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range v {
		m[s] = true
	}
	return m
}

// Validate returns every problem in the configuration.
func (c *Config) Validate() []string {
	var p []string
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }

	if c.MaxBodyMB != nil && *c.MaxBodyMB < 0 {
		add("max_body_mb: must be >= 0")
	}
	if c.RequestLogDays != nil && *c.RequestLogDays < 0 {
		add("request_log_days: must be >= 0")
	}
	if c.Mode != "personal" && c.Mode != "org" {
		add("mode: must be personal or org, got %q", c.Mode)
	}
	policies := 0
	for _, pl := range c.Plugins {
		if pl.Kind == "auth_policy" {
			policies++
		}
	}
	switch {
	case c.Mode == "org" && policies != 1:
		add("mode: org needs exactly one plugin of kind auth_policy, found %d", policies)
	case c.Mode != "org" && policies > 0:
		add("plugins: auth_policy plugins are only used in org mode")
	}
	if len(c.Providers) == 0 {
		add("providers: at least one provider is required")
	}
	provs := map[string]*Provider{}
	for i := range c.Providers {
		pr := &c.Providers[i]
		if pr.Name == "" || strings.Contains(pr.Name, "/") {
			add("providers[%d].name: required and must not contain '/'", i)
			continue
		}
		if provs[pr.Name] != nil {
			add("providers[%d].name: duplicate %q", i, pr.Name)
		}
		provs[pr.Name] = pr
		if pr.Plugin != "" {
			if pr.BaseURL != "" {
				add("providers.%s: set base_url or plugin, not both", pr.Name)
			}
			if !c.hasPlugin(pr.Plugin, "provider") {
				add("providers.%s.plugin: %q is not declared under plugins with kind provider", pr.Name, pr.Plugin)
			}
		} else if !strings.HasPrefix(pr.BaseURL, "http://") && !strings.HasPrefix(pr.BaseURL, "https://") {
			add("providers.%s.base_url: must start with http:// or https://", pr.Name)
		}
		if pr.Target != "local" && pr.Target != "external" {
			add("providers.%s.target: must be local or external", pr.Name)
		}
		if pr.KeyRef != "" && !strings.HasPrefix(pr.KeyRef, "env:") && !strings.HasPrefix(pr.KeyRef, "keychain:") && !strings.HasPrefix(pr.KeyRef, "file:") {
			add("providers.%s.key_ref: must be env:NAME, keychain:NAME or file:PATH", pr.Name)
		}
	}
	isModel := func(ref string) bool {
		prov, _, ok := strings.Cut(ref, "/")
		return ok && provs[prov] != nil
	}
	names := make([]string, 0, len(c.RoutingGroups))
	for n := range c.RoutingGroups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		g := c.RoutingGroups[n]
		if strings.Contains(n, "/") || n == "auto" {
			add("routing_groups.%s: group names must not contain '/' and must not be \"auto\"", n)
		}
		if g.Type != "order" && g.Type != "usage" {
			add("routing_groups.%s.type: must be order or usage", n)
		}
		if len(g.Members) == 0 {
			add("routing_groups.%s.members: at least one member is required", n)
		}
		for _, m := range g.Members {
			if isModel(m) {
				continue
			}
			sub, ok := c.RoutingGroups[m]
			switch {
			case !ok:
				add("routing_groups.%s.members: %q is neither <provider>/<model> nor a group", n, m)
			case m == n:
				add("routing_groups.%s.members: a group cannot contain itself", n)
			default:
				for _, sm := range sub.Members {
					if !isModel(sm) {
						add("routing_groups.%s.members: %q nests another group; only one level of nesting is supported", n, m)
						break
					}
				}
			}
		}
		hasIntent := false
		for i, r := range g.Rules {
			where := fmt.Sprintf("routing_groups.%s.rules[%d]", n, i)
			if r.Use == "" {
				add("%s.use: required", where)
			} else if !isModel(r.Use) && c.RoutingGroups[r.Use] == nil {
				add("%s.use: %q is neither <provider>/<model> nor a group", where, r.Use)
			}
			if r.UseEffort != "" && !efforts[r.UseEffort] {
				add("%s.useEffort: must be one of low, medium, high, xhigh, max", where)
			}
			if r.Time != "" && !timeWindow.MatchString(r.Time) {
				add("%s.time: must look like 09:00-18:00", where)
			}
			if len([]rune(r.Intent)) > 200 {
				add("%s.intent: at most 200 characters", where)
			}
			if r.Intent != "" {
				hasIntent = true
			}
		}
		if hasIntent && g.Classifier == "" {
			add("routing_groups.%s.classifier: required when a rule has an intent", n)
		}
		if g.Classifier != "" && !isModel(g.Classifier) {
			add("routing_groups.%s.classifier: must be <provider>/<model>", n)
		}
	}
	if c.DefaultGroup != "" && c.RoutingGroups[c.DefaultGroup] == nil {
		add("default_group: %q is not a routing group", c.DefaultGroup)
	}
	isAliasModel := func(ref string) bool {
		prov, mod, ok := strings.Cut(ref, "/")
		if !ok || mod == "" {
			return false
		}
		pr := provs[prov]
		if pr == nil {
			return false
		}
		return len(pr.Models) == 0 || slices.Contains(pr.Models, mod)
	}
	for i, a := range c.ModelAliases {
		where := fmt.Sprintf("model_aliases[%d]", i)
		if a.Match == "" {
			add("%s.match: required", where)
		} else if _, err := path.Match(a.Match, "x"); err != nil {
			add("%s.match: invalid pattern %q: %v", where, a.Match, err)
		}
		switch {
		case a.Use == "":
			add("%s.use: required", where)
		case a.Use == "auto", c.RoutingGroups[a.Use] != nil, isAliasModel(a.Use):
		default:
			add("%s.use: %q must be \"auto\", a routing group or an existing <provider>/<model>", where, a.Use)
		}
	}
	for t, m := range c.Redaction.ModeByTarget {
		if t != "local" && t != "external" {
			add("redaction.mode_by_target: unknown target %q (local, external)", t)
		}
		if !modes[m] {
			add("redaction.mode_by_target.%s: must be one of off, audit, mask, warn, block", t)
		}
	}
	for cat, a := range c.Redaction.Categories {
		if !actions[a] {
			add("redaction.categories.%s: must be one of keep, mask, warn, block", cat)
		}
	}
	if c.Redaction.Dictionary != "" {
		if _, err := os.Stat(c.Redaction.Dictionary); err != nil {
			add("redaction.dictionary: %v", err)
		}
	}
	plugins := map[string]bool{}
	for i, pl := range c.Plugins {
		if pl.Name == "" {
			add("plugins[%d].name: required", i)
		}
		switch pl.Kind {
		case "detector", "auth_policy", "audit_sink", "provider", "hooks":
		default:
			add("plugins.%s.kind: must be detector, auth_policy, audit_sink, provider or hooks", pl.Name)
		}
		if plugins[pl.Name] {
			add("plugins.%s: duplicate name", pl.Name)
		}
		if pl.Path == "" {
			add("plugins.%s.path: required", pl.Name)
		}
		plugins[pl.Name] = true
	}
	for _, d := range c.Redaction.Detectors {
		if !c.hasPlugin(d, "detector") {
			add("redaction.detectors: %q is not declared under plugins with kind detector", d)
		}
	}
	if c.Meter.OTLP != nil && c.Meter.OTLP.Endpoint == "" {
		add("meter.otlp.endpoint: required when meter.otlp is set")
	}
	return p
}

func (c *Config) hasPlugin(name, kind string) bool {
	for _, pl := range c.Plugins {
		if pl.Name == name && pl.Kind == kind {
			return true
		}
	}
	return false
}

// FailClosedOn reports whether redaction fails closed (default true).
func (r Redaction) FailClosedOn() bool { return r.FailClosed == nil || *r.FailClosed }

// Provider returns the provider by name.
func (c *Config) Provider(name string) *Provider {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}

// MaxBodyBytes returns the maximum allowed request body in bytes, or 0 for unlimited.
func (c *Config) MaxBodyBytes() int64 {
	if c.MaxBodyMB == nil {
		return int64(DefaultMaxBodyMB) << 20
	}
	if *c.MaxBodyMB <= 0 {
		return 0
	}
	return int64(*c.MaxBodyMB) << 20
}

// LogRetentionDays returns how many days request_log rows are kept (default 90; 0 = forever).
func (c *Config) LogRetentionDays() int {
	if c.RequestLogDays == nil {
		return 90
	}
	return *c.RequestLogDays
}

// ResolveAlias maps an incoming model name to the first matching alias target,
// or returns model unchanged if no alias matches.
func (c *Config) ResolveAlias(model string) string {
	for _, a := range c.ModelAliases {
		if a.Match == model {
			return a.Use
		}
		if ok, _ := path.Match(a.Match, model); ok {
			return a.Use
		}
	}
	return model
}

func normalizeYAMLValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeYAMLValue(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalizeYAMLValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeYAMLValue(val)
		}
		return out
	default:
		return v
	}
}

// Sample is written by `gatehouse serve` when no config exists.
const Sample = `# Gatehouse personal-mode configuration.
# Docs: https://github.com/yiyiSmallti/precinct-gatehouse
listen: 127.0.0.1:7788
mode: personal

providers:
  - name: ds
    base_url: https://api.deepseek.com/v1
    key_ref: keychain:ds          # store it with: gatehouse key set ds
    models: [deepseek-chat, deepseek-reasoner]
    pricing:
      deepseek-chat: { input: 2, output: 8 }
      deepseek-reasoner: { input: 4, output: 16 }
  - name: local
    base_url: http://127.0.0.1:11434/v1   # Ollama; loopback targets count as "local"
    models: [qwen2.5:3b]

default_group: default
routing_groups:
  default:
    type: order
    members: [ds/deepseek-chat, local/qwen2.5:3b]
    classifier: local/qwen2.5:3b
    rules:
      - intent: "闲聊、问候、简单问答"
        use: local/qwen2.5:3b
        useEffort: low
      - tokens_gt: 20000
        use: ds/deepseek-chat
      - intent: "多步推理或写长文"
        use: ds/deepseek-reasoner
        useEffort: medium

redaction:
  mode_by_target: { local: audit, external: mask }
  categories: { phone: mask, id_card: mask, bank_card: mask, secret: block }
  # dictionary: ~/.gatehouse/words.txt
  never_mask: []
  fail_closed: true

# meter:
#   otlp: { endpoint: http://127.0.0.1:4318, interval: 30s }
`
