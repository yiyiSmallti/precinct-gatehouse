// Package config loads and validates the Gatehouse YAML configuration.
package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"reflect"
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
	Listen         string                   `yaml:"listen,omitempty" json:"listen,omitempty"`
	Mode           string                   `yaml:"mode,omitempty" json:"mode,omitempty"`         // personal | org (org: an auth_policy plugin authorises every request)
	DataDir        string                   `yaml:"data_dir,omitempty" json:"data_dir,omitempty"` // default: directory of the config file
	MaxBodyMB      *int                     `yaml:"max_body_mb,omitempty" json:"max_body_mb,omitempty"`
	RequestLogDays *int                     `yaml:"request_log_days,omitempty" json:"request_log_days,omitempty"`
	Providers      []Provider               `yaml:"providers,omitempty" json:"providers,omitempty"`
	RoutingGroups  map[string]*RoutingGroup `yaml:"routing_groups,omitempty" json:"routing_groups,omitempty"`
	DefaultGroup   string                   `yaml:"default_group,omitempty" json:"default_group,omitempty"` // used when the request model is "auto" or empty
	ModelAliases   []ModelAlias             `yaml:"model_aliases,omitempty" json:"model_aliases,omitempty"`
	Redaction      Redaction                `yaml:"redaction,omitempty" json:"redaction,omitempty"`
	Meter          Meter                    `yaml:"meter,omitempty" json:"meter,omitempty"`
	Audit          Audit                    `yaml:"audit,omitempty" json:"audit,omitempty"`
	Plugins        []Plugin                 `yaml:"plugins,omitempty" json:"plugins,omitempty"`
	Tokens         TokenDefaults            `yaml:"tokens,omitempty" json:"tokens,omitempty"`
	Org            Org                      `yaml:"org,omitempty" json:"org,omitempty"`
	Admin          Admin                    `yaml:"admin,omitempty" json:"admin,omitempty"`
	Updater        Updater                  `yaml:"updater,omitempty" json:"updater,omitempty"`

	path string
	src  []byte
	hash string
	raw  *Config
}

// Admin configures the optional web control plane.
type Admin struct {
	Listen      string   `yaml:"listen,omitempty" json:"listen,omitempty"`
	SessionTTL  Duration `yaml:"session_ttl,omitempty" json:"session_ttl,omitempty"`   // default 12h
	IdleTimeout Duration `yaml:"idle_timeout,omitempty" json:"idle_timeout,omitempty"` // default 1h; <0 disables
}

// EffectiveSessionTTL returns the configured admin login hard cap, defaulting
// to 12h when unset or non-positive.
func (a Admin) EffectiveSessionTTL() time.Duration {
	if a.SessionTTL <= 0 {
		return 12 * time.Hour
	}
	return time.Duration(a.SessionTTL)
}

// EffectiveIdleTimeout returns the configured admin inactivity timeout,
// defaulting to 1h when unset (0) and returning 0 (disabled) when negative.
func (a Admin) EffectiveIdleTimeout() time.Duration {
	if a.IdleTimeout < 0 {
		return 0
	}
	if a.IdleTimeout == 0 {
		return time.Hour
	}
	return time.Duration(a.IdleTimeout)
}

// Updater configures built-in self-update checking, downloading, verification and cutover.
type Updater struct {
	Enabled          bool      `yaml:"enabled,omitempty" json:"enabled"`
	AutoApply        bool      `yaml:"auto_apply,omitempty" json:"auto_apply"`
	Source           string    `yaml:"source,omitempty" json:"source,omitempty"` // oci | github
	Interval         Duration  `yaml:"interval,omitempty" json:"interval,omitempty"`
	DrainTimeout     Duration  `yaml:"drain_timeout,omitempty" json:"drain_timeout,omitempty"`
	HealthTimeout    Duration  `yaml:"health_timeout,omitempty" json:"health_timeout,omitempty"`
	KeepReleases     int       `yaml:"keep_releases,omitempty" json:"keep_releases,omitempty"`
	HomeDir          string    `yaml:"home_dir,omitempty" json:"home_dir,omitempty"`
	FallbackDNS      *[]string `yaml:"fallback_dns,omitempty" json:"fallback_dns,omitempty"`
	PublicKey        string    `yaml:"public_key,omitempty" json:"public_key,omitempty"`
	RequireSignature *bool     `yaml:"require_signature,omitempty" json:"require_signature,omitempty"`
	Image            string    `yaml:"image,omitempty" json:"image,omitempty"`
	Tag              string    `yaml:"tag,omitempty" json:"tag,omitempty"`
	Username         string    `yaml:"username,omitempty" json:"username,omitempty"`
	PasswordRef      string    `yaml:"password_ref,omitempty" json:"password_ref,omitempty"`
	HasPassword      bool      `yaml:"-" json:"has_password,omitempty"`
	GitHubRepo       string    `yaml:"github_repo,omitempty" json:"github_repo,omitempty"`
	GitHubAPIURL     string    `yaml:"github_api_url,omitempty" json:"github_api_url,omitempty"`
	TokenRef         string    `yaml:"token_ref,omitempty" json:"token_ref,omitempty"`
	HasToken         bool      `yaml:"-" json:"has_token,omitempty"`
	Prerelease       bool      `yaml:"prerelease,omitempty" json:"prerelease,omitempty"`
}

