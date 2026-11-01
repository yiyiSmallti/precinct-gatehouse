// Package plugin is the Gatehouse plugin interface v1 and the SDK for
// writing plugins in Go. A plugin is a separate executable that Gatehouse
// starts as a child process and talks to over gRPC, using the
// hashicorp/go-plugin handshake: the magic cookie below must be present in
// the plugin's environment and the protocol version must match.
//
// This release implements the Detector extension point. AuthPolicy,
// AuditSink, Provider and Hooks join the same protocol version with the org
// mode.
package plugin

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	pb "github.com/yiyiSmallti/precinct-gatehouse/plugin/proto/gatehousev1"
)

// ProtocolVersion is the plugin interface major version.
const ProtocolVersion = 1

// Handshake is shared by Gatehouse and every plugin.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  ProtocolVersion,
	MagicCookieKey:   "GATEHOUSE_PLUGIN",
	MagicCookieValue: "4c1f0b5e-precinct-gatehouse",
}

// Plugin set keys.
const KindDetector = "detector"

// Finding is a detected span; Start/End are Unicode code point offsets.
type Finding struct {
	Kind       string
	Start, End int
	Confidence float32
}

// Detector is the Go interface a detector plugin implements.
type Detector interface {
	Detect(ctx context.Context, text, lang string, hints map[string]string) ([]Finding, error)
}

// DetectorPlugin adapts Detector to go-plugin's gRPC transport.
type DetectorPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	Impl Detector
}

var _ goplugin.GRPCPlugin = (*DetectorPlugin)(nil)

// GRPCServer registers the implementation (plugin side).
func (p *DetectorPlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterDetectorServer(s, &detectorServer{impl: p.Impl})
	return nil
}

// GRPCClient returns a Detector backed by the plugin (host side).
func (p *DetectorPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &detectorClient{c: pb.NewDetectorClient(c)}, nil
}

type detectorServer struct {
	pb.UnimplementedDetectorServer
	impl Detector
}

func (s *detectorServer) Detect(ctx context.Context, r *pb.DetectRequest) (*pb.DetectResponse, error) {
	fs, err := s.impl.Detect(ctx, r.GetText(), r.GetLang(), r.GetHints())
	if err != nil {
		return nil, err
	}
	out := &pb.DetectResponse{}
	for _, f := range fs {
		out.Findings = append(out.Findings, &pb.Finding{Kind: f.Kind, Start: int32(f.Start), End: int32(f.End), Confidence: f.Confidence})
	}
	return out, nil
}

type detectorClient struct{ c pb.DetectorClient }

func (c *detectorClient) Detect(ctx context.Context, text, lang string, hints map[string]string) ([]Finding, error) {
	r, err := c.c.Detect(ctx, &pb.DetectRequest{Text: text, Lang: lang, Hints: hints})
	if err != nil {
		return nil, err
	}
	out := make([]Finding, 0, len(r.GetFindings()))
	for _, f := range r.GetFindings() {
		out = append(out, Finding{Kind: f.GetKind(), Start: int(f.GetStart()), End: int(f.GetEnd()), Confidence: f.GetConfidence()})
	}
	return out, nil
}

// ServeDetector runs a detector plugin; call it from the plugin's main.
func ServeDetector(impl Detector) {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         goplugin.PluginSet{KindDetector: &DetectorPlugin{Impl: impl}},
		GRPCServer:      goplugin.DefaultGRPCServer,
	})
}
