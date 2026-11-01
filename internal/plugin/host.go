// Package plugin starts extension-point plugins as child processes and
// adapts them to Gatehouse's internal interfaces.
package plugin

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	sdk "github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

// DetectTimeout bounds one Detect call.
const DetectTimeout = 5 * time.Second

// Host owns running plugin processes.
type Host struct {
	mu      sync.Mutex
	clients []*goplugin.Client
}

// StartDetectors starts every enabled detector plugin. It returns running
// detectors and the names of required detectors (redaction.detectors) that
// are disabled or failed to start; the caller fails closed on those.
func (h *Host) StartDetectors(cfg *config.Config, log *slog.Logger) ([]redact.ExternalDetector, []string) {
	required := map[string]bool{}
	for _, d := range cfg.Redaction.Detectors {
		required[d] = true
	}
	var running []redact.ExternalDetector
	var missing []string
	for _, pc := range cfg.Plugins {
		if pc.Kind != sdk.KindDetector {
			continue
		}
		if pc.Enabled != nil && !*pc.Enabled {
			log.Warn("detector plugin disabled", "plugin", pc.Name, "required", required[pc.Name])
			if required[pc.Name] {
				missing = append(missing, pc.Name)
			}
			continue
		}
		d, err := h.startDetector(pc)
		if err != nil {
			log.Error("detector plugin failed to start", "plugin", pc.Name, "err", err)
			if required[pc.Name] {
				missing = append(missing, pc.Name)
			}
			continue
		}
		log.Info("detector plugin started", "plugin", pc.Name)
		running = append(running, d)
	}
	return running, missing
}

func (h *Host) startDetector(pc config.Plugin) (*detector, error) {
	c := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  sdk.Handshake,
		Plugins:          goplugin.PluginSet{sdk.KindDetector: &sdk.DetectorPlugin{}},
		Cmd:              exec.Command(pc.Path, pc.Args...),
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		Logger:           hclog.New(&hclog.LoggerOptions{Name: "plugin." + pc.Name, Output: io.Discard}),
		StartTimeout:     10 * time.Second,
	})
	h.mu.Lock()
	h.clients = append(h.clients, c)
	h.mu.Unlock()
	rpc, err := c.Client()
	if err != nil {
		return nil, err
	}
	raw, err := rpc.Dispense(sdk.KindDetector)
	if err != nil {
		return nil, err
	}
	return &detector{name: pc.Name, impl: raw.(sdk.Detector), client: c}, nil
}

// Close kills all plugin processes.
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		c.Kill()
	}
}

type detector struct {
	name   string
	impl   sdk.Detector
	client *goplugin.Client
}

func (d *detector) Name() string { return d.name }

// Detect calls the plugin and converts code point offsets to byte offsets.
// A plugin whose process has exited is an error (the engine fails closed).
func (d *detector) Detect(ctx context.Context, text string) ([]redact.Finding, error) {
	if d.client.Exited() {
		return nil, fmt.Errorf("plugin process exited")
	}
	ctx, cancel := context.WithTimeout(ctx, DetectTimeout)
	defer cancel()
	fs, err := d.impl.Detect(ctx, text, "", nil)
	if err != nil {
		return nil, err
	}
	// rune index -> byte offset
	idx := make([]int, 0, utf8.RuneCountInString(text)+1)
	for i := range text {
		idx = append(idx, i)
	}
	idx = append(idx, len(text))
	out := make([]redact.Finding, 0, len(fs))
	for _, f := range fs {
		if f.Start < 0 || f.End > len(idx)-1 || f.Start >= f.End || f.Kind == "" {
			continue
		}
		out = append(out, redact.Finding{Kind: f.Kind, Start: idx[f.Start], End: idx[f.End], Source: d.name})
	}
	return out, nil
}
