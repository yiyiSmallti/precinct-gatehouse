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
	r.compressor.dirty = true
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
