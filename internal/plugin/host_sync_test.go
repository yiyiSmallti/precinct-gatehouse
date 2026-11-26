package plugin

import (
	"io"
	"log/slog"
	"testing"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	sdk "github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

func TestSyncDetectorsLifecycle(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &Host{}
	defer h.Close()

	// Start with no detector plugins.
	cfg0 := &config.Config{}
	dets0, missing0 := h.SyncDetectors(cfg0, log)
	if len(dets0) != 0 || len(missing0) != 0 {
		t.Fatalf("empty sync = (%d, %v), want (0, [])", len(dets0), missing0)
	}

	// Add an enabled detector (binary does not exist; supervised in restarting state).
	enabled := true
	disabled := false
	cfg1 := &config.Config{
		Plugins: []config.Plugin{
			{Name: "astrlink", Kind: sdk.KindDetector, Path: "/nonexistent/astrlink-detector", Enabled: &enabled},
		},
		Redaction: config.Redaction{Detectors: []string{"astrlink"}},
	}
	dets1, missing1 := h.SyncDetectors(cfg1, log)
	if len(dets1) != 1 || len(missing1) != 0 {
		t.Fatalf("add detector sync = (%d, %v), want (1, [])", len(dets1), missing1)
	}
	if len(h.Status()) != 1 {
		t.Fatalf("host status len = %d, want 1", len(h.Status()))
	}

	// Sync again with unchanged config: must reuse existing supervised entry.
	dets2, _ := h.SyncDetectors(cfg1, log)
	if len(dets2) != 1 || dets2[0].(*detector).s != dets1[0].(*detector).s {
		t.Fatalf("expected unchanged detector to be reused")
	}

	// Disable the detector: removed from host and reported in missing.
	cfg2 := &config.Config{
		Plugins: []config.Plugin{
			{Name: "astrlink", Kind: sdk.KindDetector, Path: "/nonexistent/astrlink-detector", Enabled: &disabled},
		},
		Redaction: config.Redaction{Detectors: []string{"astrlink"}},
	}
	dets3, missing3 := h.SyncDetectors(cfg2, log)
	if len(dets3) != 0 || len(missing3) != 1 || missing3[0] != "astrlink" {
		t.Fatalf("disable sync = (%d, %v), want (0, [astrlink])", len(dets3), missing3)
	}
	if len(h.Status()) != 0 {
		t.Fatalf("host status after disable = %d, want 0", len(h.Status()))
	}
}
