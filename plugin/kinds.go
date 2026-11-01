package plugin

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/yiyiSmallti/precinct-gatehouse/plugin/proto/gatehousev1"
)

// Plugin set keys for the other extension points.
const (
	KindAuthPolicy = "auth_policy"
	KindAuditSink  = "audit_sink"
	KindProvider   = "provider"
	KindHooks      = "hooks"
)

// Message types shared by plugins and the host.
type (
	RequestMeta        = pb.RequestMeta
	GatewayContext     = pb.GatewayContext
	ByoApi             = pb.ByoApi
	Budget             = pb.Budget
	ChatRequest        = pb.ChatRequest
	ChatResponse       = pb.ChatResponse
	HookContext        = pb.HookContext
	HookRequest        = pb.HookRequest
	HookRequestResult  = pb.HookRequestResult
	HookResponse       = pb.HookResponse
	HookResponseResult = pb.HookResponseResult
)

// Unauthenticated makes an AuthPolicy error that Gatehouse returns as 401.
func Unauthenticated(msg string) error { return status.Error(codes.Unauthenticated, msg) }

// Forbidden makes an AuthPolicy error that Gatehouse returns as 403.
func Forbidden(msg string) error { return status.Error(codes.PermissionDenied, msg) }

// ---------- AuthPolicy ----------

// AuthPolicy resolves a gateway token into a tenant context (org mode).
type AuthPolicy interface {
	Resolve(ctx context.Context, token string, meta *RequestMeta) (*GatewayContext, error)
}

// AuthPolicyPlugin adapts AuthPolicy to go-plugin.
type AuthPolicyPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	Impl AuthPolicy
}

func (p *AuthPolicyPlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterAuthPolicyServer(s, &policyServer{impl: p.Impl})
	return nil
}

func (p *AuthPolicyPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &policyClient{c: pb.NewAuthPolicyClient(c)}, nil
}

type policyServer struct {
	pb.UnimplementedAuthPolicyServer
	impl AuthPolicy
}

func (s *policyServer) Resolve(ctx context.Context, r *pb.ResolveRequest) (*pb.GatewayContext, error) {
	return s.impl.Resolve(ctx, r.GetToken(), r.GetRequestMeta())
}

type policyClient struct{ c pb.AuthPolicyClient }

func (c *policyClient) Resolve(ctx context.Context, token string, meta *RequestMeta) (*GatewayContext, error) {
	return c.c.Resolve(ctx, &pb.ResolveRequest{Token: token, RequestMeta: meta})
}

// ---------- AuditSink ----------

// AuditSink receives batches of JSON audit records.
type AuditSink interface {
	Write(ctx context.Context, records [][]byte) (accepted int, err error)
}

// AuditSinkPlugin adapts AuditSink to go-plugin.
type AuditSinkPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	Impl AuditSink
}

func (p *AuditSinkPlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterAuditSinkServer(s, &auditServer{impl: p.Impl})
	return nil
}

func (p *AuditSinkPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &auditClient{c: pb.NewAuditSinkClient(c)}, nil
}

type auditServer struct {
	pb.UnimplementedAuditSinkServer
	impl AuditSink
}

func (s *auditServer) Write(ctx context.Context, r *pb.AuditWriteRequest) (*pb.AuditWriteResponse, error) {
	n, err := s.impl.Write(ctx, r.GetRecordsJson())
	if err != nil {
		return nil, err
	}
	return &pb.AuditWriteResponse{Accepted: int32(n)}, nil
}

type auditClient struct{ c pb.AuditSinkClient }

func (c *auditClient) Write(ctx context.Context, records [][]byte) (int, error) {
	r, err := c.c.Write(ctx, &pb.AuditWriteRequest{RecordsJson: records})
	if err != nil {
		return 0, err
	}
	return int(r.GetAccepted()), nil
}

// ---------- Provider ----------

// Provider serves a non-OpenAI-compatible model. send is called once for a
// JSON body, or repeatedly with SSE bytes; the first call carries status and
// content type.
type Provider interface {
	Chat(ctx context.Context, req *ChatRequest, send func(*ChatResponse) error) error
}

