package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeTierBackend struct {
	id   string
	srv  *httptest.Server
	down atomic.Bool

	mu   sync.Mutex
	reqs []map[string]any
}

func startFakeTierBackend(t *testing.T, id string, ctxErrOnMarker bool) *fakeTierBackend {
	t.Helper()
	fb := &fakeTierBackend{id: id}
	fb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get_server_info" {
			if fb.down.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"down"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model_path":"fake-` + id + `"}`))
			return
		}
		if fb.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"backend down"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var reqObj map[string]any
		_ = json.Unmarshal(raw, &reqObj)
		fb.mu.Lock()
		fb.reqs = append(fb.reqs, reqObj)
		fb.mu.Unlock()

		if ctxErrOnMarker && strings.Contains(string(raw), "TRIGGER_CTX_400") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 1000 tokens. However, your messages resulted in 950 tokens and max_completion_tokens=200."}}`))
			return
		}

		if stream, _ := reqObj["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"[%s] \"},\"finish_reason\":null}]}\n\n", id)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"stream-ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":140,\"completion_tokens\":8,\"prompt_tokens_details\":{\"cached_tokens\":30},\"total_tokens\":148}}\n\n")
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}

		promptToks := 80
		if id == "t2" {
			promptToks = 280
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":      "chatcmpl-" + id,
			"object":  "chat.completion",
			"created": 1795000000,
			"model":   "deepseek-v3",
			"choices": []any{
				map[string]any{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":    "assistant",
						"content": "[" + id + "] ok",
					},
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     promptToks,
				"completion_tokens": 6,
				"total_tokens":      promptToks + 6,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": 20,
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeTierBackend) count() int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return len(fb.reqs)
}

func (fb *fakeTierBackend) lastReq() map[string]any {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.reqs) == 0 {
		return nil
	}
	return fb.reqs[len(fb.reqs)-1]
}