// EffectiveSource returns "oci" or "github", inferring from Image or GitHubRepo when Source is empty.
func (u Updater) EffectiveSource() string {
	if s := strings.ToLower(strings.TrimSpace(u.Source)); s != "" {
		return s
	}
	if strings.TrimSpace(u.Image) != "" {
		return "oci"
	}
	if strings.TrimSpace(u.GitHubRepo) != "" {
		return "github"
	}
	return ""
}

// RequireSig reports whether Ed25519 signature verification is mandatory for updates.
func (u Updater) RequireSig() bool {
	if u.RequireSignature != nil {
		return *u.RequireSignature
	}
	return strings.TrimSpace(u.PublicKey) != ""
}

// KeepCount returns how many historical releases to retain on disk.
func (u Updater) KeepCount() int {
	if u.KeepReleases > 0 {
		return u.KeepReleases
	}
	return 5
}

// EffectiveTag returns the target OCI tag, defaulting to "latest".
func (u Updater) EffectiveTag() string {
	if t := strings.TrimSpace(u.Tag); t != "" {
		return t
	}
	return "latest"
}

// EffectiveFallbackDNS returns configured fallback DNS servers, defaulting to
// 223.5.5.5 and 119.29.29.29 when FallbackDNS is omitted (nil). An explicit
// empty slice disables fallback DNS.
func (u Updater) EffectiveFallbackDNS() []string {
	if u.FallbackDNS != nil {
		if len(*u.FallbackDNS) == 0 {
			return []string{}
		}
		return append([]string(nil), (*u.FallbackDNS)...)
	}
	return []string{"223.5.5.5", "119.29.29.29"}
}

// ValidateEd25519PublicKey checks that s encodes a 32-byte Ed25519 public key
// (with optional "ed25519:" prefix, in standard/raw base64 or hex).
func ValidateEd25519PublicKey(s string) error {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return fmt.Errorf("empty public key")
	}
	raw = strings.TrimPrefix(raw, "ed25519:")
	raw = strings.TrimSpace(raw)
	var wrongLen int
	for _, dec := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		hex.DecodeString,
	} {
		if b, err := dec(raw); err == nil {
			if len(b) == ed25519.PublicKeySize {
				return nil
			}
			wrongLen = len(b)
		}
	}
	if wrongLen > 0 {
		return fmt.Errorf("expected %d bytes, got %d", ed25519.PublicKeySize, wrongLen)
	}
	return fmt.Errorf("invalid base64 or hex encoding")
}

// ModelAlias maps a requested model name (exact or path.Match pattern) to a
// routing target ("auto", a routing group, or "<provider>/<model>").
type ModelAlias struct {
	Match string `yaml:"match,omitempty" json:"match,omitempty"`
	Use   string `yaml:"use,omitempty" json:"use,omitempty"`
}

// Org configures org mode.
type Org struct {
	PolicyTimeout Duration `yaml:"policy_timeout,omitempty" json:"policy_timeout,omitempty"` // per Resolve call, default 3s
	PolicyCache   Duration `yaml:"policy_cache,omitempty" json:"policy_cache,omitempty"`     // max time a resolved context is reused, default 5s (never past its expiry); also how long a revoked token keeps working
}