// ProviderPlugin adapts Provider to go-plugin.
type ProviderPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	Impl Provider
}

func (p *ProviderPlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterProviderServer(s, &providerServer{impl: p.Impl})
	return nil
}

func (p *ProviderPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &ProviderClient{c: pb.NewProviderClient(c)}, nil
}

type providerServer struct {
	pb.UnimplementedProviderServer
	impl Provider
}

func (s *providerServer) Chat(r *pb.ChatRequest, stream grpc.ServerStreamingServer[pb.ChatResponse]) error {
	return s.impl.Chat(stream.Context(), r, stream.Send)
}

// ProviderClient is the host side of a provider plugin.
type ProviderClient struct{ c pb.ProviderClient }

// Chat starts a call; Recv returns io.EOF at the end.
func (c *ProviderClient) Chat(ctx context.Context, req *ChatRequest) (interface {
	Recv() (*ChatResponse, error)
}, error) {
	return c.c.Chat(ctx, req)
}

// ---------- Hooks ----------

// Hooks observe and may rewrite requests (not model, stream or tools).
type Hooks interface {
	OnRequest(ctx context.Context, r *HookRequest) (*HookRequestResult, error)
	OnResponse(ctx context.Context, r *HookResponse) error
}

// HooksPlugin adapts Hooks to go-plugin.
type HooksPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	Impl Hooks
}

func (p *HooksPlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterHooksServer(s, &hooksServer{impl: p.Impl})
	return nil
}

func (p *HooksPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &hooksClient{c: pb.NewHooksClient(c)}, nil
}

type hooksServer struct {
	pb.UnimplementedHooksServer
	impl Hooks
}

func (s *hooksServer) OnRequest(ctx context.Context, r *pb.HookRequest) (*pb.HookRequestResult, error) {
	return s.impl.OnRequest(ctx, r)
}

func (s *hooksServer) OnResponse(ctx context.Context, r *pb.HookResponse) (*pb.HookResponseResult, error) {
	return &pb.HookResponseResult{}, s.impl.OnResponse(ctx, r)
}

type hooksClient struct{ c pb.HooksClient }

func (c *hooksClient) OnRequest(ctx context.Context, r *HookRequest) (*HookRequestResult, error) {
	return c.c.OnRequest(ctx, r)
}

func (c *hooksClient) OnResponse(ctx context.Context, r *HookResponse) error {
	_, err := c.c.OnResponse(ctx, r)
	return err
}

// ---------- serving ----------

// Set lists the extension points one plugin executable serves; nil entries
// are skipped. A plugin usually serves one kind.
type Set struct {
	Detector   Detector
	AuthPolicy AuthPolicy
	AuditSink  AuditSink
	Provider   Provider
	Hooks      Hooks
}

// Serve runs a plugin serving every non-nil extension point in set; call it
// from the plugin's main.
func Serve(set Set) {
	ps := goplugin.PluginSet{}
	if set.Detector != nil {
		ps[KindDetector] = &DetectorPlugin{Impl: set.Detector}
	}
	if set.AuthPolicy != nil {
		ps[KindAuthPolicy] = &AuthPolicyPlugin{Impl: set.AuthPolicy}
	}
	if set.AuditSink != nil {
		ps[KindAuditSink] = &AuditSinkPlugin{Impl: set.AuditSink}
	}
	if set.Provider != nil {
		ps[KindProvider] = &ProviderPlugin{Impl: set.Provider}
	}
	if set.Hooks != nil {
		ps[KindHooks] = &HooksPlugin{Impl: set.Hooks}
	}
	goplugin.Serve(&goplugin.ServeConfig{HandshakeConfig: Handshake, Plugins: ps, GRPCServer: goplugin.DefaultGRPCServer})
}

// PluginSet is the full set the host uses to dispense any kind.
func PluginSet() goplugin.PluginSet {
	return goplugin.PluginSet{
		KindDetector:   &DetectorPlugin{},
		KindAuthPolicy: &AuthPolicyPlugin{},
		KindAuditSink:  &AuditSinkPlugin{},
		KindProvider:   &ProviderPlugin{},
		KindHooks:      &HooksPlugin{},
	}
}
