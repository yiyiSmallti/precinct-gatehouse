// Package provider sends Chat Completions requests to OpenAI-compatible
// upstreams (DeepSeek, other compatible endpoints, local vLLM/Ollama) and
// tracks their health for failover.
package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/keystore"
)

// Cooldown is how long a failed model is skipped by order groups.
const Cooldown = 30 * time.Second

type ctxKey int

const compressHdrCtxKey ctxKey = 1

type compressHeaders struct {
	noCompress      string
	contextCompress string
}

// WithCompressHeaders annotates ctx with client context-compression control headers
// so Provider.Chat forwards them upstream.
func WithCompressHeaders(ctx context.Context, noCompress, contextCompress string) context.Context {
	if noCompress == "" && contextCompress == "" {
		return ctx
	}
	return context.WithValue(ctx, compressHdrCtxKey, compressHeaders{
		noCompress:      noCompress,
		contextCompress: contextCompress,
	})
}

func compressHeadersFromContext(ctx context.Context) (string, string) {
	v, _ := ctx.Value(compressHdrCtxKey).(compressHeaders)
	return v.noCompress, v.contextCompress
}

// Registry holds every configured provider.
type Registry struct {
	byName map[string]*Provider
	health sync.Map // model ref -> time.Time (unhealthy until)
}

// Provider is one upstream.
type Provider struct {
	Name         string
	BaseURL      string
	Target       string
	Models       []string
	Pricing      map[string]config.Pricing
	SystemPrompt string
	ExtraBody    map[string]any
	key          string
	client       *http.Client
	plug         func(ctx context.Context, body []byte) (*http.Response, error)
}

// SetPlugin routes a provider's calls to a provider plugin.
func (r *Registry) SetPlugin(name string, fn func(ctx context.Context, body []byte) (*http.Response, error)) {
	if p := r.byName[name]; p != nil {
		p.plug = fn
	}
}

// NewRegistry resolves keys and builds HTTP clients.
// If resolve is nil, keystore.Resolve is used.
func NewRegistry(cfg *config.Config, resolve func(ref string) (string, error)) (*Registry, error) {
	if resolve == nil {
		resolve = keystore.Resolve
	}
	r := &Registry{byName: map[string]*Provider{}}
	for _, pc := range cfg.Providers {
		key, err := resolve(pc.KeyRef)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", pc.Name, err)
		}
		baseURL := pc.BaseURL
		if baseURL == "" && pc.Tiered != nil {
			baseURL = "http://tiered.invalid/v1"
		}
		r.byName[pc.Name] = &Provider{
			Name: pc.Name, BaseURL: baseURL, Target: pc.Target, Models: pc.Models, Pricing: pc.Pricing,
			SystemPrompt: pc.SystemPrompt, ExtraBody: pc.ExtraBody, key: key,
			client: &http.Client{
				Timeout: pc.Timeout.D(300 * time.Second),
				// never follow redirects: a redirect could replay the request
				// body to a host outside the provider's trust boundary
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			},
		}
	}
	return r, nil
}

// SetTransport overrides the underlying HTTP RoundTripper for a named provider.
func (r *Registry) SetTransport(name string, rt http.RoundTripper) {
	if p := r.byName[name]; p != nil && rt != nil {
		p.client.Transport = rt
	}
}

// Lookup splits "<provider>/<model>" and returns the provider and model id.
func (r *Registry) Lookup(ref string) (*Provider, string, bool) {
	name, model, ok := strings.Cut(ref, "/")
	if !ok {
		return nil, "", false
	}
	p := r.byName[name]
	return p, model, p != nil
}

// All returns providers in no particular order.
func (r *Registry) All() []*Provider {
	out := make([]*Provider, 0, len(r.byName))
	for _, p := range r.byName {
		out = append(out, p)
	}
	return out
}

// Healthy reports whether a model is outside its failure cooldown.
func (r *Registry) Healthy(ref string) bool {
	v, ok := r.health.Load(ref)
	return !ok || time.Now().After(v.(time.Time))
}

// MarkFailed starts the cooldown for a model.
func (r *Registry) MarkFailed(ref string) { r.health.Store(ref, time.Now().Add(Cooldown)) }

// MarkOK clears the cooldown.
func (r *Registry) MarkOK(ref string) { r.health.Delete(ref) }

// Cost prices a call.
func (p *Provider) Cost(model string, prompt, completion int) float64 {
	pr, ok := p.Pricing[model]
	if !ok {
		return 0
	}
	return (float64(prompt)*pr.Input + float64(completion)*pr.Output) / 1e6
}

// UpstreamError is a failed upstream call. Retryable errors trigger failover.
type UpstreamError struct {
	Status    int
	Body      []byte
	Header    http.Header
	Err       error
	Retryable bool
}

func (e *UpstreamError) Error() string {
	// The body is never included: providers may echo prompt text, and this
	// message ends up in audit records and client error messages.
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("upstream status %d", e.Status)
}

// Chat posts body to <base_url>/chat/completions. On success the caller owns
// resp.Body. Any non-2xx is returned as *UpstreamError with the body read.
func (p *Provider) Chat(ctx context.Context, body []byte) (*http.Response, error) {
	if p.plug != nil {
		return p.plug(ctx, body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &UpstreamError{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if p.key != "" {
		req.Header.Set("Authorization", "Bearer "+p.key)
	}
	if noComp, compCtrl := compressHeadersFromContext(ctx); noComp != "" || compCtrl != "" {
		if noComp != "" {
			req.Header.Set("X-No-Compress", noComp)
		}
		if compCtrl != "" {
			req.Header.Set("X-Context-Compress", compCtrl)
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, &UpstreamError{Err: err, Retryable: ctx.Err() == nil}
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, &UpstreamError{Status: resp.StatusCode, Body: b, Header: resp.Header.Clone(), Retryable: resp.StatusCode >= 500 || resp.StatusCode == 429 || resp.StatusCode == 408}
	}
	return resp, nil
}
