package tiered

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
)

func boolPtr(v bool) *bool {
	return &v
}

func makeCompTestBody(sys string, turns []map[string]any) []byte {
	msgs := make([]map[string]any, 0, 1+len(turns))
	if sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	msgs = append(msgs, turns...)
	b, _ := marshalNoEscapeHTML(map[string]any{
		"model":    "m1",
		"messages": msgs,
	})
	return b
}

func testCompressRouter(t *testing.T, url, persistPath string, cc ContextCompressConfig) *Router {
	t.Helper()
	f := false
	cfg := RouterConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      &f,
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		ContextCompress:    &cc,
		Backends: []BackendConfig{
			{ID: "b1", URL: url, Tier: 1, MaxContextTokens: 200000, MaxSafeInput: 180000, PromoteAt: 100000, SpeedScore: 100},
		},
	}
	r, err := newRouterWithTarget("p", "local", "", cfg, persistPath, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newRouterWithTarget: %v", err)
	}
	t.Cleanup(r.close)
	for _, b := range r.getBackends() {
		b.healthy.Store(true)
	}
	r.compressor.deferAfterLiveDelay.Store(-1)
	r.compressor.idlePollInterval.Store(int64(15 * time.Millisecond))
	return r
}

func TestStage1WeightedPruningAndFrozenCheckpointResilience(t *testing.T) {
	persistPath := filepath.Join(t.TempDir(), "checkpoints.json")
	cc := ContextCompressConfig{
		Enabled:              true,
		Stage2Enabled:        boolPtr(false),
		TriggerTokens:        3500,
		Stage1TargetTokens:   1200,
		KeepRecentGroups:     2,
		KeepRecentMinTokens:  40,
		KeepRecentMaxTokens:  500,
		StaleReadMaxChars:    80,
		ToolMaxChars:         160,
		MutationToolMaxChars: 240,
		AssistantMaxChars:    200,
		UserPasteMaxChars:    200,
		PersistPath:          persistPath,
	}
	r := testCompressRouter(t, "http://127.0.0.1:18081", persistPath, cc)
	normCfg := r.compressor.getCfg()

	sysPrompt := "System contract: obey every workspace boundary."
	staleBody := strings.Repeat("STALE_READ_CONTENT_SHOULD_COLLAPSE ", 180)
	freshReadBody := strings.Repeat("LATEST_READ_RESULT_KEEP ", 120)
	errBody := "FAIL error exit code 1: compilation broken\n" + strings.Repeat("stack line\n", 180)
	mutationBody := "Successfully wrote patch\n" + strings.Repeat("diff hunk ", 160)
	verboseAssistant := strings.Repeat("Verbose explanation detail. ", 220)

	turns := []map[string]any{
		{"role": "user", "content": "CRITICAL RULE: NEVER modify production database migrations."},
		{"role": "assistant", "content": verboseAssistant, "tool_calls": []map[string]any{
			{"id": "call_read_1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"/srv/app.go"}`}},
		}},
		{"role": "tool", "tool_call_id": "call_read_1", "content": staleBody},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
			{"id": "call_err", "type": "function", "function": map[string]any{"name": "run_command", "arguments": `{"command":"go test"}`}},
		}},
		{"role": "tool", "tool_call_id": "call_err", "content": errBody},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
			{"id": "call_edit", "type": "function", "function": map[string]any{"name": "edit_file", "arguments": `{"path":"/srv/handler.go"}`}},
		}},
		{"role": "tool", "tool_call_id": "call_edit", "content": mutationBody},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
			{"id": "call_read_2", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"/srv/app.go"}`}},
		}},
		{"role": "tool", "tool_call_id": "call_read_2", "content": freshReadBody},
		{"role": "user", "content": "Now summarize the remaining work."},
	}

	body1 := makeCompTestBody(sysPrompt, turns)
	an1, ok := r.analyzeChatBody(body1)
	if !ok || an1.TotalEst < normCfg.TriggerTokens {
		t.Fatalf("expected request >= trigger (%d), got ok=%v est=%d", normCfg.TriggerTokens, ok, an1.TotalEst)
	}

	res1 := r.compressor.MaybeCompress(body1, an1, nil, normCfg, 24000, "local", false)
	if !res1.Compressed || res1.Mode != "stage1" || res1.CheckpointID == "" {
		t.Fatalf("expected stage1 compression, got %+v", res1)
	}
	if res1.CompressedTokens >= res1.OriginalTokens {
		t.Fatalf("expected compressed tokens < original (%d -> %d)", res1.OriginalTokens, res1.CompressedTokens)
	}

	outStr := string(res1.Body)
	if !strings.Contains(outStr, "NEVER modify production database migrations") {
		t.Fatalf("expected constraint-bearing P1 user prompt preserved")
	}
	if !strings.Contains(outStr, "[旧只读快照已过期") || strings.Contains(outStr, strings.Repeat("STALE_READ_CONTENT_SHOULD_COLLAPSE ", 20)) {
		t.Fatalf("expected stale earlier read_file to collapse")
	}
	if !strings.Contains(outStr, "LATEST_READ_RESULT_KEEP") {
		t.Fatalf("expected latest non-stale tail read_file kept intact")
	}
	if !strings.Contains(outStr, "FAIL error exit code 1") {
		t.Fatalf("expected error tool signal preserved")
	}

	// Subsequent turn appends only a tail user reply and must reuse the EXACT same frozen checkpoint.
	turns2 := append(append([]map[string]any{}, turns...),
		map[string]any{"role": "assistant", "content": "Done."},
		map[string]any{"role": "user", "content": "Proceed to next step."},
	)
	body2 := makeCompTestBody(sysPrompt, turns2)
	an2, _ := r.analyzeChatBody(body2)
	res2 := r.compressor.MaybeCompress(body2, an2, nil, normCfg, 24000, "local", false)
	if !res2.Compressed || res2.Mode != "stage1" || res2.CheckpointID != res1.CheckpointID {
		t.Fatalf("expected same frozen checkpoint %q, got %+v", res1.CheckpointID, res2)
	}

	// Verify frozen prefix stability: compressed messages up to -2 match res1.
	var p1, p2 struct {
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal(res1.Body, &p1)
	_ = json.Unmarshal(res2.Body, &p2)
	if len(p2.Messages) != len(p1.Messages)+2 {
		t.Fatalf("expected +2 tail messages over frozen prefix, got %d vs %d", len(p2.Messages), len(p1.Messages))
	}
	for i := range p1.Messages {
		if !bytes.Equal(compactJSONBytes(p1.Messages[i]), compactJSONBytes(p2.Messages[i])) {
			t.Fatalf("frozen prefix diverged at msg %d:\n  want: %s\n  got:  %s", i, p1.Messages[i], p2.Messages[i])
		}
	}

	// Bypass header must skip compression completely.
	bypassHdr := http.Header{"X-No-Compress": []string{"1"}}
	resBypass := r.compressor.MaybeCompress(body2, an2, bypassHdr, normCfg, 24000, "local", false)
	if resBypass.Compressed || resBypass.Mode != "none" || resBypass.SkipReason != "bypass-header" {
		t.Fatalf("expected bypass-header skip, got %+v", resBypass)
	}
}

func TestStage2AsyncSummarizationAndPrewarm(t *testing.T) {
	var sumPromptsMu sync.Mutex
	var sumPrompts []string
	var prewarmCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		bodyStr := string(raw)

		if r.Header.Get("User-Agent") == "dsv41-smart-router-prewarm/1.0" {
			prewarmCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"pw","choices":[{"index":0,"message":{"role":"assistant","content":"."},"finish_reason":"stop"}],"usage":{"prompt_tokens":320,"completion_tokens":1,"total_tokens":321,"prompt_tokens_details":{"cached_tokens":300}}}`))
			return
		}

		if strings.Contains(bodyStr, "【服务端上下文压缩指令】") {
			sumPromptsMu.Lock()
			idx := len(sumPrompts) + 1
			sumPrompts = append(sumPrompts, bodyStr)
			sumPromptsMu.Unlock()

			summary := fmt.Sprintf(
				"## 1. 用户目标、硬约束与最新纠偏\n- HARD CONSTRAINT GEN %d: NEVER DROP PROD DB.\n\n## 2. 已落地的事实变更与环境状态\n- Updated `/srv/app.go` cleanly.\n\n## 3. 未完成待办\n- Continue remaining tests.\n\n## 4. 关键结论\n- Verified token reduction.\n\n## 5. 核心实体索引\n- `context_compress.go`: checkpoint engine.",
				idx,
			)
			respJSON, _ := json.Marshal(map[string]any{
				"id": "sum",
				"choices": []map[string]any{{
					"index":         0,
					"message":       map[string]string{"role": "assistant", "content": summary},
					"finish_reason": "stop",
				}},
				"usage": map[string]int{"prompt_tokens": 400, "completion_tokens": 90, "total_tokens": 490},
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(respJSON)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"live","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	persistPath := filepath.Join(t.TempDir(), "checkpoints.json")
	cc := ContextCompressConfig{
		Enabled:                true,
		Stage2Enabled:          boolPtr(true),
		Stage2Prewarm:          boolPtr(true),
		Stage2MaxSummaryTokens: 512,
		TriggerTokens:          1800,
		Stage1TargetTokens:     900,
		KeepRecentGroups:       2,
		KeepRecentMinTokens:    30,
		KeepRecentMaxTokens:    240,
		ToolMaxChars:           120,
		AssistantMaxChars:      160,
		UserPasteMaxChars:      160,
		PersistPath:            persistPath,
	}
	r := testCompressRouter(t, srv.URL, persistPath, cc)
	r.ensureLoops()
	normCfg := r.compressor.getCfg()

	turns := []map[string]any{
		{"role": "user", "content": "HARD CONSTRAINT: NEVER DROP PROD DB."},
		{"role": "assistant", "content": strings.Repeat("Detailed design analysis block alpha. ", 90)},
		{"role": "user", "content": strings.Repeat("Requirements detail block beta. ", 80)},
		{"role": "assistant", "content": strings.Repeat("Implementation detail block gamma. ", 80)},
		{"role": "user", "content": "Current short turn question."},
	}
	body1 := makeCompTestBody("System base.", turns)
	an1, _ := r.analyzeChatBody(body1)
	res1 := r.compressor.MaybeCompress(body1, an1, nil, normCfg, 24000, "local", false)
	if !res1.Compressed || res1.Mode != "stage1" || res1.PendingJob == nil {
		t.Fatalf("expected stage1 with pending stage2 job, got %+v", res1)
	}
	r.compressor.ScheduleStage2(res1.PendingJob)

	deadline := time.Now().Add(4 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		list := r.compressor.List()
		if len(list) == 1 && list[0].Stage == 2 && list[0].Stage2Status == "ready" {
			ready = true
			if list[0].PrewarmCached != 300 {
				t.Fatalf("expected prewarm_cached=300, got %+v", list[0])
			}
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !ready || prewarmCalls.Load() < 1 {
		t.Fatalf("expected checkpoint to reach stage2 ready & prewarmed, list=%+v prewarm=%d", r.compressor.List(), prewarmCalls.Load())
	}

	// Follow-up turn uses Stage 2 compressed history.
	turns2 := append(append([]map[string]any{}, turns...),
		map[string]any{"role": "assistant", "content": "Acknowledged."},
		map[string]any{"role": "user", "content": "Next small step."},
	)
	body2 := makeCompTestBody("System base.", turns2)
	an2, _ := r.analyzeChatBody(body2)
	res2 := r.compressor.MaybeCompress(body2, an2, nil, normCfg, 24000, "local", false)
	if !res2.Compressed || res2.Mode != "stage2" || !strings.Contains(string(res2.Body), summaryHeaderMarker) {
		t.Fatalf("expected stage2 mode with summary marker, got mode=%s ok=%v", res2.Mode, res2.Compressed)
	}

	// Generation N+1: grow tail after CP1 past trigger_tokens -> inherits CP1 HardConstraints.
	gen2Turns := append([]map[string]any{}, turns2...)
	for i := 0; i < 4; i++ {
		gen2Turns = append(gen2Turns,
			map[string]any{"role": "assistant", "content": strings.Repeat("Gen2 large step output. ", 90)},
			map[string]any{"role": "user", "content": fmt.Sprintf("Gen2 user follow-up %d with more context.", i)},
		)
	}
	body3 := makeCompTestBody("System base.", gen2Turns)
	an3, _ := r.analyzeChatBody(body3)
	res3 := r.compressor.MaybeCompress(body3, an3, nil, normCfg, 24000, "local", false)
	if !res3.Compressed || res3.CheckpointID == "" || res3.CheckpointID == res1.CheckpointID || res3.PendingJob == nil {
		t.Fatalf("expected generation 2 re-compression, got %+v", res3)
	}
	r.compressor.ScheduleStage2(res3.PendingJob)

	deadline = time.Now().Add(4 * time.Second)
	gen2Ready := false
	for time.Now().Before(deadline) {
		for _, v := range r.compressor.List() {
			if v.ID == res3.CheckpointID && v.Generation == 2 && v.Stage2Status == "ready" {
				gen2Ready = true
				break
			}
		}
		if gen2Ready {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !gen2Ready {
		t.Fatalf("expected Generation 2 checkpoint ready, got %+v", r.compressor.List())
	}

	sumPromptsMu.Lock()
	defer sumPromptsMu.Unlock()
	if len(sumPrompts) < 2 {
		t.Fatalf("expected 2 stage2 summarizer prompts, got %d", len(sumPrompts))
	}
	if !strings.Contains(sumPrompts[1], "【跨代继承硬约束】") || !strings.Contains(sumPrompts[1], "NEVER DROP PROD DB") {
		t.Fatalf("expected Generation 2 summarizer prompt to inherit HardConstraints from Gen 1, got:\n%s", sumPrompts[1])
	}
}

func TestSingleTurnGiantPromptSkipped(t *testing.T) {
	persistPath := filepath.Join(t.TempDir(), "checkpoints.json")
	cc := ContextCompressConfig{
		Enabled:            true,
		TriggerTokens:      500,
		Stage1TargetTokens: 200,
		PersistPath:        persistPath,
	}
	r := testCompressRouter(t, "http://127.0.0.1:18081", persistPath, cc)
	normCfg := r.compressor.getCfg()

	body := makeCompTestBody("Sys", []map[string]any{
		{"role": "user", "content": strings.Repeat("Giant single turn prompt text. ", 250)},
	})
	an, ok := r.analyzeChatBody(body)
	if !ok || an.TotalEst < 500 {
		t.Fatalf("expected giant request >=500 tokens, got ok=%v est=%d", ok, an.TotalEst)
	}
	res := r.compressor.MaybeCompress(body, an, nil, normCfg, 24000, "local", false)
	if res.Compressed || res.Mode != "none" || res.SkipReason != "too-few-turns" {
		t.Fatalf("expected single-turn prompt skipped with too-few-turns, got %+v", res)
	}
}

func TestWarmPassthroughGuardsDoublePrefill(t *testing.T) {
	persistPath := filepath.Join(t.TempDir(), "checkpoints.json")
	cc := ContextCompressConfig{
		Enabled:                   true,
		Stage2Enabled:             boolPtr(true),
		TriggerTokens:             1200,
		Stage1TargetTokens:        600,
		KeepRecentGroups:          2,
		KeepRecentMinTokens:       20,
		KeepRecentMaxTokens:       150,
		Stage2WarmStage1MaxTokens: 100, // Force warm passthrough instead of mutating prefix immediately
		Stage2WarmStage1MinDrop:   90,
		PersistPath:               persistPath,
	}
	r := testCompressRouter(t, "http://127.0.0.1:18081", persistPath, cc)
	normCfg := r.compressor.getCfg()

	body := makeCompTestBody("Sys", []map[string]any{
		{"role": "user", "content": "Turn 1: " + strings.Repeat("warm prefix block ", 160)},
		{"role": "assistant", "content": "Reply 1: " + strings.Repeat("warm answer block ", 160)},
		{"role": "user", "content": "Turn 2: " + strings.Repeat("warm prompt block ", 120)},
		{"role": "assistant", "content": "Reply 2."},
		{"role": "user", "content": "Latest short question."},
	})
	an, ok := r.analyzeChatBody(body)
	if !ok || an.TotalEst < normCfg.TriggerTokens {
		t.Fatalf("expected warm request >= %d tokens, got ok=%v est=%d", normCfg.TriggerTokens, ok, an.TotalEst)
	}
	// Simulate warm cache hit (>=50% cached, low miss < 2*heavyMissThreshold).
	an.CachedEst = an.TotalEst * 80 / 100
	an.MissEst = an.TotalEst - an.CachedEst

	res := r.compressor.MaybeCompress(body, an, nil, normCfg, 24000, "local", false)
	if res.Compressed || res.Mode != "none" {
		t.Fatalf("expected warm passthrough not to mutate body on live request, got %+v", res)
	}
	if res.CheckpointID == "" || res.Stage2Status != "pending" || res.PendingJob == nil {
		t.Fatalf("expected Stage 0 checkpoint queued for Stage 2 background work, got %+v", res)
	}
	if !bytes.Equal(res.Body, body) {
		t.Fatalf("expected warm passthrough to preserve exact request bytes")
	}
}

func TestConfidentialCheckpointIsolation(t *testing.T) {
	var externalCalls atomic.Int32
	extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer extSrv.Close()

	// Part 1: confidential checkpoint on loopback backend stays memory-only (never written to disk, summary redacted).
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		summary := "## 1. 用户目标\n- TOP_SECRET_CONFIDENTIAL_LINE\n\n## 2. 状态\n- Memory only." + strings.Repeat(" padding", 12)
		b, _ := json.Marshal(map[string]any{
			"id":      "s",
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": summary}, "finish_reason": "stop"}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer localSrv.Close()

	dir := t.TempDir()
	persistPath := filepath.Join(dir, "checkpoints.json")
	cc := ContextCompressConfig{
		Enabled:                true,
		Stage2Enabled:          boolPtr(true),
		Stage2Prewarm:          boolPtr(false),
		Stage2MaxSummaryTokens: 256,
		TriggerTokens:          1400,
		Stage1TargetTokens:     600,
		KeepRecentGroups:       2,
		KeepRecentMinTokens:    20,
		KeepRecentMaxTokens:    160,
		ToolMaxChars:           120,
		AssistantMaxChars:      120,
		UserPasteMaxChars:      120,
		PersistPath:            persistPath,
	}
	r := testCompressRouter(t, localSrv.URL, persistPath, cc)
	r.ensureLoops()
	normCfg := r.compressor.getCfg()

	turns := []map[string]any{
		{"role": "user", "content": "Confidential plan note: TOP_SECRET_CONFIDENTIAL_LINE"},
		{"role": "assistant", "content": strings.Repeat("Secret analysis detail ", 220)},
		{"role": "user", "content": strings.Repeat("Sensitive requirements text ", 200)},
		{"role": "assistant", "content": "Understood."},
		{"role": "user", "content": "Next question."},
	}
	body := makeCompTestBody("System", turns)
	an, _ := r.analyzeChatBody(body)

	res := r.compressor.MaybeCompress(body, an, nil, normCfg, 24000, "local", true)
	if !res.Compressed || res.PendingJob == nil || !res.PendingJob.confidential {
		t.Fatalf("expected confidential stage1 + pending job, got mode=%s skip=%s orig=%d comp=%d pending=%v", res.Mode, res.SkipReason, res.OriginalTokens, res.CompressedTokens, res.PendingJob != nil)
	}
	r.compressor.ScheduleStage2(res.PendingJob)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		list := r.compressor.List()
		if len(list) == 1 && list[0].Stage == 2 {
			if !list[0].Confidential || list[0].SummaryPreview != "" {
				t.Fatalf("expected confidential checkpoint with redacted summary_preview, got %+v", list[0])
			}
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	r.compressor.gcAndSave()
	r.compressor.saveToDisk()
	if _, err := os.Stat(persistPath); !errors.Is(err, os.ErrNotExist) {
		data, _ := os.ReadFile(persistPath)
		t.Fatalf("confidential checkpoint must NEVER persist to disk, got err=%v data=%s", err, data)
	}

	// Part 2: confidential checkpoint must refuse external non-loopback summarizer_url.
	persist2 := filepath.Join(t.TempDir(), "checkpoints.json")
	ccExt := cc
	ccExt.SummarizerURL = "https://api.example.com/v1"
	ccExt.PersistPath = persist2
	r2 := testCompressRouter(t, localSrv.URL, persist2, ccExt)
	r2.ensureLoops()
	res2 := r2.compressor.MaybeCompress(body, an, nil, r2.compressor.getCfg(), 24000, "local", true)
	if res2.PendingJob == nil {
		t.Fatalf("expected pending job on confidential turn")
	}
	r2.compressor.ScheduleStage2(res2.PendingJob)

	deadline = time.Now().Add(3 * time.Second)
	failedRefusal := false
	for time.Now().Before(deadline) {
		list := r2.compressor.List()
		if len(list) == 1 && list[0].Stage2Status == "failed" {
			if !strings.Contains(list[0].Stage2Err, "confidential") {
				t.Fatalf("expected confidential refusal error, got %q", list[0].Stage2Err)
			}
			failedRefusal = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !failedRefusal || externalCalls.Load() != 0 {
		t.Fatalf("expected confidential checkpoint to block external summarizer (failed=%v calls=%d)", failedRefusal, externalCalls.Load())
	}

	// Part 3: isolate confidential checkpoints from non-confidential callers, isolate distinct caller scopes, and scrub disk on upgradeConfidential.
	localSrv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		summary := "## 1. 用户目标\n- TOP_SECRET_CONFIDENTIAL_SYS\n\n## 2. 状态\n- Confidential isolated." + strings.Repeat(" padding", 12)
		b, _ := json.Marshal(map[string]any{
			"id":      "s3",
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": summary}, "finish_reason": "stop"}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer localSrv3.Close()

	persist3 := filepath.Join(t.TempDir(), "checkpoints.json")
	cc3 := cc
	cc3.PersistPath = persist3
	r3 := testCompressRouter(t, localSrv3.URL, persist3, cc3)
	r3.ensureLoops()
	normCfg3 := r3.compressor.getCfg()

	turns3 := []map[string]any{
		{"role": "user", "content": "Shared archived note line"},
		{"role": "assistant", "content": strings.Repeat("Detailed analysis block ", 220)},
		{"role": "user", "content": strings.Repeat("Detailed requirements text ", 200)},
		{"role": "assistant", "content": "Understood."},
		{"role": "user", "content": "Next question."},
	}
	body3Conf := makeCompTestBody("System: TOP_SECRET_CONFIDENTIAL_SYS", turns3)
	an3Conf, _ := r3.analyzeChatBody(body3Conf)
	resConf := r3.compressor.MaybeCompress(body3Conf, an3Conf, nil, normCfg3, 24000, "local", true)
	if !resConf.Compressed || resConf.PendingJob == nil || resConf.CheckpointID == "" {
		t.Fatalf("expected confidential stage1 checkpoint, got %+v", resConf)
	}
	r3.compressor.ScheduleStage2(resConf.PendingJob)

	deadline = time.Now().Add(4 * time.Second)
	confReady := false
	for time.Now().Before(deadline) {
		for _, item := range r3.compressor.List() {
			if item.ID == resConf.CheckpointID && item.Stage2Status == "ready" {
				if !item.Confidential || item.SummaryPreview != "" {
					t.Fatalf("expected confidential checkpoint with redacted summary_preview, got %+v", item)
				}
				confReady = true
				break
			}
		}
		if confReady {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !confReady {
		t.Fatalf("timed out waiting for confidential stage2 ready")
	}

	turns3Follow := append(append([]map[string]any(nil), turns3...),
		map[string]any{"role": "assistant", "content": "Answer."},
		map[string]any{"role": "user", "content": "Follow-up non-confidential turn."},
	)
	body3Pub := makeCompTestBody("Public system", turns3Follow)
	an3Pub, _ := r3.analyzeChatBody(body3Pub)
	resPub := r3.compressor.MaybeCompress(body3Pub, an3Pub, nil, normCfg3, 24000, "local", false)
	if !resPub.Compressed || resPub.CheckpointID == "" || resPub.CheckpointID == resConf.CheckpointID {
		t.Fatalf("expected isolated public checkpoint != %s, got %+v", resConf.CheckpointID, resPub)
	}
	if strings.Contains(string(resPub.Body), "TOP_SECRET_CONFIDENTIAL_SYS") {
		t.Fatalf("non-confidential caller leaked confidential Stage 2 summary: %s", string(resPub.Body))
	}

	// Even with an identical system prompt string, non-confidential must NOT reuse the confidential checkpoint.
	body3PubSameSys := makeCompTestBody("System: TOP_SECRET_CONFIDENTIAL_SYS", turns3Follow)
	an3PubSameSys, _ := r3.analyzeChatBody(body3PubSameSys)
	resPubSameSys := r3.compressor.MaybeCompress(body3PubSameSys, an3PubSameSys, nil, normCfg3, 24000, "local", false)
	if !resPubSameSys.Compressed || resPubSameSys.CheckpointID == resConf.CheckpointID || strings.Contains(string(resPubSameSys.Body), summaryHeaderMarker) {
		t.Fatalf("non-confidential caller must not reuse confidential Stage 2 checkpoint %s, got %+v", resConf.CheckpointID, resPubSameSys)
	}

	// Distinct callerScope with identical non-confidential body creates a distinct checkpoint ID.
	resCallerB := r3.compressor.MaybeCompressWithScope(body3Pub, an3Pub, nil, normCfg3, 24000, "local", false, "caller-B\x00internal")
	if !resCallerB.Compressed || resCallerB.CheckpointID == "" || resCallerB.CheckpointID == resPub.CheckpointID || resCallerB.CheckpointID == resConf.CheckpointID {
		t.Fatalf("expected distinct callerScope checkpoint, got %+v (pub=%s conf=%s)", resCallerB, resPub.CheckpointID, resConf.CheckpointID)
	}

	r3.compressor.gcAndSave()
	r3.compressor.saveToDisk()
	diskPub, err := os.ReadFile(persist3)
	if err != nil {
		t.Fatalf("expected non-confidential checkpoints persisted to disk: %v", err)
	}
	if !strings.Contains(string(diskPub), resPub.CheckpointID) || strings.Contains(string(diskPub), resConf.CheckpointID) || strings.Contains(string(diskPub), "TOP_SECRET_CONFIDENTIAL_SYS") {
		t.Fatalf("disk persistence mismatch: pub=%s conf=%s data=%s", resPub.CheckpointID, resConf.CheckpointID, diskPub)
	}

	// Directly upgrading resPub to confidential must scrub it from disk and redact List().
	r3.compressor.upgradeConfidential(resPub.CheckpointID)
	diskUpgraded, err := os.ReadFile(persist3)
	if err == nil && strings.Contains(string(diskUpgraded), resPub.CheckpointID) {
		t.Fatalf("expected upgraded checkpoint %s scrubbed from disk, got %s", resPub.CheckpointID, diskUpgraded)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected read error after upgradeConfidential: %v", err)
	}
	for _, item := range r3.compressor.List() {
		if item.ID == resPub.CheckpointID {
			if !item.Confidential || item.SummaryPreview != "" {
				t.Fatalf("expected upgraded checkpoint confidential & redacted in List(), got %+v", item)
			}
		}
	}
}

func TestStage2WarmupFilterAndTargetPurge(t *testing.T) {
	var seenSumBodiesMu sync.Mutex
	var seenSumBodies []string
	var seenSumAuth string

	sumSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenSumBodiesMu.Lock()
		seenSumBodies = append(seenSumBodies, string(b))
		seenSumAuth = r.Header.Get("Authorization")
		seenSumBodiesMu.Unlock()
		summary := "## 1. 目标\n- REDACTED OK.\n\n## 2. 状态\n- Verified warmup redaction filter." + strings.Repeat(" more", 10)
		resp, _ := json.Marshal(map[string]any{
			"id":      "sum",
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": summary}, "finish_reason": "stop"}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	defer sumSrv.Close()

	backSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backSrv.Close()

	dir := t.TempDir()
	persistPath := filepath.Join(dir, "checkpoints.json")
	cc := ContextCompressConfig{
		Enabled:                true,
		Stage2Enabled:          boolPtr(true),
		Stage2MaxSummaryTokens: 256,
		TriggerTokens:          1400,
		Stage1TargetTokens:     600,
		KeepRecentGroups:       2,
		KeepRecentMinTokens:    20,
		KeepRecentMaxTokens:    160,
		AssistantMaxChars:      120,
		SummarizerURL:          sumSrv.URL,
		SummarizerKeyRef:       "env:TEST_SUM_KEY",
		PersistPath:            persistPath,
	}
	r := testCompressRouter(t, backSrv.URL, persistPath, cc)
	r.setKeyResolver(func(ref string) (string, error) {
		if ref == "env:TEST_SUM_KEY" {
			return "sk-resolved-summarizer", nil
		}
		return "", errors.New("not found")
	})

	var sawFilterURL atomic.Value
	r.setWarmupFilter(func(ctx context.Context, provider, backendURL string, payload []byte) ([]byte, error) {
		sawFilterURL.Store(backendURL)
		if bytes.Contains(payload, []byte("BLOCK_TRIGGER")) {
			return nil, errors.New("sensitive data blocked")
		}
		return bytes.ReplaceAll(payload, []byte("SECRET_KEY_PLAINTEXT"), []byte("[REDACTED]")), nil
	})
	r.ensureLoops()

	turns := []map[string]any{
		{"role": "user", "content": "Credential check SECRET_KEY_PLAINTEXT in config"},
		{"role": "assistant", "content": strings.Repeat("Long response block alpha. ", 90)},
		{"role": "user", "content": strings.Repeat("Long user context block beta. ", 80)},
		{"role": "assistant", "content": "Ok."},
		{"role": "user", "content": "Continue."},
	}
	body := makeCompTestBody("System", turns)
	an, _ := r.analyzeChatBody(body)
	res := r.compressor.MaybeCompress(body, an, nil, r.compressor.getCfg(), 24000, "local", false)
	if !res.Compressed || res.PendingJob == nil {
		t.Fatalf("expected stage1 checkpoint + stage2 job, got %+v", res)
	}
	r.compressor.ScheduleStage2(res.PendingJob)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if list := r.compressor.List(); len(list) == 1 && list[0].Stage2Status == "ready" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	seenSumBodiesMu.Lock()
	gotBodies := append([]string(nil), seenSumBodies...)
	gotAuth := seenSumAuth
	seenSumBodiesMu.Unlock()
	if len(gotBodies) != 1 || strings.Contains(gotBodies[0], "SECRET_KEY_PLAINTEXT") || !strings.Contains(gotBodies[0], "[REDACTED]") {
		t.Fatalf("expected summarizer payload to be redacted by warmupFilter, got %v", gotBodies)
	}
	if gotAuth != "Bearer sk-resolved-summarizer" {
		t.Fatalf("expected SummarizerKeyRef auth Bearer sk-resolved-summarizer, got %q", gotAuth)
	}
	if u, _ := sawFilterURL.Load().(string); u != sumSrv.URL {
		t.Fatalf("expected warmupFilter to inspect summarizer URL %q, got %q", sumSrv.URL, u)
	}

	// Verify local -> external target reload purges local checkpoints from memory and disk.
	r.compressor.mu.Lock()
	r.compressor.markDirtyLocked()
	r.compressor.mu.Unlock()
	r.compressor.saveToDisk()
	if _, err := os.Stat(persistPath); err != nil {
		t.Fatalf("expected local checkpoint file persisted before target reload: %v", err)
	}

	extCfg := r.getConfig()
	if err := r.apply("external", "", extCfg); err != nil {
		t.Fatalf("r.apply external: %v", err)
	}
	if rem := r.compressor.List(); len(rem) != 0 {
		t.Fatalf("expected local checkpoints purged on local->external reload, got %+v", rem)
	}
	diskAfter, err := os.ReadFile(persistPath)
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	if strings.Contains(string(diskAfter), res.CheckpointID) {
		t.Fatalf("expected purged checkpoint removed from disk, got %s", diskAfter)
	}

	// Verify warmupFilter block aborts Stage 2 safely.
	turnsBlock := append([]map[string]any{}, turns...)
	turnsBlock[0] = map[string]any{"role": "user", "content": "Secret BLOCK_TRIGGER payload"}
	bodyBlk := makeCompTestBody("System", turnsBlock)
	anBlk, _ := r.analyzeChatBody(bodyBlk)
	resBlk := r.compressor.MaybeCompress(bodyBlk, anBlk, nil, r.compressor.getCfg(), 24000, "external", false)
	if resBlk.PendingJob == nil {
		t.Fatalf("expected pending job")
	}
	r.compressor.ScheduleStage2(resBlk.PendingJob)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if list := r.compressor.List(); len(list) == 1 && list[0].Stage2Status == "failed" {
			if !strings.Contains(list[0].Stage2Err, "blocked by redaction policy") {
				t.Fatalf("expected redaction policy block error, got %q", list[0].Stage2Err)
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("expected warmupFilter block to mark checkpoint Stage2Status=failed, got %+v", r.compressor.List())
}

func TestShadowModeDoesNotCompressUpstream(t *testing.T) {
	var seenBytes []byte
	shadowUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBytes, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sh","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer shadowUp.Close()

	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer b1.Close()

	hub := NewHub(t.TempDir(), nil)
	defer func() { _ = hub.Close() }()

	f := false
	tc := config.TieredConfig{
		Mode:               "shadow",
		HeavyMissThreshold: 24000,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      &f,
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		ContextCompress: &ContextCompressConfig{
			Enabled:             true,
			Stage2Enabled:       boolPtr(false),
			TriggerTokens:       500,
			Stage1TargetTokens:  200,
			KeepRecentGroups:    2,
			KeepRecentMinTokens: 20,
			KeepRecentMaxTokens: 120,
			AssistantMaxChars:   80,
		},
		Backends: []config.TieredBackend{
			{ID: "b1", URL: b1.URL, Tier: 1, MaxContextTokens: 64000, MaxSafeInput: 32000, PromoteAt: 16000, SpeedScore: 100},
		},
	}

	upstreamRT := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL, _ = req.URL.Parse(shadowUp.URL + req.URL.Path)
		return http.DefaultTransport.RoundTrip(clone)
	})
	tr, err := hub.Transport("p", "local", &tc, upstreamRT)
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	hub.Activate(&config.Config{Providers: []config.Provider{{Name: "p", Target: "local", Tiered: &tc}}})

	origReqBody := makeCompTestBody("Sys", []map[string]any{
		{"role": "user", "content": "turn 1"},
		{"role": "assistant", "content": strings.Repeat("Shadow mode huge output ", 120)},
		{"role": "user", "content": "turn 2"},
		{"role": "assistant", "content": "ok"},
		{"role": "user", "content": "turn 3"},
	})
	req, _ := http.NewRequest(http.MethodPost, "http://shadow.invalid/v1/chat/completions", bytes.NewReader(origReqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if !bytes.Equal(seenBytes, origReqBody) {
		t.Fatalf("shadow mode mutated upstream request bytes (orig=%d seen=%d)", len(origReqBody), len(seenBytes))
	}
	if r, ok := hub.Router("p"); ok && len(r.compressor.List()) != 0 {
		t.Fatalf("shadow mode must not create compression checkpoints, got %+v", r.compressor.List())
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestContextCompressCodexFindingsRegression(t *testing.T) {
	// 1 & 3: Orphaned pending Stage 2 retry + response_format excluded from cluster summarizer while kept in prewarm.
	var mu sync.Mutex
	var clusterSumBodies []string
	var prewarmBodies []string

	clusterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		s := string(raw)
		if r.Header.Get("User-Agent") == "dsv41-smart-router-prewarm/1.0" {
			mu.Lock()
			prewarmBodies = append(prewarmBodies, s)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"pw","choices":[{"index":0,"message":{"role":"assistant","content":"."},"finish_reason":"stop"}],"usage":{"prompt_tokens":200,"prompt_tokens_details":{"cached_tokens":180}}}`))
			return
		}
		if strings.Contains(s, "【服务端上下文压缩指令】") {
			mu.Lock()
			clusterSumBodies = append(clusterSumBodies, s)
			mu.Unlock()
			summary := "## 1. 用户目标\n- NO RESPONSE FORMAT LEAK.\n\n## 2. 已落地的事实变更\n- Applied cluster summary cleanly with tool metadata." + strings.Repeat(" ok", 10)
			b, _ := json.Marshal(map[string]any{
				"id":      "csum",
				"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": summary}, "finish_reason": "stop"}},
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"live","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer clusterSrv.Close()

	ccCluster := ContextCompressConfig{
		Enabled:                true,
		Stage2Enabled:          boolPtr(true),
		Stage2Prewarm:          boolPtr(true),
		Stage2MaxSummaryTokens: 256,
		TriggerTokens:          1400,
		Stage1TargetTokens:     600,
		KeepRecentGroups:       2,
		KeepRecentMinTokens:    20,
		KeepRecentMaxTokens:    160,
		AssistantMaxChars:      120,
		UserPasteMaxChars:      120,
	}
	rCluster := testCompressRouter(t, clusterSrv.URL, filepath.Join(t.TempDir(), "cp.json"), ccCluster)
	rCluster.ensureLoops()
	normCluster := rCluster.compressor.getCfg()

	turns := []map[string]any{
		{"role": "user", "content": "Rule: respond carefully."},
		{"role": "assistant", "content": strings.Repeat("Verbose cluster analysis text block. ", 90)},
		{"role": "user", "content": strings.Repeat("Detailed context block beta. ", 80)},
		{"role": "assistant", "content": "Ok."},
		{"role": "user", "content": "Next?"},
	}
	msgsWithSys := append([]map[string]any{{"role": "system", "content": "Sys."}}, turns...)
	reqObj := map[string]any{
		"model":           "m1",
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "strict_out"}},
		"messages":        msgsWithSys,
	}
	bodyCluster1, _ := marshalNoEscapeHTML(reqObj)
	anC1, _ := rCluster.analyzeChatBody(bodyCluster1)

	// Abort before calling ScheduleStage2 -> stored checkpoint must stay none, NOT permanently stuck pending.
	resC1 := rCluster.compressor.MaybeCompress(bodyCluster1, anC1, nil, normCluster, 24000, "local", false)
	if !resC1.Compressed || resC1.PendingJob == nil {
		t.Fatalf("expected stage1 + PendingJob, got mode=%s skip=%s pending=%v", resC1.Mode, resC1.SkipReason, resC1.PendingJob != nil)
	}
	rCluster.compressor.mu.RLock()
	storedStatus1 := rCluster.compressor.checkpoints[resC1.CheckpointID].Stage2Status
	rCluster.compressor.mu.RUnlock()
	if storedStatus1 != "none" {
		t.Fatalf("expected unscheduled checkpoint to remain none before ScheduleStage2, got %q", storedStatus1)
	}

	// Next live hit (same prefix + new user turn) recreates PendingJob even if checkpoint status were orphaned "pending".
	turns2 := append(append([]map[string]any{}, turns...),
		map[string]any{"role": "assistant", "content": "Reply."},
		map[string]any{"role": "user", "content": "Follow up."},
	)
	reqObj2 := map[string]any{
		"model":           "m1",
		"response_format": reqObj["response_format"],
		"messages":        append([]map[string]any{{"role": "system", "content": "Sys."}}, turns2...),
	}
	bodyCluster2, _ := marshalNoEscapeHTML(reqObj2)
	anC2, _ := rCluster.analyzeChatBody(bodyCluster2)

	rCluster.compressor.mu.Lock()
	rCluster.compressor.checkpoints[resC1.CheckpointID].Stage2Status = "pending"
	rCluster.compressor.mu.Unlock()

	resC2 := rCluster.compressor.MaybeCompress(bodyCluster2, anC2, nil, normCluster, 24000, "local", false)
	if resC2.PendingJob == nil {
		t.Fatalf("expected orphaned pending checkpoint with no in-flight session job to recreate PendingJob")
	}
	rCluster.compressor.ScheduleStage2(resC2.PendingJob)

	deadline := time.Now().Add(4 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		if list := rCluster.compressor.List(); len(list) == 1 && list[0].Stage == 2 && list[0].Stage2Status == "ready" {
			ready = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("expected rescheduled checkpoint to reach Stage 2 ready, got %+v", rCluster.compressor.List())
	}

	mu.Lock()
	gotSum := append([]string(nil), clusterSumBodies...)
	gotPW := append([]string(nil), prewarmBodies...)
	mu.Unlock()
	if len(gotSum) != 1 || strings.Contains(gotSum[0], `"response_format"`) {
		t.Fatalf("expected cluster summarizer to omit response_format, got %v", gotSum)
	}
	if len(gotPW) != 1 || !strings.Contains(gotPW[0], `"response_format"`) {
		t.Fatalf("expected prewarm request to keep response_format, got %v", gotPW)
	}

	// 2 & 4: External summarizer includes null-content tool_calls+args, and local checkpoint restores redaction placeholders while external target keeps them masked.
	var extSumMu sync.Mutex
	var extSumPayloads []string
	extSumSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		reqStr := string(raw)
		extSumMu.Lock()
		extSumPayloads = append(extSumPayloads, reqStr)
		extSumMu.Unlock()

		placeholder := ""
		if idx := strings.Index(reqStr, "{{"); idx >= 0 {
			if end := strings.Index(reqStr[idx:], "}}"); end > 2 {
				placeholder = reqStr[idx : idx+end+2]
			}
		}
		summary := fmt.Sprintf(
			"## 1. 用户目标\n- Project tag: %s\n\n## 2. 已落地的事实变更\n- Executed edit_file on `/srv/critical_patch.go` cleanly."+"\n\n## 3. 待办\n- Verify restore isolation.",
			placeholder,
		)
		b, _ := json.Marshal(map[string]any{
			"id":      "xsum",
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": summary}, "finish_reason": "stop"}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer extSumSrv.Close()

	redEng, err := redact.New(config.Redaction{Terms: []string{"SECRET_PROJECT_ALPHA"}}, nil, nil)
	if err != nil {
		t.Fatalf("redact.New: %v", err)
	}
	userKey := []byte("0123456789abcdef0123456789abcdef")

	toolTurns := []map[string]any{
		{"role": "user", "content": "Inspect SECRET_PROJECT_ALPHA repository now."},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
			{"id": "call_edit_1", "type": "function", "function": map[string]any{"name": "edit_file", "arguments": `{"path":"/srv/critical_patch.go","line":42}`}},
		}},
		{"role": "tool", "tool_call_id": "call_edit_1", "content": "Successfully wrote patch."},
		{"role": "assistant", "content": strings.Repeat("Detailed implementation audit note for external summarizer. ", 85)},
		{"role": "user", "content": strings.Repeat("Additional context text for threshold crossing. ", 80)},
		{"role": "assistant", "content": "Done."},
		{"role": "user", "content": "Next turn."},
	}
	toolBody := makeCompTestBody("System.", toolTurns)

	wireRedaction := func(r *Router, provName string) {
		r.setWarmupFilter(func(ctx context.Context, _ string, _ string, payload []byte) ([]byte, error) {
			obj, derr := oai.Decode(payload)
			if derr != nil {
				return nil, derr
			}
			rr, rerr := redEng.Apply(ctx, obj, redact.ModeMask, userKey, "tiered-warmup:"+provName)
			if rerr != nil {
				return nil, rerr
			}
			if !rr.Changed {
				return payload, nil
			}
			return oai.Encode(obj), nil
		})
		r.setWarmupRestore(func(_ string, text string) string {
			bucket := redEng.Vault.BucketWithTTL("tiered-warmup:"+provName, redEng.VaultTTL())
			out, _ := redact.RestoreText(text, bucket, nil)
			return out
		})
	}

	ccExtSum := ccCluster
	ccExtSum.SummarizerURL = extSumSrv.URL
	rLocal := testCompressRouter(t, clusterSrv.URL, filepath.Join(t.TempDir(), "cp-local.json"), ccExtSum)
	wireRedaction(rLocal, "p-local")
	rLocal.ensureLoops()

	anL, _ := rLocal.analyzeChatBody(toolBody)
	resL1 := rLocal.compressor.MaybeCompress(toolBody, anL, nil, rLocal.compressor.getCfg(), 24000, "local", false)
	if !resL1.Compressed || resL1.PendingJob == nil {
		t.Fatalf("expected local stage1 + job, got %s/%s", resL1.Mode, resL1.SkipReason)
	}
	rLocal.compressor.ScheduleStage2(resL1.PendingJob)

	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if list := rLocal.compressor.List(); len(list) == 1 && list[0].Stage2Status == "ready" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	extSumMu.Lock()
	if len(extSumPayloads) != 1 {
		extSumMu.Unlock()
		t.Fatalf("expected 1 external summarizer request, got %d", len(extSumPayloads))
	}
	firstSumReq := extSumPayloads[0]
	extSumMu.Unlock()

	if !strings.Contains(firstSumReq, "tool_call edit_file(call_edit_1)") || !strings.Contains(firstSumReq, `/srv/critical_patch.go`) || !strings.Contains(firstSumReq, "[tool edit_file (call_edit_1)]") {
		t.Fatalf("expected external summarizer transcript to include tool_calls name/id/args and tool result header, got:\n%s", firstSumReq)
	}
	if strings.Contains(firstSumReq, "SECRET_PROJECT_ALPHA") || !strings.Contains(firstSumReq, "{{") {
		t.Fatalf("expected external summarizer payload redacted with placeholder, got:\n%s", firstSumReq)
	}

	followBody := makeCompTestBody("System.", append(append([]map[string]any{}, toolTurns...),
		map[string]any{"role": "assistant", "content": "Ack."},
		map[string]any{"role": "user", "content": "Continue."},
	))
	anLF, _ := rLocal.analyzeChatBody(followBody)
	resLF := rLocal.compressor.MaybeCompress(followBody, anLF, nil, rLocal.compressor.getCfg(), 24000, "local", false)
	if !resLF.Compressed || resLF.Mode != "stage2" {
		t.Fatalf("expected stage2 hit on local router, got mode=%s skip=%s", resLF.Mode, resLF.SkipReason)
	}
	if !strings.Contains(string(resLF.Body), "SECRET_PROJECT_ALPHA") || strings.Contains(string(resLF.Body), "{{") {
		t.Fatalf("expected local Stage 2 checkpoint to restore original SECRET_PROJECT_ALPHA without leaving placeholders, got:\n%s", string(resLF.Body))
	}

	// Verify external router/origin does NOT restore masked placeholder back into Stage 2 SummaryText.
	cfgExt := RouterConfig{
		Mode:               "active",
		HeavyMissThreshold: 24000,
		HealthIntervalSec:  60,
		ZhThinkAnchor:      boolPtr(false),
		StarterCache:       &config.TieredStarterCache{Enabled: false},
		ContextCompress:    &ccExtSum,
		Backends: []BackendConfig{
			{ID: "b1", URL: clusterSrv.URL, Tier: 1, MaxContextTokens: 200000, MaxSafeInput: 180000, PromoteAt: 100000, SpeedScore: 100},
		},
	}
	rExt, err := newRouterWithTarget("p-ext", "external", "", cfgExt, filepath.Join(t.TempDir(), "cp-ext.json"), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newRouterWithTarget external: %v", err)
	}
	t.Cleanup(rExt.close)
	for _, b := range rExt.getBackends() {
		b.healthy.Store(true)
	}
	rExt.compressor.deferAfterLiveDelay.Store(-1)
	wireRedaction(rExt, "p-ext")
	rExt.ensureLoops()

	resE1 := rExt.compressor.MaybeCompress(toolBody, anL, nil, rExt.compressor.getCfg(), 24000, "external", false)
	if !resE1.Compressed || resE1.PendingJob == nil {
		t.Fatalf("expected external stage1 + job, got %s/%s", resE1.Mode, resE1.SkipReason)
	}
	rExt.compressor.ScheduleStage2(resE1.PendingJob)
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if list := rExt.compressor.List(); len(list) == 1 && list[0].Stage2Status == "ready" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	resEF := rExt.compressor.MaybeCompress(followBody, anLF, nil, rExt.compressor.getCfg(), 24000, "external", false)
	if !resEF.Compressed || resEF.Mode != "stage2" {
		t.Fatalf("expected external stage2 hit, got %s/%s", resEF.Mode, resEF.SkipReason)
	}
	if strings.Contains(string(resEF.Body), "SECRET_PROJECT_ALPHA") || !strings.Contains(string(resEF.Body), "{{") {
		t.Fatalf("expected external Stage 2 checkpoint to keep placeholder masked (not restore local secret), got:\n%s", string(resEF.Body))
	}
}

func TestContextCompressRound3CodexRegression(t *testing.T) {
	// 1. Reject truncated summarizer completions (finish_reason=length) before installing them, keeping Stage 1 usable for retry.
	var returnFull atomic.Bool
	truncSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		fr := "length"
		summary := "## 1. 用户目标\n- Partial truncated output only, cut mid-sentence without sections 2-5." + strings.Repeat(" partial", 12)
		if returnFull.Load() {
			fr = "stop"
			summary = "## 1. 用户目标\n- Keep contract.\n\n## 2. 已落地的事实变更\n- Applied clean summary after retry.\n\n## 3. 未完成待办\n- None." + strings.Repeat(" ok", 8)
		}
		b, _ := json.Marshal(map[string]any{
			"id": "trunc",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": summary},
				"finish_reason": fr,
			}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer truncSrv.Close()

	ccTrunc := ContextCompressConfig{
		Enabled:                true,
		Stage2Enabled:          boolPtr(true),
		Stage2Prewarm:          boolPtr(false),
		Stage2MaxSummaryTokens: 256,
		TriggerTokens:          1400,
		Stage1TargetTokens:     600,
		KeepRecentGroups:       2,
		KeepRecentMinTokens:    20,
		KeepRecentMaxTokens:    160,
		AssistantMaxChars:      120,
		UserPasteMaxChars:      120,
	}
	rTrunc := testCompressRouter(t, truncSrv.URL, filepath.Join(t.TempDir(), "trunc.json"), ccTrunc)
	rTrunc.ensureLoops()
	normTrunc := rTrunc.compressor.getCfg()

	turns := []map[string]any{
		{"role": "user", "content": "Rule: complete summary required."},
		{"role": "assistant", "content": strings.Repeat("Detailed design block alpha. ", 90)},
		{"role": "user", "content": strings.Repeat("Detailed user specification beta. ", 80)},
		{"role": "assistant", "content": "Understood."},
		{"role": "user", "content": "Proceed."},
	}
	body1 := makeCompTestBody("Sys.", turns)
	an1, _ := rTrunc.analyzeChatBody(body1)
	res1 := rTrunc.compressor.MaybeCompress(body1, an1, nil, normTrunc, 24000, "local", false)
	if !res1.Compressed || res1.PendingJob == nil {
		t.Fatalf("expected stage1 + pending job, got %+v", res1)
	}
	rTrunc.compressor.ScheduleStage2(res1.PendingJob)

	deadline := time.Now().Add(4 * time.Second)
	failedTrunc := false
	for time.Now().Before(deadline) {
		list := rTrunc.compressor.List()
		if len(list) == 1 && list[0].Stage2Status == "failed" {
			if !strings.Contains(list[0].Stage2Err, "truncated") || list[0].Stage != 1 {
				t.Fatalf("expected failed stage2 due to truncation while preserving stage=1, got %+v", list[0])
			}
			failedTrunc = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !failedTrunc {
		t.Fatalf("timed out waiting for truncated Stage 2 failure, got %+v", rTrunc.compressor.List())
	}

	// Next live request must still reuse frozen Stage 1 checkpoint.
	body2 := makeCompTestBody("Sys.", append(append([]map[string]any{}, turns...),
		map[string]any{"role": "assistant", "content": "Ack."},
		map[string]any{"role": "user", "content": "Next turn while stage2 retrying."},
	))
	an2, _ := rTrunc.analyzeChatBody(body2)
	res2 := rTrunc.compressor.MaybeCompress(body2, an2, nil, normTrunc, 24000, "local", false)
	if !res2.Compressed || res2.Mode != "stage1" || res2.CheckpointID != res1.CheckpointID {
		t.Fatalf("expected frozen stage1 fallback after truncated stage2, got %+v", res2)
	}

	// Expire retry cooldown, return finish_reason=stop, and verify Stage 2 succeeds.
	rTrunc.compressor.mu.Lock()
	rTrunc.compressor.checkpoints[res1.CheckpointID].Stage2FailedAt = time.Now().Add(-2 * stage2RetryCooldown)
	rTrunc.compressor.mu.Unlock()
	returnFull.Store(true)

	resRetry := rTrunc.compressor.MaybeCompress(body2, an2, nil, normTrunc, 24000, "local", false)
	if resRetry.PendingJob == nil {
		t.Fatalf("expected retry PendingJob after cooldown expired, got %+v", resRetry)
	}
	rTrunc.compressor.ScheduleStage2(resRetry.PendingJob)

	deadline = time.Now().Add(4 * time.Second)
	readyAfterRetry := false
	for time.Now().Before(deadline) {
		list := rTrunc.compressor.List()
		if len(list) == 1 && list[0].Stage == 2 && list[0].Stage2Status == "ready" {
			readyAfterRetry = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !readyAfterRetry {
		t.Fatalf("expected Stage 2 ready after retry with finish_reason=stop, got %+v", rTrunc.compressor.List())
	}

	// 2. Hot-reload local -> external cancels an in-flight waitForIdleBackend Stage 2 job without dispatching to the new external backend.
	rHot := testCompressRouter(t, truncSrv.URL, filepath.Join(t.TempDir(), "hot.json"), ccTrunc)
	rHot.ensureLoops()
	resHot := rHot.compressor.MaybeCompress(body1, an1, nil, rHot.compressor.getCfg(), 24000, "local", false)
	if !resHot.Compressed || resHot.PendingJob == nil {
		t.Fatalf("expected stage1 + pending job on rHot, got %+v", resHot)
	}
	for _, b := range rHot.getBackends() {
		b.activeReqs.Store(1) // Keep waitForIdleBackend waiting until reload cancels it.
	}
	rHot.compressor.ScheduleStage2(resHot.PendingJob)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rHot.compressor.stage2Active.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !rHot.compressor.stage2Active.Load() {
		t.Fatalf("expected Stage 2 job active inside waitForIdleBackend")
	}

	var extChatHits atomic.Int32
	extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		extChatHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ext","choices":[{"index":0,"message":{"role":"assistant","content":"should never run"},"finish_reason":"stop"}]}`))
	}))
	defer extSrv.Close()

	extCfg := rHot.getConfig()
	extCfg.Backends = []BackendConfig{
		{ID: "ext1", URL: extSrv.URL, Tier: 1, MaxContextTokens: 200000, MaxSafeInput: 180000, PromoteAt: 100000, SpeedScore: 100},
	}
	if err := rHot.apply("external", "", extCfg); err != nil {
		t.Fatalf("rHot.apply external: %v", err)
	}

	deadline = time.Now().Add(1 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		if !rHot.compressor.stage2Active.Load() {
			stopped = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !stopped {
		t.Fatalf("expected in-flight local Stage 2 job to abort promptly on local->external reload")
	}
	if extChatHits.Load() != 0 {
		t.Fatalf("aborted local Stage 2 job must NOT dispatch to new external backend (hits=%d)", extChatHits.Load())
	}
	if rem := rHot.compressor.List(); len(rem) != 0 {
		t.Fatalf("expected 0 remaining local checkpoints after reload, got %+v", rem)
	}

	// 3. Serialize concurrent checkpoint snapshot + disk write operations (no .tmp race or resurrected state).
	persistConc := filepath.Join(t.TempDir(), "concurrent-checkpoints.json")
	ccConc := ccTrunc
	ccConc.Stage2Enabled = boolPtr(false)
	ccConc.PersistPath = persistConc
	rConc := testCompressRouter(t, truncSrv.URL, persistConc, ccConc)
	normConc := rConc.compressor.getCfg()

	var wg sync.WaitGroup
	var created atomic.Int32
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for iter := 0; iter < 15; iter++ {
				tTurns := []map[string]any{
					{"role": "user", "content": fmt.Sprintf("Worker %d iter %d rule", idx, iter)},
					{"role": "assistant", "content": strings.Repeat("Long worker content analysis block ", 110)},
					{"role": "user", "content": strings.Repeat("Worker detailed specification block ", 100)},
					{"role": "assistant", "content": "Ack."},
					{"role": "user", "content": "Continue."},
				}
				b := makeCompTestBody("Sys.", tTurns)
				a, _ := rConc.analyzeChatBody(b)
				res := rConc.compressor.MaybeCompress(b, a, nil, normConc, 24000, "local", false)
				if res.CheckpointID != "" {
					created.Add(1)
				}
				rConc.compressor.gcAndSave()
				rConc.compressor.saveToDisk()
				if res.CheckpointID != "" {
					switch iter % 3 {
					case 0:
						rConc.compressor.upgradeConfidential(res.CheckpointID)
					case 1:
						_, _ = rConc.compressor.Delete(res.CheckpointID)
					}
				}
			}
		}(g)
	}
	wg.Wait()
	if created.Load() == 0 {
		t.Fatalf("expected concurrent workers to create checkpoints")
	}
	beforeClearRaw, err := os.ReadFile(persistConc)
	if err != nil {
		t.Fatalf("expected persist file to exist before Clear(): %v", err)
	}
	var beforeClearList []ContextCheckpoint
	if err := json.Unmarshal(beforeClearRaw, &beforeClearList); err != nil {
		t.Fatalf("invalid JSON written by concurrent saves before Clear(): %v\nraw=%s", err, beforeClearRaw)
	}

	rConc.compressor.Clear()
	rConc.compressor.gcAndSave()
	rConc.compressor.saveToDisk()
	finalRaw, err := os.ReadFile(persistConc)
	if err != nil {
		t.Fatalf("read concurrent persist file: %v", err)
	}
	var finalList []ContextCheckpoint
	if err := json.Unmarshal(finalRaw, &finalList); err != nil {
		t.Fatalf("invalid JSON written by concurrent saves: %v\nraw=%s", err, finalRaw)
	}
	if len(finalList) != 0 {
		t.Fatalf("expected 0 checkpoints on disk after Clear(), got %d resurrected entries: %s", len(finalList), finalRaw)
	}
}