// Provider is one OpenAI-compatible upstream, or a provider plugin.
type Provider struct {
	Name         string             `yaml:"name,omitempty" json:"name,omitempty"`
	BaseURL      string             `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	Plugin       string             `yaml:"plugin,omitempty" json:"plugin,omitempty"`   // name of a provider plugin; replaces base_url
	KeyRef       string             `yaml:"key_ref,omitempty" json:"key_ref,omitempty"` // env:NAME | keychain:NAME | file:PATH | db:NAME | empty
	Target       string             `yaml:"target,omitempty" json:"target,omitempty"`   // local | external; default: local for loopback base_url, otherwise external
	Models       []string           `yaml:"models,omitempty" json:"models,omitempty"`   // model ids exposed as <provider>/<model>
	Pricing      map[string]Pricing `yaml:"pricing,omitempty" json:"pricing,omitempty"` // per model id, currency units per 1M tokens
	Timeout      Duration           `yaml:"timeout,omitempty" json:"timeout,omitempty"` // per upstream request, default 300s
	SystemPrompt string             `yaml:"system_prompt,omitempty" json:"system_prompt,omitempty"`
	ExtraBody    map[string]any     `yaml:"extra_body,omitempty" json:"extra_body,omitempty"`
	Tiered       *TieredConfig      `yaml:"tiered,omitempty" json:"tiered,omitempty"`
	HasKey       bool               `yaml:"-" json:"has_key"`
}

// TieredConfig configures built-in tiered routing and starter prefix caching for a provider.
type TieredConfig struct {
	Mode               string                 `yaml:"mode,omitempty" json:"mode,omitempty"` // shadow | active
	HeavyMissThreshold int                    `yaml:"heavy_miss_threshold,omitempty" json:"heavy_miss_threshold,omitempty"`
	HeavyStickySec     int                    `yaml:"heavy_sticky_sec,omitempty" json:"heavy_sticky_sec,omitempty"`
	SpilloverThreshold int                    `yaml:"spillover_threshold,omitempty" json:"spillover_threshold,omitempty"`
	SSEKeepaliveSec    int                    `yaml:"sse_keepalive_sec,omitempty" json:"sse_keepalive_sec,omitempty"`
	HealthIntervalSec  int                    `yaml:"health_interval_sec,omitempty" json:"health_interval_sec,omitempty"`
	HealthMaxTimeouts  int                    `yaml:"health_max_timeouts,omitempty" json:"health_max_timeouts,omitempty"`
	ZhThinkAnchor      *bool                  `yaml:"zh_think_anchor,omitempty" json:"zh_think_anchor,omitempty"`
	Backends           []TieredBackend        `yaml:"backends,omitempty" json:"backends,omitempty"`
	StarterCache       *TieredStarterCache    `yaml:"starter_cache,omitempty" json:"starter_cache,omitempty"`
	ContextCompress    *TieredContextCompress `yaml:"context_compress,omitempty" json:"context_compress,omitempty"`
}

// TieredContextCompress configures two-stage checkpoint-frozen context compression for a tiered provider.
type TieredContextCompress struct {
	Enabled                   bool   `yaml:"enabled" json:"enabled"`
	TriggerTokens             int    `yaml:"trigger_tokens,omitempty" json:"trigger_tokens,omitempty"`
	Stage1TargetTokens        int    `yaml:"stage1_target_tokens,omitempty" json:"stage1_target_tokens,omitempty"`
	KeepRecentGroups          int    `yaml:"keep_recent_groups,omitempty" json:"keep_recent_groups,omitempty"`
	KeepRecentMinTokens       int    `yaml:"keep_recent_min_tokens,omitempty" json:"keep_recent_min_tokens,omitempty"`
	KeepRecentMaxTokens       int    `yaml:"keep_recent_max_tokens,omitempty" json:"keep_recent_max_tokens,omitempty"`
	ToolMaxChars              int    `yaml:"tool_max_chars,omitempty" json:"tool_max_chars,omitempty"`
	MutationToolMaxChars      int    `yaml:"mutation_tool_max_chars,omitempty" json:"mutation_tool_max_chars,omitempty"`
	StaleReadMaxChars         int    `yaml:"stale_read_max_chars,omitempty" json:"stale_read_max_chars,omitempty"`
	UserPasteMaxChars         int    `yaml:"user_paste_max_chars,omitempty" json:"user_paste_max_chars,omitempty"`
	AssistantMaxChars         int    `yaml:"assistant_max_chars,omitempty" json:"assistant_max_chars,omitempty"`
	Stage2Enabled             *bool  `yaml:"stage2_enabled,omitempty" json:"stage2_enabled,omitempty"`
	Stage2MaxSummaryTokens    int    `yaml:"stage2_max_summary_tokens,omitempty" json:"stage2_max_summary_tokens,omitempty"`
	Stage2TimeoutSec          int    `yaml:"stage2_timeout_sec,omitempty" json:"stage2_timeout_sec,omitempty"`
	Stage2Prewarm             *bool  `yaml:"stage2_prewarm,omitempty" json:"stage2_prewarm,omitempty"`
	Stage2WarmStage1MaxTokens int    `yaml:"stage2_warm_stage1_max_tokens,omitempty" json:"stage2_warm_stage1_max_tokens,omitempty"`
	Stage2WarmStage1MinDrop   int    `yaml:"stage2_warm_stage1_min_drop_pct,omitempty" json:"stage2_warm_stage1_min_drop_pct,omitempty"`
	SummarizerURL             string `yaml:"summarizer_url,omitempty" json:"summarizer_url,omitempty"`
	SummarizerModel           string `yaml:"summarizer_model,omitempty" json:"summarizer_model,omitempty"`
	SummarizerKeyRef          string `yaml:"summarizer_key_ref,omitempty" json:"summarizer_key_ref,omitempty"`
	SummarizerAPIKey          string `yaml:"summarizer_api_key,omitempty" json:"summarizer_api_key,omitempty"`
	HasSummarizerKey          bool   `yaml:"-" json:"has_summarizer_key,omitempty"`
	CheckpointTTLHours        int    `yaml:"checkpoint_ttl_hours,omitempty" json:"checkpoint_ttl_hours,omitempty"`
	MaxCheckpoints            int    `yaml:"max_checkpoints,omitempty" json:"max_checkpoints,omitempty"`
	PersistPath               string `yaml:"persist_path,omitempty" json:"persist_path,omitempty"`
}

// TieredBackend defines one backend tier inside a tiered provider.
type TieredBackend struct {
	ID               string `yaml:"id" json:"id"`
	URL              string `yaml:"url" json:"url"`
	Tier             int    `yaml:"tier" json:"tier"`
	MaxContextTokens int    `yaml:"max_context_tokens,omitempty" json:"max_context_tokens,omitempty"`
	MaxSafeInput     int    `yaml:"max_safe_input" json:"max_safe_input"`
	PromoteAt        int    `yaml:"promote_at" json:"promote_at"`
	DemoteBelow      int    `yaml:"demote_below,omitempty" json:"demote_below,omitempty"`
	SpeedScore       int    `yaml:"speed_score,omitempty" json:"speed_score,omitempty"`
}

// TieredStarterCache configures starter prompt observation and idle warm-up.
type TieredStarterCache struct {
	Enabled           bool   `yaml:"enabled" json:"enabled"`
	CanonicalizeTools bool   `yaml:"canonicalize_tools" json:"canonicalize_tools"`
	MinTokens         int    `yaml:"min_tokens,omitempty" json:"min_tokens,omitempty"`
	MaxTokens         int    `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	MinHits           int    `yaml:"min_hits,omitempty" json:"min_hits,omitempty"`
	MaxEntries        int    `yaml:"max_entries,omitempty" json:"max_entries,omitempty"`
	KeepAliveSec      int    `yaml:"keepalive_sec,omitempty" json:"keepalive_sec,omitempty"`
	CheckIntervalSec  int    `yaml:"check_interval_sec,omitempty" json:"check_interval_sec,omitempty"`
	StartupDelaySec   int    `yaml:"startup_delay_sec,omitempty" json:"startup_delay_sec,omitempty"`
	EntryTTLHours     int    `yaml:"entry_ttl_hours,omitempty" json:"entry_ttl_hours,omitempty"`
	PersistPath       string `yaml:"persist_path,omitempty" json:"persist_path,omitempty"`
}

