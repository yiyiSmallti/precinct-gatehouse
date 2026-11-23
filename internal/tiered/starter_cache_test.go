package tiered

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCanonicalizeTools(t *testing.T) {
	bodyA := []byte(`{"model":"DeepSeek-V4.1-Flash-1017","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Write","description":"w"}},{"type":"function","function":{"name":"Bash","description":"b"}},{"type":"function","function":{"name":"Read","description":"r"}}]}`)
	bodyB := []byte(`{"model":"DeepSeek-V4.1-Flash-1017","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Read","description":"r"}},{"type":"function","function":{"name":"Write","description":"w"}},{"type":"function","function":{"name":"Bash","description":"b"}}]}`)

	outA, changedA, cntA := canonicalizeTools(bodyA)
	outB, changedB, cntB := canonicalizeTools(bodyB)
	if !changedA || !changedB || cntA != 3 || cntB != 3 {
		t.Fatalf("expected both reordered with count 3, got changed=(%v,%v) cnt=(%d,%d)", changedA, changedB, cntA, cntB)
	}
	if !bytes.Equal(outA, outB) {
		t.Fatalf("canonical outputs differ:\nA=%s\nB=%s", outA, outB)
	}

	outC, changedC, _ := canonicalizeTools(outA)
	if changedC {
		t.Fatalf("expected already-sorted tools to return changed=false")
	}
	if !bytes.Equal(outC, outA) {
		t.Fatalf("expected identical bytes")
	}
}

func TestStarterObserveAndLCPShrink(t *testing.T) {
	staticPart := strings.Repeat("You are an expert coding agent with strict safety and verification rules.\n", 80)
	req1, _ := json.Marshal(map[string]any{
		"model": "DeepSeek-V4.1-Flash-1017",
		"messages": []map[string]any{
			{"role": "system", "content": staticPart + "\nWorking directory: /workspace/project-alpha\nGit branch: main"},
			{"role": "user", "content": "fix bug"},
		},
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "Bash", "description": "run bash"}},
			{"type": "function", "function": map[string]any{"name": "Read", "description": "read file"}},
		},
	})
	req2, _ := json.Marshal(map[string]any{
		"model": "DeepSeek-V4.1-Flash-1017",
		"messages": []map[string]any{
			{"role": "system", "content": staticPart + "\nWorking directory: /workspace/project-beta\nGit branch: dev"},
			{"role": "user", "content": "add feature"},
		},
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "Read", "description": "read file"}},
			{"type": "function", "function": map[string]any{"name": "Bash", "description": "run bash"}},
		},
	})

	req1 = bytes.ReplaceAll(req1, []byte(`":`), []byte(`": `))
	req2 = bytes.ReplaceAll(req2, []byte(`":`), []byte(`": `))
	req1, _, _ = canonicalizeTools(req1)
	req2, _, _ = canonicalizeTools(req2)

	t1 := extractStarterTemplate(req1, "claude-code/1.0", 512, 100000)
	t2 := extractStarterTemplate(req2, "claude-code/1.0", 512, 100000)
	if t1 == nil || t2 == nil {
		t.Fatalf("expected non-nil starter templates")
	}
	if t1.FamilyHash != t2.FamilyHash {
		t.Fatalf("expected same family hash after canonicalizing tools, got %s vs %s", t1.FamilyHash, t2.FamilyHash)
	}

	tmpDir := t.TempDir()
	persist := filepath.Join(tmpDir, "starters.json")
	cfg := RouterConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		Backends: []BackendConfig{
			{ID: "gpu2-200k", URL: "http://127.0.0.1:1", Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 185000, PromoteAt: 150000, SpeedScore: 55},
		},
		StarterCache: &StarterCacheConfig{
			Enabled:           true,
			CanonicalizeTools: true,
			MinTokens:         512,
			MaxTokens:         100000,
			MinHits:           2,
			MaxEntries:        16,
			KeepAliveSec:      240,
			PersistPath:       persist,
		},
	}
	r, err := newRouter("test", cfg, persist, nil, nil)
	if err != nil {
		t.Fatalf("newRouter: %v", err)
	}
	r.starters.Observe(t1)
	r.starters.Observe(t2)

	e := r.starters.entries[t1.FamilyHash]
	if e == nil {
		t.Fatalf("missing entry")
	}
	if e.Hits != 2 || !e.LCPShrunk || !e.NeedsWarm {
		t.Fatalf("expected hits=2, LCPShrunk=true, NeedsWarm=true, got hits=%d shrunk=%v needsWarm=%v", e.Hits, e.LCPShrunk, e.NeedsWarm)
	}
	txt := extractMessagePlainText(e.SystemMsgs[0])
	if strings.Contains(txt, "project-alpha") || strings.Contains(txt, "project-beta") {
		t.Fatalf("LCP should have stripped dynamic cwd tail, got tail=%q", txt[len(txt)-80:])
	}
	if !strings.HasPrefix(txt, "You are an expert coding agent") {
		t.Fatalf("LCP lost static prefix")
	}

	// Regression: observing a 3rd request whose existing prefix is a strict prefix of incoming must not shrink again.
	e.NeedsWarm = false
	req3, _ := json.Marshal(map[string]any{
		"model": "DeepSeek-V4.1-Flash-1017",
		"messages": []map[string]any{
			{"role": "system", "content": staticPart + "\nWorking directory: /workspace/project-gamma\nGit branch: release"},
			{"role": "user", "content": "check status"},
		},
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "Bash", "description": "run bash"}},
			{"type": "function", "function": map[string]any{"name": "Read", "description": "read file"}},
		},
	})
	t3 := extractStarterTemplate(req3, "claude-code/1.0", 512, 100000)
	r.starters.Observe(t3)
	txtAfter3 := extractMessagePlainText(e.SystemMsgs[0])
	if txtAfter3 != txt {
		t.Fatalf("LCP shrank repeatedly on 3rd observation! len %d -> %d", len(txt), len(txtAfter3))
	}
	if e.NeedsWarm {
		t.Fatalf("expected NeedsWarm to remain false when existing is already a prefix of incoming")
	}

	r.starters.saveToDisk()
	r2, err := newRouter("test", cfg, persist, nil, nil)
	if err != nil {
		t.Fatalf("reload newRouter: %v", err)
	}
	re := r2.starters.entries[t1.FamilyHash]
	if re == nil || !re.NeedsWarm || re.Hits != 3 {
		t.Fatalf("expected persisted entry reloaded with NeedsWarm=true hits=3, got %+v", re)
	}
}

