package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
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

func TestUpdaterConfigValidationAndDefaults(t *testing.T) {
	dir := t.TempDir()
	validPub := "ed25519:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

	t.Run("valid_oci", func(t *testing.T) {
		y := `
updater:
  enabled: true
  auto_apply: true
  source: oci
  image: registry.cn-chengdu.aliyuncs.com/smallti/gatehouse
  tag: latest
  username: ali
  password_ref: db:password
  interval: 2m
  drain_timeout: 5m
  health_timeout: 25s
  keep_releases: 5
  public_key: ` + validPub + `
`
		cfg, err := Parse([]byte(y), filepath.Join(dir, "oci.yaml"))
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if cfg.Updater.EffectiveSource() != "oci" {
			t.Fatalf("EffectiveSource = %q, want oci", cfg.Updater.EffectiveSource())
		}
		if !cfg.Updater.RequireSig() {
			t.Fatal("expected RequireSig() true when public_key is set")
		}
		if len(cfg.Updater.EffectiveFallbackDNS()) != 2 {
			t.Fatalf("expected 2 default fallback DNS servers, got %v", cfg.Updater.EffectiveFallbackDNS())
		}
	})

	t.Run("valid_github_with_empty_fallback_dns", func(t *testing.T) {
		y := `
updater:
  enabled: true
  github_repo: yiyiSmallti/precinct-gatehouse
  github_api_url: https://api.github.com/
  token_ref: env:GH_TOKEN
  fallback_dns: []
  prerelease: true
`
		cfg, err := Parse([]byte(y), filepath.Join(dir, "gh.yaml"))
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if cfg.Updater.EffectiveSource() != "github" {
			t.Fatalf("EffectiveSource = %q, want github", cfg.Updater.EffectiveSource())
		}
		if cfg.Updater.GitHubAPIURL != "https://api.github.com" {
			t.Fatalf("GitHubAPIURL = %q", cfg.Updater.GitHubAPIURL)
		}
		if len(cfg.Updater.EffectiveFallbackDNS()) != 0 {
			t.Fatalf("expected empty fallback DNS when explicitly [], got %v", cfg.Updater.EffectiveFallbackDNS())
		}

		// Round-trip through RawCopy + MarshalRaw + Parse must preserve fallback_dns: [] (and JSON [] serialization).
		rawYAML, err := MarshalRaw(cfg.RawCopy())
		if err != nil {
			t.Fatalf("MarshalRaw failed: %v", err)
		}
		if !strings.Contains(string(rawYAML), "fallback_dns: []") {
			t.Fatalf("expected MarshalRaw to preserve 'fallback_dns: []', got:\n%s", string(rawYAML))
		}
		reloaded, err := Parse(rawYAML, filepath.Join(dir, "gh-roundtrip.yaml"))
		if err != nil {
			t.Fatalf("re-Parse failed: %v", err)
		}
		if len(reloaded.Updater.EffectiveFallbackDNS()) != 0 {
			t.Fatalf("expected empty EffectiveFallbackDNS after round-trip, got %v", reloaded.Updater.EffectiveFallbackDNS())
		}
		jb, err := json.Marshal(reloaded.RawCopy().Updater)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(jb), `"fallback_dns":[]`) {
			t.Fatalf("expected JSON to preserve '\"fallback_dns\":[]', got %s", string(jb))
		}
	})

	t.Run("updater_not_restart_required", func(t *testing.T) {
		c1, err := Parse([]byte("listen: 127.0.0.1:7788\n"), filepath.Join(dir, "a.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		c2, err := Parse([]byte("listen: 127.0.0.1:7788\nupdater:\n  enabled: true\n  github_repo: a/b\n"), filepath.Join(dir, "b.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if rr := RestartRequiredDiff(c1, c2); len(rr) != 0 {
			t.Fatalf("expected updater to be hot-reloadable, got restart_required=%v", rr)
		}
	})

	invalidCases := []struct {
		name string
		yaml string
	}{
		{"bad_source", "updater:\n  source: s3\n"},
		{"enabled_missing_target", "updater:\n  enabled: true\n"},
		{"enabled_oci_missing_image", "updater:\n  enabled: true\n  source: oci\n"},
		{"bad_github_repo", "updater:\n  enabled: true\n  github_repo: onlyone\n"},
		{"interval_too_short", "updater:\n  interval: 10s\n"},
		{"bad_password_ref", "updater:\n  password_ref: plain:123\n"},
		{"bad_public_key", "updater:\n  public_key: not-base64!!\n"},
		{"require_sig_without_key", "updater:\n  require_signature: true\n"},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.yaml), filepath.Join(dir, tc.name+".yaml")); err == nil {
				t.Fatalf("expected Parse error for %s, got nil", tc.name)
			}
		})
	}
}
