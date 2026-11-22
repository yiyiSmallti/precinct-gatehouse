package config

import (
	"path/filepath"
	"testing"
)

func TestTieredDefaultTargetInference(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name       string
		yaml       string
		wantTarget string
	}{
		{
			name: "shadow_external_base_url",
			yaml: `
providers:
  - name: p
    base_url: https://api.example.com/v1
    models: [m1]
    tiered:
      mode: shadow
      backends:
        - id: b1
          url: http://127.0.0.1:30000
          tier: 1
          max_safe_input: 1000
          promote_at: 500
`,
			wantTarget: "external",
		},
		{
			name: "shadow_loopback_base_remote_backend",
			yaml: `
providers:
  - name: p
    base_url: http://127.0.0.1:29999/v1
    models: [m1]
    tiered:
      mode: shadow
      backends:
        - id: b1
          url: http://10.0.0.5:30000
          tier: 1
          max_safe_input: 1000
          promote_at: 500
`,
			wantTarget: "external",
		},
		{
			name: "shadow_all_loopback",
			yaml: `
providers:
  - name: p
    base_url: http://127.0.0.1:29999/v1
    models: [m1]
    tiered:
      mode: shadow
      backends:
        - id: b1
          url: http://127.0.0.1:30000
          tier: 1
          max_safe_input: 1000
          promote_at: 500
`,
			wantTarget: "local",
		},
		{
			name: "active_all_loopback",
			yaml: `
providers:
  - name: p
    models: [m1]
    tiered:
      mode: active
      backends:
        - id: b1
          url: http://localhost:30000
          tier: 1
          max_safe_input: 1000
          promote_at: 500
`,
			wantTarget: "local",
		},
		{
			name: "active_remote_backend",
			yaml: `
providers:
  - name: p
    models: [m1]
    tiered:
      mode: active
      backends:
        - id: b1
          url: http://198.18.200.11:30010
          tier: 1
          max_safe_input: 1000
          promote_at: 500
`,
			wantTarget: "external",
		},
		{
			name: "explicit_local_preserved",
			yaml: `
providers:
  - name: p
    target: local
    models: [m1]
    tiered:
      mode: active
      backends:
        - id: b1
          url: http://198.18.200.11:30010
          tier: 1
          max_safe_input: 1000
          promote_at: 500
`,
			wantTarget: "local",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml), filepath.Join(dir, tc.name+".yaml"))
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}
			if got := cfg.Providers[0].Target; got != tc.wantTarget {
				t.Fatalf("got target=%q, want %q", got, tc.wantTarget)
			}
		})
	}
}