func TestWarmupIdleBackendAndPrefixSeeding(t *testing.T) {
	var warmCalls atomic.Int32
	mockBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" || r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"DeepSeek-V4.1-Flash-1017"}]}`))
			return
		}
		warmCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","choices":[{"message":{"role":"assistant","content":"."}}],"usage":{"prompt_tokens":26010,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":0}}}`))
	}))
	defer mockBackend.Close()

	tmpDir := t.TempDir()
	persist := filepath.Join(tmpDir, "starters.json")
	cfg := RouterConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		Backends: []BackendConfig{
			{ID: "gpu2-200k", URL: mockBackend.URL, Tier: 1, MaxContextTokens: 204800, MaxSafeInput: 185000, PromoteAt: 150000, SpeedScore: 55},
			{ID: "gpu1-1m", URL: mockBackend.URL, Tier: 3, MaxContextTokens: 1048576, MaxSafeInput: 1048576, PromoteAt: 1048576, SpeedScore: 110},
		},
		StarterCache: &StarterCacheConfig{
			Enabled:           true,
			CanonicalizeTools: true,
			MinTokens:         512,
			MaxTokens:         100000,
			MinHits:           2,
			KeepAliveSec:      240,
			PersistPath:       persist,
		},
	}
	r, err := newRouter("test", cfg, persist, nil, nil)
	if err != nil {
		t.Fatalf("newRouter: %v", err)
	}
	r.backends[0].healthy.Store(true)
	r.backends[1].healthy.Store(true)

	sysText := strings.Repeat("Static Agent System Instructions Block 1234567890.\n", 1500)
	chatReq, _ := json.Marshal(map[string]any{
		"model": "DeepSeek-V4.1-Flash-1017",
		"messages": []map[string]any{
			{"role": "system", "content": sysText},
			{"role": "user", "content": "hello world"},
		},
	})
	tmpl := extractStarterTemplate(chatReq, "opencode/1.0", 512, 100000)
	if tmpl == nil {
		t.Fatalf("expected starter template")
	}

	r.starters.Observe(tmpl)
	r.starters.Observe(tmpl)
	r.starters.entries[tmpl.FamilyHash].NeedsWarm = true
	r.backends[0].activeReqs.Store(1)
	r.backends[1].activeReqs.Store(1)
	res := r.starters.WarmDue(false, "")
	if len(res) != 0 || warmCalls.Load() != 0 {
		t.Fatalf("expected warm skipped when all backends busy")
	}

	r.backends[0].activeReqs.Store(0)
	res = r.starters.WarmDue(false, "")
	if len(res) != 1 || res[0].Backend != "gpu2-200k" || res[0].PromptTokens != 26010 {
		t.Fatalf("unexpected warm result: %+v", res)
	}

	an, ok := r.analyzeChatBody(chatReq)
	if !ok {
		t.Fatalf("analyzeChatBody failed")
	}
	if an.CachedEst != 26000 || an.MissEst >= 24000 {
		t.Fatalf("expected warmed prefix recognized: CachedEst=%d MissEst=%d", an.CachedEst, an.MissEst)
	}
	b, reason, isHeavy := r.selectBackend(an, nil)
	if b.config().ID != "gpu2-200k" || isHeavy {
		t.Fatalf("expected brand-new session with warmed 26K starter to route to tier1 gpu2-200k (not heavy-miss), got %s reason=%s heavy=%v", b.config().ID, reason, isHeavy)
	}
}
