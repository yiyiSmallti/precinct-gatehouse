// Package policy resolves the tenant context of a request in org mode by
// calling the AuthPolicy plugin. It fails closed: when the plugin is not
// running, times out or errors, the request is refused with 503.
package policy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/plugin"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/token"
	sdk "github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

// Context is a resolved tenant context.
type Context struct {
	Tenant, User, Department  string
	OnBehalfOf                string
	InstanceID, WorkspaceID   string
	DataClass                 string // floor for the request
	AllowedGroups, Models     []string
	ByoAPI                    bool
	ByoMaxClass               string
	TokensRemaining           *int64
	DepartmentTokensRemaining *int64
	PolicyVersion             int64
	Expires                   time.Time
}

// Scope converts the context into a token scope for model checks.
func (c *Context) Scope() token.Scope {
	return token.Scope{Groups: c.AllowedGroups, Models: c.Models, DataClass: "confidential"}
}

// Error is a policy failure with its HTTP status.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// Resolver calls the AuthPolicy plugin with a short-lived cache.
type Resolver struct {
	name    string
	plug    *plugin.Started // nil when the plugin is disabled; supervised (restarted) otherwise
	timeout time.Duration
	maxAge  time.Duration

	mu    sync.Mutex
	cache map[[32]byte]entry
}

type entry struct {
	ctx     *Context
	expires time.Time
}

// New builds a resolver; plug may be nil (every request then gets 503).
func New(cfg *config.Config, name string, plug *plugin.Started) *Resolver {
	return &Resolver{
		name: name, plug: plug,
		timeout: cfg.Org.PolicyTimeout.D(3 * time.Second),
		maxAge:  cfg.Org.PolicyCache.D(5 * time.Second),
		cache:   map[[32]byte]entry{},
	}
}

// Available reports whether the policy plugin is running. It is false while
// the plugin restarts after a crash.
func (r *Resolver) Available() bool { return r.plug != nil && !r.plug.Exited() }

// Name is the policy plugin's configured name.
func (r *Resolver) Name() string { return r.name }

// Resolve authorises tok for meta. Errors are *Error.
func (r *Resolver) Resolve(ctx context.Context, tok string, meta *sdk.RequestMeta) (*Context, error) {
	if !r.Available() {
		// fail closed even for cached contexts: a dead policy plugin cannot revoke
		return nil, &Error{Status: http.StatusServiceUnavailable, Code: "policy_unavailable", Msg: fmt.Sprintf("policy plugin %s is not running; org mode refuses requests (fail-closed)", r.name)}
	}
	key := sha256.Sum256([]byte(tok + "\x00" + meta.GetModel() + "\x00" + meta.GetRouteGroup()))
	now := time.Now()
	r.mu.Lock()
	if e, ok := r.cache[key]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e.ctx, nil
	}
	r.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	raw, ok := r.plug.Impl()
	if !ok {
		// exited after the check above
		return nil, &Error{Status: http.StatusServiceUnavailable, Code: "policy_unavailable", Msg: fmt.Sprintf("policy plugin %s is not running; org mode refuses requests (fail-closed)", r.name)}
	}
	gc, err := raw.(sdk.AuthPolicy).Resolve(cctx, tok, meta)
	if err != nil {
		st, _ := status.FromError(err)
		switch st.Code() {
		case codes.Unauthenticated:
			return nil, &Error{Status: http.StatusUnauthorized, Code: "invalid_token", Msg: "policy: " + st.Message()}
		case codes.PermissionDenied:
			return nil, &Error{Status: http.StatusForbidden, Code: "policy_denied", Msg: "policy: " + st.Message()}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Status: http.StatusServiceUnavailable, Code: "policy_unavailable", Msg: fmt.Sprintf("policy plugin %s: %s; request refused (fail-closed)", r.name, st.Code())}
	}
	c := convert(gc)
	if token.ClassRank(c.DataClass) < 0 {
		return nil, &Error{Status: http.StatusServiceUnavailable, Code: "policy_invalid", Msg: fmt.Sprintf("policy plugin %s returned unknown data class %q", r.name, c.DataClass)}
	}
	exp := now.Add(r.maxAge)
	if !c.Expires.IsZero() && c.Expires.Before(exp) {
		exp = c.Expires
	}
	if exp.After(now) {
		r.mu.Lock()
		if len(r.cache) > 4096 {
			for k, e := range r.cache {
				if !now.Before(e.expires) {
					delete(r.cache, k)
				}
			}
			if len(r.cache) > 4096 {
				r.cache = map[[32]byte]entry{}
			}
		}
		r.cache[key] = entry{ctx: c, expires: exp}
		r.mu.Unlock()
	}
	return c, nil
}

func convert(g *sdk.GatewayContext) *Context {
	c := &Context{
		Tenant: g.GetTenant(), User: g.GetUser(), Department: g.GetDepartment(), OnBehalfOf: g.GetOnBehalfOf(),
		InstanceID: g.GetInstanceId(), WorkspaceID: g.GetWorkspaceId(), DataClass: g.GetDataClass(),
		AllowedGroups: g.GetAllowedGroups(), Models: g.GetModels(),
		ByoAPI: g.GetByoApi().GetEnabled(), ByoMaxClass: g.GetByoApi().GetMaxClass(),
		PolicyVersion: g.GetPolicyVersion(),
	}
	if c.DataClass == "" {
		c.DataClass = "internal"
	}
	if b := g.GetBudget(); b != nil {
		if b.GetHasTokensRemaining() {
			v := b.GetTokensRemaining()
			c.TokensRemaining = &v
		}
		if b.GetHasDepartmentTokensRemaining() {
			v := b.GetDepartmentTokensRemaining()
			c.DepartmentTokensRemaining = &v
		}
	}
	if t := g.GetExpiresAtUnix(); t > 0 {
		c.Expires = time.Unix(t, 0)
	}
	return c
}