func TestG12TieredRouter(t *testing.T) {
	rep := newReport(t, "G12-tiered-router", "G12 内置分层路由器 (Shadow & Active)",
		"验证 Shadow 模式旁路对比与日志落盘、热切换至 Active 保留前缀缓存、短/长请求分层、粘性滞回、400 超长自动重试、max_tokens 钳制、tools 规范化、健康探测故障转移以及 Starter 观察/钉选/预热。")

	t1 := startFakeTierBackend(t, "t1", true)
	t2 := startFakeTierBackend(t, "t2", false)

	var shadowHits atomic.Int32
	shadowBase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shadowHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Router-Backend", "t1")
		w.Header().Set("X-Router-Reason", "upstream-shadow-t1")
		_, _ = w.Write([]byte(`{"id":"shadow1","object":"chat.completion","created":1795000000,"model":"deepseek-v3","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"[shadow-base] ok"}}],"usage":{"prompt_tokens":110,"completion_tokens":4,"total_tokens":114,"prompt_tokens_details":{"cached_tokens":10}}}`))
	}))
	t.Cleanup(shadowBase.Close)

	adminAddr := freePort(t)
	cfgYAML := fmt.Sprintf(`
listen: {{LISTEN}}
mode: personal
admin:
  listen: %s
  session_ttl: 1h
providers:
  - name: ds
    base_url: %s/v1
    target: local
    models: [deepseek-v3]
    tiered:
      mode: shadow
      heavy_miss_threshold: 10000
      health_interval_sec: 1
      zh_think_anchor: false
      backends:
        - id: t1
          url: %s
          tier: 1
          max_context_tokens: 20000
          max_safe_input: 900
          promote_at: 200
          speed_score: 100
        - id: t2
          url: %s
          tier: 2
          max_context_tokens: 65536
          max_safe_input: 3800
          promote_at: 3800
          demote_below: 120
          speed_score: 60
      starter_cache:
        enabled: true
        canonicalize_tools: true
        min_tokens: 80
        max_tokens: 8000
        min_hits: 2
        max_entries: 32
        keepalive_sec: 300
        check_interval_sec: 60
        startup_delay_sec: 60
        entry_ttl_hours: 24
default_group: main
routing_groups:
  main:
    type: order
    members: [ds/deepseek-v3]
`, adminAddr, shadowBase.URL, t1.srv.URL, t2.srv.URL)

	g := startGateway(t, cfgYAML, nil)
	adminBase := "http://" + adminAddr
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(adminBase + "/admin/"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	pwCmd := exec.Command(gatehouse, "admin", "set-password", "--config", filepath.Join(g.Dir, "config.yaml"))
	pwCmd.Stdin = strings.NewReader("admin12345\n")
	if out, err := pwCmd.CombinedOutput(); err != nil {
		t.Fatalf("admin set-password: %v\n%s", err, out)
	}

	jar, _ := cookiejar.New(nil)
	admClient := &http.Client{Jar: jar}
	doAdm := func(method, path string, body any) (int, map[string]any, string) {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, adminBase+path, rdr)
		req.Header.Set("X-Gatehouse-Admin", "1")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := admClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return resp.StatusCode, m, string(raw)
	}

	if st, _, raw := doAdm(http.MethodPost, "/admin/api/login", map[string]string{"username": "admin", "password": "admin12345"}); st != 200 {
		t.Fatalf("admin login failed: %d %s", st, raw)
	}

	rawKeyOut := g.CLI(t, "apikey", "create", "--json", "--name", "e2e-tiered")
	var keyRes struct {
		Key string `json:"key"`
	}
	if idx := strings.Index(rawKeyOut, "{"); idx < 0 || json.Unmarshal([]byte(rawKeyOut[idx:]), &keyRes) != nil || keyRes.Key == "" {
		t.Fatalf("apikey create --json output: %s", rawKeyOut)
	}
	tok := keyRes.Key

	// 8. Shadow mode: goes to base_url, X-Router-Backend returned, request_log records route_backend, warm returns 409
	sysStarter := "You are an expert systems architect working inside a strict workspace verification harness. " + strings.Repeat("Guideline alpha beta gamma delta. ", 14) // ~110+ est tokens
	shadowReq := map[string]any{
		"model": "ds/deepseek-v3",
		"messages": []any{
			map[string]any{"role": "system", "content": sysStarter},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	rSh := g.Chat(t, tok, shadowReq, map[string]string{"User-Agent": "claude-code/1.0"})
	_, logsMap, rawLogs := doAdm(http.MethodGet, "/admin/api/logs?limit=5", nil)
	logsArr, _ := logsMap["logs"].([]any)
	firstRouteBackend := ""
	if len(logsArr) > 0 {
		if row, _ := logsArr[0].(map[string]any); row != nil {
			firstRouteBackend = fmt.Sprint(row["route_backend"])
		}
	}
	_, tieredShadow, rawTieredShadow := doAdm(http.MethodGet, "/admin/api/tiered", nil)
	provsSh, _ := tieredShadow["providers"].([]any)
	cachedBeforeHot := 0
	if len(provsSh) > 0 {
		if p0, _ := provsSh[0].(map[string]any); p0 != nil {
			if c, ok := p0["cached_prefixes"].(float64); ok {
				cachedBeforeHot = int(c)
			}
		}
	}
	// Send second starter hit in shadow mode too (so starter qualifies >=2 hits)
	_ = g.Chat(t, tok, shadowReq, map[string]string{"User-Agent": "claude-code/1.0"})

	sWarmShadow, warmShMap, rawWarmSh := doAdm(http.MethodPost, "/admin/api/tiered/ds/starters/warm", map[string]any{"force": true})
	rep.Check("8. Shadow 模式转发至 base_url，返回 X-Router-Backend 且 request_log 写入 route_backend (warm 返回 409)",
		rSh.Status == 200 && shadowHits.Load() == 2 && t1.count() == 0 && t2.count() == 0 &&
			rSh.Header.Get("X-Router-Backend") == "t1" && firstRouteBackend == "t1" &&
			sWarmShadow == 409 && warmShMap["error"] == "shadow_mode",
		"status=%d hits=%d t1=%d t2=%d hdr=%q logRoute=%q warm=(%d,%s) logs=%s",
		rSh.Status, shadowHits.Load(), t1.count(), t2.count(), rSh.Header.Get("X-Router-Backend"),
		firstRouteBackend, sWarmShadow, rawWarmSh, rawLogs)

	// Re-read cached_prefixes right after 2 shadow completions before hot switch
	_, tieredBeforeHot, _ := doAdm(http.MethodGet, "/admin/api/tiered", nil)
	if provs, _ := tieredBeforeHot["providers"].([]any); len(provs) > 0 {
		if p0, _ := provs[0].(map[string]any); p0 != nil {
			if c, ok := p0["cached_prefixes"].(float64); ok {
				cachedBeforeHot = int(c)
			}
		}
	}
	_ = rawTieredShadow

	// 9. Hot-switch shadow -> active via PUT /admin/api/config/sections/providers keeps router & cached_prefixes
	_, cfgMap, rawCfg := doAdm(http.MethodGet, "/admin/api/config", nil)
	baseHash, _ := cfgMap["hash"].(string)
	cfgObj, _ := cfgMap["raw"].(map[string]any)
	provsList, _ := cfgObj["providers"].([]any)
	if len(provsList) == 0 {
		t.Fatalf("expected providers in config: %s", rawCfg)
	}
	dsCfg, _ := provsList[0].(map[string]any)
	dsTiered, _ := dsCfg["tiered"].(map[string]any)
	dsTiered["mode"] = "active"
	dsCfg["base_url"] = ""
	sPut, _, rawPut := doAdm(http.MethodPut, "/admin/api/config/sections/providers", map[string]any{
		"base_hash": baseHash,
		"data":      provsList,
	})
	_, tieredActive, rawTieredActive := doAdm(http.MethodGet, "/admin/api/tiered", nil)
	provsAct, _ := tieredActive["providers"].([]any)
	modeAfterHot := ""
	cachedAfterHot := 0
	if len(provsAct) > 0 {
		if p0, _ := provsAct[0].(map[string]any); p0 != nil {
			modeAfterHot = fmt.Sprint(p0["mode"])
			if c, ok := p0["cached_prefixes"].(float64); ok {
				cachedAfterHot = int(c)
			}
		}
	}
	rep.Check("9. 热切换 shadow -> active 成功且保留同一 Router 的前缀缓存 (cached_prefixes 未清零)",
		sPut == 200 && modeAfterHot == "active" && cachedBeforeHot > 0 && cachedAfterHot >= cachedBeforeHot,
		"put=%d(%s) mode=%q before=%d after=%d tiered=%s",
		sPut, rawPut, modeAfterHot, cachedBeforeHot, cachedAfterHot, rawTieredActive)

	// 1. Short request -> tier1
	rShort := g.Chat(t, tok, map[string]any{
		"model":    "ds/deepseek-v3",
		"messages": msgs("user", "short ping"),
	}, nil)
	rep.Check("1. 短请求命中 tier1 (t1)",
		rShort.Status == 200 && rShort.Header.Get("X-Router-Backend") == "t1" && strings.Contains(rShort.Content(), "[t1]"),
		"status=%d hdr=%q body=%s", rShort.Status, rShort.Header.Get("X-Router-Backend"), rShort.Content())

	// 2. Long request -> tier2 (with system+user+assistant+user >= 200 tokens, plus verify SSE stream)
	sessSys := "Session system context baseline anchor. " + strings.Repeat("Context clause ", 22) // ~110 tokens
	sessU1 := "User turn one prompt. " + strings.Repeat("Details about module ", 14)              // ~80 tokens (sys+u1 ~190 -> in [120, 200))
	longA1 := "Assistant turn one answer. " + strings.Repeat("Analysis paragraph ", 18)        // ~95 tokens
	longU2 := "User turn two follow up question asking for final summary."                      // total ~290 tokens >= 200
	longReq := map[string]any{
		"model": "ds/deepseek-v3",
		"messages": []any{
			map[string]any{"role": "system", "content": sessSys},
			map[string]any{"role": "user", "content": sessU1},
			map[string]any{"role": "assistant", "content": longA1},
			map[string]any{"role": "user", "content": longU2},
		},
	}
	rLong := g.Chat(t, tok, longReq, nil)
	rSSE := g.Chat(t, tok, map[string]any{
		"model":    "ds/deepseek-v3",
		"stream":   true,
		"messages": msgs("user", "short sse check"),
	}, nil)
	rep.Check("2. 长请求升级命中 tier2 (t2)，且 SSE 流式透传正常",
		rLong.Status == 200 && rLong.Header.Get("X-Router-Backend") == "t2" && strings.Contains(rLong.Content(), "[t2]") &&
			rSSE.Status == 200 && rSSE.Header.Get("X-Router-Backend") == "t1" && strings.Contains(string(rSSE.Raw), "stream-ok"),
		"status=%d hdr=%q reason=%q body=%s sse=%d", rLong.Status, rLong.Header.Get("X-Router-Backend"), rLong.Header.Get("X-Router-Reason"), rLong.Content(), rSSE.Status)

	// 3. Sticky hysteresis: same session prefix (hashes[1] == sessSys+sessU1, est ~150 in [demote_below 120, promote_at 200)) stays on tier2!
	stickyReq := map[string]any{
		"model": "ds/deepseek-v3",
		"messages": []any{
			map[string]any{"role": "system", "content": sessSys},
			map[string]any{"role": "user", "content": sessU1},
		},
	}
	rSticky := g.Chat(t, tok, stickyReq, nil)
	rep.Check("3. 同会话长度回落到 [demote_below, promote_at) 区间保持粘性滞回在 tier2",
		rSticky.Status == 200 && rSticky.Header.Get("X-Router-Backend") == "t2" && strings.Contains(rSticky.Header.Get("X-Router-Reason"), "sticky"),
		"status=%d hdr=%q reason=%q", rSticky.Status, rSticky.Header.Get("X-Router-Backend"), rSticky.Header.Get("X-Router-Reason"))

	// 4. Backend 400 context-length error retries on larger tier (t1 -> 400 -> t2 200)
	t1BeforeRetry := t1.count()
	t2BeforeRetry := t2.count()
	rRetry := g.Chat(t, tok, map[string]any{
		"model":    "ds/deepseek-v3",
		"messages": msgs("user", "TRIGGER_CTX_400 short"),
	}, nil)
	rep.Check("4. t1 返回 400 context-length 错误后自动重试更大 tier (t2) 并返回 200",
		rRetry.Status == 200 && rRetry.Header.Get("X-Router-Backend") == "t2" &&
			t1.count() == t1BeforeRetry+1 && t2.count() == t2BeforeRetry+1,
		"status=%d hdr=%q t1=%d->%d t2=%d->%d",
		rRetry.Status, rRetry.Header.Get("X-Router-Backend"), t1BeforeRetry, t1.count(), t2BeforeRetry, t2.count())

	// 5. max_tokens clamping on non-highest tier (t1 max_context_tokens=20000, max_tokens=18000 > safeCap gets clamped)
	rClamp := g.Chat(t, tok, map[string]any{
		"model":      "ds/deepseek-v3",
		"max_tokens": 18000,
		"messages":   msgs("user", "Clamp check prompt: "+strings.Repeat("token budget verification ", 12)),
	}, nil)
	t1LastClamp := t1.lastReq()
	clampedVal, _ := t1LastClamp["max_tokens"].(float64)
	rep.Check("5. 非最高 tier 请求 max_tokens 超窗时被安全钳制",
		rClamp.Status == 200 && rClamp.Header.Get("X-Router-Backend") == "t1" &&
			strings.HasPrefix(rClamp.Header.Get("X-Router-Max-Tokens-Clamped"), "18000->") &&
			clampedVal >= 8192 && clampedVal < 18000,
		"status=%d clampHdr=%q upstream_max_tokens=%v",
		rClamp.Status, rClamp.Header.Get("X-Router-Max-Tokens-Clamped"), t1LastClamp["max_tokens"])

	// 6. Canonicalize tools sorts tool declarations deterministically and flags header
	rTools := g.Chat(t, tok, map[string]any{
		"model":    "ds/deepseek-v3",
		"messages": msgs("user", "call tool"),
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "zeta_tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}, "a": map[string]any{"type": "string"}}}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "alpha_tool", "parameters": map[string]any{"type": "object"}}},
		},
	}, nil)
	t1LastTools := t1.lastReq()
	firstToolName := ""
	if ts, _ := t1LastTools["tools"].([]any); len(ts) == 2 {
		if t0, _ := ts[0].(map[string]any); t0 != nil {
			if fn, _ := t0["function"].(map[string]any); fn != nil {
				firstToolName = fmt.Sprint(fn["name"])
			}
		}
	}
	rep.Check("6. tools 乱序传入时按规范化排序并带 X-Router-Tools-Canonicalized=1",
		rTools.Status == 200 && rTools.Header.Get("X-Router-Tools-Canonicalized") == "1" && firstToolName == "alpha_tool",
		"status=%d hdr=%q firstTool=%q", rTools.Status, rTools.Header.Get("X-Router-Tools-Canonicalized"), firstToolName)

	// 7. Backend down -> health probe marks t1 unhealthy -> short request goes straight to t2 without hitting t1 -> /admin/api/tiered shows healthy=false
	t1.down.Store(true)
	unhealthySeen := false
	probeDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(probeDeadline) {
		_, tm, _ := doAdm(http.MethodGet, "/admin/api/tiered", nil)
		if ps, _ := tm["providers"].([]any); len(ps) > 0 {
			if p0, _ := ps[0].(map[string]any); p0 != nil {
				if bs, _ := p0["backends"].([]any); len(bs) > 0 {
					if b0, _ := bs[0].(map[string]any); b0 != nil && b0["id"] == "t1" && b0["healthy"] == false {
						unhealthySeen = true
						break
					}
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t1CountDown := t1.count()
	rFailover := g.Chat(t, tok, map[string]any{
		"model":    "ds/deepseek-v3",
		"messages": msgs("user", "short while t1 down"),
	}, nil)
	rep.Check("7. t1 故障后健康探测标记为 unhealthy，短请求直接路由到 t2 且 /admin/api/tiered 可见",
		unhealthySeen && rFailover.Status == 200 && rFailover.Header.Get("X-Router-Backend") == "t2" && t1.count() == t1CountDown,
		"unhealthySeen=%v status=%d hdr=%q t1Hits=%d->%d",
		unhealthySeen, rFailover.Status, rFailover.Header.Get("X-Router-Backend"), t1CountDown, t1.count())

	// Bring t1 back healthy before starter warm so warm tests both backends
	t1.down.Store(false)
	rehealthyDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(rehealthyDeadline) {
		_, tm, _ := doAdm(http.MethodGet, "/admin/api/tiered", nil)
		if ps, _ := tm["providers"].([]any); len(ps) > 0 {
			if p0, _ := ps[0].(map[string]any); p0 != nil {
				if bs, _ := p0["backends"].([]any); len(bs) > 0 {
					if b0, _ := bs[0].(map[string]any); b0 != nil && b0["id"] == "t1" && b0["healthy"] == true {
						break
					}
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}

	// 10. Starters: observe (already >=2 hits from claude-code/1.0 above), pin, warm (200 in active)
	_, stMap, rawSt := doAdm(http.MethodGet, "/admin/api/tiered/ds/starters", nil)
	stArr, _ := stMap["starters"].([]any)
	starterHash := ""
	if len(stArr) > 0 {
		if s0, _ := stArr[0].(map[string]any); s0 != nil {
			starterHash = fmt.Sprint(s0["hash"])
		}
	}
	sPin, pinMap, rawPin := doAdm(http.MethodPost, "/admin/api/tiered/ds/starters/pin", map[string]any{
		"hash":   starterHash,
		"pinned": true,
		"label":  "e2e-starter",
	})
	pinnedVal := false
	if stObj, _ := pinMap["starter"].(map[string]any); stObj != nil {
		pinnedVal, _ = stObj["pinned"].(bool)
	}
	sWarmAct, warmActMap, rawWarmAct := doAdm(http.MethodPost, "/admin/api/tiered/ds/starters/warm", map[string]any{
		"hash":  starterHash,
		"force": true,
	})
	warmResults, _ := warmActMap["results"].([]any)
	warmOK := len(warmResults) > 0
	for _, r := range warmResults {
		rm, _ := r.(map[string]any)
		if rm == nil || (rm["error"] != nil && fmt.Sprint(rm["error"]) != "") {
			warmOK = false
		}
	}
	rep.Check("10. Starter 自动观察入库，可 pin 钉选并在 active 模式下手动 warm 返回 200",
		starterHash != "" && sPin == 200 && pinnedVal && sWarmAct == 200 && warmOK,
		"starters=%d(%s) pin=(%d,%s) warm=(%d,%s)",
		len(stArr), rawSt, sPin, rawPin, sWarmAct, rawWarmAct)
}