// Pricing is the price per one million tokens.
type Pricing struct {
	Input  float64 `yaml:"input,omitempty" json:"input,omitempty"`
	Output float64 `yaml:"output,omitempty" json:"output,omitempty"`
}

// RoutingGroup is an order/usage group with optional intent rules.
type RoutingGroup struct {
	Type              string             `yaml:"type,omitempty" json:"type,omitempty"`       // order | usage
	Members           []string           `yaml:"members,omitempty" json:"members,omitempty"` // <provider>/<model> or another group name (one level)
	Weights           map[string]float64 `yaml:"weights,omitempty" json:"weights,omitempty"` // usage groups: member -> weight (default 1)
	Classifier        string             `yaml:"classifier,omitempty" json:"classifier,omitempty"`
	ClassifierTimeout Duration           `yaml:"classifier_timeout,omitempty" json:"classifier_timeout,omitempty"` // default 8s
	Rules             []Rule             `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// Rule is an intent-routing rule. All set conditions must hold.
type Rule struct {
	Name      string `yaml:"name,omitempty" json:"name,omitempty"`
	Intent    string `yaml:"intent,omitempty" json:"intent,omitempty"`       // natural-language intent, judged by the classifier (<=200 chars)
	TokensGT  int    `yaml:"tokens_gt,omitempty" json:"tokens_gt,omitempty"` // estimated prompt tokens greater than
	TokensLT  int    `yaml:"tokens_lt,omitempty" json:"tokens_lt,omitempty"` // estimated prompt tokens less than
	HasImage  *bool  `yaml:"has_image,omitempty" json:"has_image,omitempty"` // request contains image input
	Reasoning *bool  `yaml:"reasoning,omitempty" json:"reasoning,omitempty"` // request asks for reasoning (reasoning_effort set)
	Source    string `yaml:"source,omitempty" json:"source,omitempty"`       // X-Gatehouse-Source header equals
	Time      string `yaml:"time,omitempty" json:"time,omitempty"`           // local time window "HH:MM-HH:MM"
	Use       string `yaml:"use,omitempty" json:"use,omitempty"`             // <provider>/<model> or group name
	UseEffort string `yaml:"useEffort,omitempty" json:"useEffort,omitempty"` // low | medium | high | xhigh | max
}

// Redaction configures the redaction stage.
type Redaction struct {
	ModeByTarget    map[string]string `yaml:"mode_by_target,omitempty" json:"mode_by_target,omitempty"` // local|external -> off|audit|mask|warn|block
	Categories      map[string]string `yaml:"categories,omitempty" json:"categories,omitempty"`         // category -> keep|mask|warn|block (default mask)
	Dictionary      string            `yaml:"dictionary,omitempty" json:"dictionary,omitempty"`         // file, one term per line
	Terms           []string          `yaml:"terms,omitempty" json:"terms,omitempty"`                   // inline dictionary
	NeverMask       []string          `yaml:"never_mask,omitempty" json:"never_mask,omitempty"`
	InternalDomains []string          `yaml:"internal_domains,omitempty" json:"internal_domains,omitempty"` // suffixes, default .internal .corp .lan .local .intra
	Detectors       []string          `yaml:"detectors,omitempty" json:"detectors,omitempty"`               // names of detector plugins that must be available
	FailClosed      *bool             `yaml:"fail_closed,omitempty" json:"fail_closed,omitempty"`           // default true
	StripFields     []string          `yaml:"strip_fields,omitempty" json:"strip_fields,omitempty"`         // extra top-level request fields removed before anything else (dsh_session_log always is)
	VaultTTL        Duration          `yaml:"vault_ttl,omitempty" json:"vault_ttl,omitempty"`               // default 24h of inactivity
}

// Meter configures usage metering.
type Meter struct {
	OTLP *OTLP `yaml:"otlp,omitempty" json:"otlp,omitempty"`
}

// OTLP configures the OTLP/HTTP metric exporter.
type OTLP struct {
	Endpoint string            `yaml:"endpoint,omitempty" json:"endpoint,omitempty"` // e.g. http://127.0.0.1:4318
	Interval Duration          `yaml:"interval,omitempty" json:"interval,omitempty"` // default 30s
	Headers  map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// Audit configures the local audit sink.
type Audit struct {
	Dir string `yaml:"dir,omitempty" json:"dir,omitempty"` // default <data_dir>/audit
}

// Plugin declares an extension-point plugin executable.
type Plugin struct {
	Name    string   `yaml:"name,omitempty" json:"name,omitempty"`
	Kind    string   `yaml:"kind,omitempty" json:"kind,omitempty"` // detector | auth_policy | audit_sink | provider | hooks
	Path    string   `yaml:"path,omitempty" json:"path,omitempty"`
	Args    []string `yaml:"args,omitempty" json:"args,omitempty"`
	Enabled *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// TokenDefaults configures token issuing.
type TokenDefaults struct {
	DefaultTTL Duration `yaml:"default_ttl,omitempty" json:"default_ttl,omitempty"` // default 24h for `token issue` without --ttl
}

// Duration is a time.Duration that unmarshals from "8s", "24h".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if strings.TrimSpace(s) == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	if d == 0 {
		return nil, nil
	}
	return time.Duration(d).String(), nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	if d == 0 {
		return []byte(`""`), nil
	}
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*d = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		*d = 0
		return nil
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

// Hash returns the SHA-256 hex digest of the raw configuration source bytes.
func (c *Config) Hash() string { return c.hash }

// Source returns a copy of the raw configuration YAML bytes.
func (c *Config) Source() []byte { return append([]byte(nil), c.src...) }

// RawCopy decodes the original YAML bytes without applying defaults so callers
// can modify fields and serialize back without baking in implicit defaults.
func (c *Config) RawCopy() *Config {
	var out Config
	if len(c.src) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(c.src))
		dec.KnownFields(true)
		_ = dec.Decode(&out)
	}
	normalizeConfigExtraBody(&out)
	return &out
}

// MarshalRaw serializes a raw Config (without defaults) to indented YAML.
func MarshalRaw(raw *Config) ([]byte, error) {
	if raw == nil {
		return nil, fmt.Errorf("nil config")
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(raw); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func normalizeConfigExtraBody(c *Config) {
	for i := range c.Providers {
		if c.Providers[i].ExtraBody != nil {
			if m, ok := normalizeYAMLValue(c.Providers[i].ExtraBody).(map[string]any); ok {
				c.Providers[i].ExtraBody = m
			}
		}
	}
}

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
	return Parse(b, path)
}

// Parse decodes raw YAML bytes, preserves the un-defaulted raw copy, applies
// defaults, and validates the configuration.
func Parse(data []byte, path string) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var raw Config
	decRaw := yaml.NewDecoder(bytes.NewReader(data))
	decRaw.KnownFields(true)
	if err := decRaw.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	normalizeConfigExtraBody(&raw)

	sum := sha256.Sum256(data)
	c.src = append([]byte(nil), data...)
	c.hash = hex.EncodeToString(sum[:])
	c.raw = &raw
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
		if p.Tiered != nil {
			if p.Tiered.Mode == "" {
				p.Tiered.Mode = "shadow"
			}
			for j := range p.Tiered.Backends {
				p.Tiered.Backends[j].URL = strings.TrimRight(p.Tiered.Backends[j].URL, "/")
			}
			if p.Tiered.StarterCache != nil && p.Tiered.StarterCache.PersistPath != "" {
				p.Tiered.StarterCache.PersistPath = ExpandHome(p.Tiered.StarterCache.PersistPath)
			}
			if p.Tiered.ContextCompress != nil && p.Tiered.ContextCompress.PersistPath != "" {
				p.Tiered.ContextCompress.PersistPath = ExpandHome(p.Tiered.ContextCompress.PersistPath)
			}
		}
		if p.Target == "" {
			p.Target = DefaultProviderTarget(p)
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
	c.Updater.Source = strings.ToLower(strings.TrimSpace(c.Updater.Source))
	c.Updater.Image = strings.TrimSpace(c.Updater.Image)
	c.Updater.Tag = strings.TrimSpace(c.Updater.Tag)
	c.Updater.Username = strings.TrimSpace(c.Updater.Username)
	c.Updater.GitHubRepo = strings.TrimSpace(c.Updater.GitHubRepo)
	c.Updater.GitHubAPIURL = strings.TrimRight(strings.TrimSpace(c.Updater.GitHubAPIURL), "/")
	c.Updater.PublicKey = strings.TrimSpace(c.Updater.PublicKey)
	if c.Updater.HomeDir != "" {
		c.Updater.HomeDir = ExpandHome(strings.TrimSpace(c.Updater.HomeDir))
	}
	if c.Updater.FallbackDNS != nil {
		cleaned := make([]string, 0, len(*c.Updater.FallbackDNS))
		for _, d := range *c.Updater.FallbackDNS {
			if s := strings.TrimSpace(d); s != "" {
				cleaned = append(cleaned, s)
			}
		}
		c.Updater.FallbackDNS = &cleaned
	}
}

var loopbackRe = regexp.MustCompile(`^https?://(127\.\d+\.\d+\.\d+|localhost|\[::1\])(:\d+)?(/|$)`)

// IsLoopbackURL reports whether u points to a loopback host (127.x.x.x, localhost, or [::1]).
func IsLoopbackURL(u string) bool { return loopbackRe.MatchString(u) }

// DefaultProviderTarget infers "local" when all active destinations are loopback, otherwise "external".
func DefaultProviderTarget(p *Provider) string {
	if p == nil {
		return "external"
	}
	if p.Tiered != nil {
		if len(p.Tiered.Backends) == 0 {
			return "external"
		}
		if p.Tiered.Mode != "active" && !IsLoopbackURL(p.BaseURL) {
			return "external"
		}
		for _, b := range p.Tiered.Backends {
			if !IsLoopbackURL(b.URL) {
				return "external"
			}
		}
		return "local"
	}
	if IsLoopbackURL(p.BaseURL) {
		return "local"
	}
	return "external"
}

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

func validateTiered(add func(string, ...any), pr *Provider) {
	t := pr.Tiered
	if t.Mode != "shadow" && t.Mode != "active" {
		add("providers.%s.tiered.mode: must be shadow or active, got %q", pr.Name, t.Mode)
	}
	if t.Mode == "shadow" {
		if !strings.HasPrefix(pr.BaseURL, "http://") && !strings.HasPrefix(pr.BaseURL, "https://") {
			add("providers.%s.base_url: required (http:// or https://) when tiered.mode is shadow", pr.Name)
		}
	} else if pr.BaseURL != "" && !strings.HasPrefix(pr.BaseURL, "http://") && !strings.HasPrefix(pr.BaseURL, "https://") {
		add("providers.%s.base_url: must start with http:// or https:// when set", pr.Name)
	}
	if len(t.Backends) == 0 {
		add("providers.%s.tiered.backends: at least one backend is required", pr.Name)
		return
	}
	seenID := map[string]bool{}
	for j, b := range t.Backends {
		if strings.TrimSpace(b.ID) == "" {
			add("providers.%s.tiered.backends[%d].id: required", pr.Name, j)
		} else if seenID[b.ID] {
			add("providers.%s.tiered.backends[%d].id: duplicate %q", pr.Name, j, b.ID)
		}
		seenID[b.ID] = true
		if b.Tier <= 0 {
			add("providers.%s.tiered.backends[%d].tier: must be > 0", pr.Name, j)
		}
		if !strings.HasPrefix(b.URL, "http://") && !strings.HasPrefix(b.URL, "https://") {
			add("providers.%s.tiered.backends[%d].url: must start with http:// or https://", pr.Name, j)
		}
		if b.MaxSafeInput <= 0 {
			add("providers.%s.tiered.backends[%d].max_safe_input: must be > 0", pr.Name, j)
		}
		if b.PromoteAt <= 0 {
			add("providers.%s.tiered.backends[%d].promote_at: must be > 0", pr.Name, j)
		}
	}
	if t.ContextCompress != nil {
		cc := t.ContextCompress
		if cc.SummarizerURL != "" && !strings.HasPrefix(cc.SummarizerURL, "http://") && !strings.HasPrefix(cc.SummarizerURL, "https://") {
			add("providers.%s.tiered.context_compress.summarizer_url: must start with http:// or https://", pr.Name)
		}
		if cc.SummarizerKeyRef != "" && !isValidKeyRef(cc.SummarizerKeyRef) {
			add("providers.%s.tiered.context_compress.summarizer_key_ref: must be env:NAME, keychain:NAME, file:PATH or db:NAME", pr.Name)
		}
	}
}

func isValidKeyRef(ref string) bool {
	return strings.HasPrefix(ref, "env:") ||
		strings.HasPrefix(ref, "keychain:") ||
		strings.HasPrefix(ref, "file:") ||
		(strings.HasPrefix(ref, "db:") && len(ref) > 3)
}

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
	for _, l := range [][2]string{{"listen", c.Listen}, {"admin.listen", c.Admin.Listen}} {
		if l[1] == "" {
			continue
		}
		if _, port, err := net.SplitHostPort(l[1]); err != nil || port == "" {
			add("%s: must look like 127.0.0.1:7788, got %q", l[0], l[1])
		}
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
			if pr.Tiered != nil {
				add("providers.%s: tiered cannot be used together with plugin", pr.Name)
			}
			if !c.hasPlugin(pr.Plugin, "provider") {
				add("providers.%s.plugin: %q is not declared under plugins with kind provider", pr.Name, pr.Plugin)
			}
		} else if pr.Tiered != nil {
			validateTiered(add, pr)
		} else if !strings.HasPrefix(pr.BaseURL, "http://") && !strings.HasPrefix(pr.BaseURL, "https://") {
			add("providers.%s.base_url: must start with http:// or https://", pr.Name)
		}
		if pr.Target != "local" && pr.Target != "external" {
			add("providers.%s.target: must be local or external", pr.Name)
		}
		if pr.KeyRef != "" && !isValidKeyRef(pr.KeyRef) {
			add("providers.%s.key_ref: must be env:NAME, keychain:NAME, file:PATH or db:NAME", pr.Name)
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
	validateUpdater(add, &c.Updater)
	return p
}

func validateUpdater(add func(string, ...any), u *Updater) {
	if u.Source != "" && u.Source != "oci" && u.Source != "github" {
		add("updater.source: must be oci or github, got %q", u.Source)
	}
	if u.GitHubRepo != "" {
		owner, repo, ok := strings.Cut(u.GitHubRepo, "/")
		if !ok || strings.Contains(repo, "/") || strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" {
			add("updater.github_repo: must look like owner/repo, got %q", u.GitHubRepo)
		}
	}
	if u.GitHubAPIURL != "" && !strings.HasPrefix(u.GitHubAPIURL, "http://") && !strings.HasPrefix(u.GitHubAPIURL, "https://") {
		add("updater.github_api_url: must start with http:// or https://")
	}
	if u.Interval != 0 && time.Duration(u.Interval) < 30*time.Second {
		add("updater.interval: must be at least 30s, got %s", time.Duration(u.Interval))
	}
	if time.Duration(u.DrainTimeout) < 0 {
		add("updater.drain_timeout: must be >= 0")
	}
	if time.Duration(u.HealthTimeout) < 0 {
		add("updater.health_timeout: must be >= 0")
	}
	if u.KeepReleases < 0 {
		add("updater.keep_releases: must be >= 0")
	}
	if u.PasswordRef != "" && !isValidKeyRef(u.PasswordRef) {
		add("updater.password_ref: must be env:NAME, keychain:NAME, file:PATH or db:NAME")
	}
	if u.TokenRef != "" && !isValidKeyRef(u.TokenRef) {
		add("updater.token_ref: must be env:NAME, keychain:NAME, file:PATH or db:NAME")
	}
	if u.PublicKey != "" {
		if err := ValidateEd25519PublicKey(u.PublicKey); err != nil {
			add("updater.public_key: %v", err)
		}
	}
	if u.RequireSignature != nil && *u.RequireSignature && u.PublicKey == "" {
		add("updater.require_signature: public_key is required when require_signature is true")
	}
	if u.Enabled {
		src := u.EffectiveSource()
		switch src {
		case "oci":
			if u.Image == "" {
				add("updater.image: required when updater.enabled is true and source is oci")
			}
		case "github":
			if u.GitHubRepo == "" {
				add("updater.github_repo: required when updater.enabled is true and source is github")
			}
		default:
			add("updater.source: image or github_repo is required when updater.enabled is true")
		}
	}
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

// RestartRequiredDiff compares two defaulted configs and returns the names of
// fields whose changes cannot be hot-reloaded and require a process restart.
func RestartRequiredDiff(old, next *Config) []string {
	if old == nil || next == nil {
		return nil
	}
	var out []string
	if old.Listen != next.Listen {
		out = append(out, "listen")
	}
	if old.Mode != next.Mode {
		out = append(out, "mode")
	}
	if old.DataDir != next.DataDir {
		out = append(out, "data_dir")
	}
	if old.Admin.Listen != next.Admin.Listen {
		out = append(out, "admin.listen")
	}
	if !reflect.DeepEqual(old.Plugins, next.Plugins) {
		out = append(out, "plugins")
	}
	if old.Audit.Dir != next.Audit.Dir {
		out = append(out, "audit.dir")
	}
	if !reflect.DeepEqual(old.Meter.OTLP, next.Meter.OTLP) {
		out = append(out, "meter.otlp")
	}
	if !reflect.DeepEqual(old.Org, next.Org) {
		out = append(out, "org")
	}
	return out
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

// Sample is written by `gatehouse serve` when no config exists. It boots
// with no providers; everything else is configured from the web console,
// which rewrites this file.
const Sample = `# Gatehouse configuration. Edit it from the web console instead of by hand:
#   http://127.0.0.1:7789/admin/
listen: 127.0.0.1:7788
mode: personal

admin:
  listen: 127.0.0.1:7789

redaction:
  mode_by_target: { local: audit, external: mask }
  categories: { phone: mask, id_card: mask, bank_card: mask, secret: block }
  fail_closed: true
`
