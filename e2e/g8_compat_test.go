package e2e

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const g8Config = `
listen: {{LISTEN}}
mode: personal
providers:
  - name: ds
    base_url: %s
    target: local
    models: [deepseek-chat]
    system_prompt: "【网关规范】中文思考"
    extra_body:
      chat_template_kwargs:
        thinking: true
default_group: main
routing_groups:
  main:
    members: [ds/deepseek-chat]
model_aliases:
  - match: "claude-sonnet-4-5"
    use: main
  - match: "claude-*"
    use: auto
  - match: "gpt-*"
    use: ds/deepseek-chat
`

type sseEvent struct {
	Event string
	Data  map[string]any
	Raw   string
}

func parseSSE(raw []byte) []sseEvent {
	var out []sseEvent
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var curEvent string
	var curData []string
	flush := func() {
		if curEvent == "" && len(curData) == 0 {
			return
		}
		joined := strings.Join(curData, "\n")
		ev := sseEvent{Event: curEvent, Raw: joined}
		if strings.HasPrefix(joined, "{") {
			_ = json.Unmarshal([]byte(joined), &ev.Data)
		}
		out = append(out, ev)
		curEvent = ""
		curData = nil
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if v, ok := strings.CutPrefix(line, "event:"); ok {
			curEvent = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "data:"); ok {
			curData = append(curData, strings.TrimSpace(v))
		}
	}
	flush()
	return out
}

func postJSON(t *testing.T, url string, headers map[string]string, body any) Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := Response{Status: resp.StatusCode, Header: resp.Header, Raw: raw}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func TestG8Compat(t *testing.T) {
	rep := newReport(t, "G8-compat", "G8 协议适配（Anthropic Messages & OpenAI Responses）",
		"验证 `/v1/messages`、`/v1/messages/count_tokens`、`/v1/responses` 的非流式/流式、thinking、tool_calls、别名路由、`system_prompt`/`extra_body` 注入及限额与 `request_log`。")

	fake := startFake(t, "ds")
	g := startGateway(t, fmt.Sprintf(g8Config, fake.URL), nil)

	createOut := g.CLI(t, "apikey", "create", "--name", "compat-key", "--json")
	var keyRes struct {
		Key    string `json:"key"`
		APIKey struct {
			ID string `json:"id"`
		} `json:"api_key"`
	}
	if err := json.Unmarshal([]byte(createOut[strings.Index(createOut, "{"):]), &keyRes); err != nil {
		t.Fatalf("apikey create: %s", createOut)
	}
	sk := keyRes.Key

	// Check /v1/models lists non-wildcard alias claude-sonnet-4-5
	mReq, _ := http.NewRequest(http.MethodGet, g.URL+"/models", nil)
	mReq.Header.Set("x-api-key", sk)
	mResp, err := http.DefaultClient.Do(mReq)
	if err != nil {
		t.Fatal(err)
	}
	mRaw, _ := io.ReadAll(mResp.Body)
	mResp.Body.Close()
	rep.Check("/v1/models 支持 sk- 且列出精确别名", mResp.StatusCode == 200 && strings.Contains(string(mRaw), `"claude-sonnet-4-5"`) && !strings.Contains(string(mRaw), `"claude-*"`), "status=%d", mResp.StatusCode)

	// --- Anthropic Messages ---
	antHdr := map[string]string{"x-api-key": sk}
	rAnt := postJSON(t, g.URL+"/messages", antHdr, map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 512,
		"system":     "你是资深工程师。",
		"messages": []any{
			map[string]any{"role": "user", "content": "你好 #think"},
		},
	})
	contentBlocks, _ := rAnt.Body["content"].([]any)
	hasThinkBlock, hasTextBlock := false, false
	for _, b := range contentBlocks {
		bm, _ := b.(map[string]any)
		if bm["type"] == "thinking" && bm["thinking"] == "思考..." {
			hasThinkBlock = true
		}
		if bm["type"] == "text" && strings.Contains(fmt.Sprint(bm["text"]), "你好 #think") {
			hasTextBlock = true
		}
	}
	rep.Check("Anthropic 非流式 + x-api-key + thinking + 保留原 model",
		rAnt.Status == 200 && rAnt.Body["type"] == "message" && rAnt.Body["model"] == "claude-sonnet-4-5" && hasThinkBlock && hasTextBlock,
		"status=%d model=%v think=%v text=%v", rAnt.Status, rAnt.Body["model"], hasThinkBlock, hasTextBlock)

	// Check upstream got system_prompt concatenation and extra_body
	reqs := fake.ChatRequests()
	lastUp := reqs[len(reqs)-1]
	upMsgs, _ := lastUp["messages"].([]any)
	sysText := ""
	if len(upMsgs) > 0 {
		sysText, _ = upMsgs[0].(map[string]any)["content"].(string)
	}
	ctk, _ := lastUp["chat_template_kwargs"].(map[string]any)
	rep.Check("上游收到 system_prompt 拼接与 extra_body",
		strings.HasPrefix(sysText, "【网关规范】中文思考\n\n你是资深工程师。") && ctk["thinking"] == true,
		"sys=%q extra=%v", sysText, ctk)

	// Anthropic tools + round 2 tool_result
	antTools := []any{map[string]any{
		"name":         "echo",
		"description":  "echo input",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{"echo": map[string]any{"type": "string"}}},
	}}
	rAntTool := postJSON(t, g.URL+"/messages", antHdr, map[string]any{
		"model":      "claude-3-7-sonnet",
		"max_tokens": 512,
		"tools":      antTools,
		"messages":   []any{map[string]any{"role": "user", "content": "请调用工具"}},
	})
	var toolUseID string
	var toolInputMap bool
	if cbs, ok := rAntTool.Body["content"].([]any); ok {
		for _, b := range cbs {
			bm, _ := b.(map[string]any)
			if bm["type"] == "tool_use" {
				toolUseID, _ = bm["id"].(string)
				_, toolInputMap = bm["input"].(map[string]any)
			}
		}
	}
	rep.Check("Anthropic tools → tool_use（input 为对象）",
		rAntTool.Status == 200 && rAntTool.Body["stop_reason"] == "tool_use" && toolUseID != "" && toolInputMap,
		"stop=%v id=%s input_is_obj=%v", rAntTool.Body["stop_reason"], toolUseID, toolInputMap)

	rAntR2 := postJSON(t, g.URL+"/messages", antHdr, map[string]any{
		"model":      "claude-3-7-sonnet",
		"max_tokens": 512,
		"tools":      antTools,
		"messages": []any{
			map[string]any{"role": "user", "content": "请调用工具"},
			map[string]any{"role": "assistant", "content": rAntTool.Body["content"]},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": toolUseID, "content": "工具返回OK"},
			}},
		},
	})
	reqs = fake.ChatRequests()
	r2Up := reqs[len(reqs)-1]
	r2Msgs, _ := r2Up["messages"].([]any)
	foundRoleTool := false
	for _, rm := range r2Msgs {
		mo, _ := rm.(map[string]any)
		if mo["role"] == "tool" && mo["tool_call_id"] == toolUseID && mo["content"] == "工具返回OK" {
			foundRoleTool = true
		}
	}
	rep.Check("Anthropic 第二轮 tool_result → 上游 role:tool 且 tool_call_id 匹配",
		rAntR2.Status == 200 && foundRoleTool, "status=%d found_tool_msg=%v", rAntR2.Status, foundRoleTool)

	// Anthropic streaming with #think and tools
	rAntStream := postJSON(t, g.URL+"/messages", antHdr, map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 512,
		"stream":     true,
		"tools":      antTools,
		"messages":   []any{map[string]any{"role": "user", "content": "流式 #think"}},
	})
	evs := parseSSE(rAntStream.Raw)
	var evNames []string
	hasThinkDelta, hasTextDelta, hasInputJSONDelta := false, false, false
	var inputJSONBuf strings.Builder
	for _, ev := range evs {
		evNames = append(evNames, ev.Event)
		if ev.Event == "content_block_delta" {
			d, _ := ev.Data["delta"].(map[string]any)
			switch d["type"] {
			case "thinking_delta":
				hasThinkDelta = true
			case "text_delta":
				hasTextDelta = true
			case "input_json_delta":
				hasInputJSONDelta = true
				inputJSONBuf.WriteString(fmt.Sprint(d["partial_json"]))
			}
		}
	}
	var parsedToolArgs map[string]any
	errJSON := json.Unmarshal([]byte(inputJSONBuf.String()), &parsedToolArgs)
	rep.Check("Anthropic 流式事件顺序与 thinking/text/tool_use delta",
		rAntStream.Status == 200 && len(evNames) >= 8 &&
			evNames[0] == "message_start" &&
			evNames[len(evNames)-2] == "message_delta" &&
			evNames[len(evNames)-1] == "message_stop" &&
			hasThinkDelta && hasTextDelta && hasInputJSONDelta && errJSON == nil,
		"events=%v json=%q", evNames, inputJSONBuf.String())

	// Anthropic bad key -> 401 Anthropic error format
	rAntBad := postJSON(t, g.URL+"/messages", map[string]string{"x-api-key": "sk-bad"}, map[string]any{
		"model": "claude-sonnet-4-5", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	errObj, _ := rAntBad.Body["error"].(map[string]any)
	rep.Check("Anthropic 坏 key → 401 且为 Anthropic 错误格式",
		rAntBad.Status == 401 && rAntBad.Body["type"] == "error" && errObj["type"] == "authentication_error",
		"status=%d body=%s", rAntBad.Status, string(rAntBad.Raw))

	// Anthropic count_tokens -> no upstream call
	beforeCountReqs := len(fake.Requests())
	rCount := postJSON(t, g.URL+"/messages/count_tokens", antHdr, map[string]any{
		"model":    "claude-sonnet-4-5",
		"system":   "系统提示",
		"messages": []any{map[string]any{"role": "user", "content": "统计这段中文 token 数"}},
	})
	inpTok, _ := rCount.Body["input_tokens"].(float64)
	rep.Check("Anthropic count_tokens 返回数字且不调上游",
		rCount.Status == 200 && inpTok > 0 && len(fake.Requests()) == beforeCountReqs,
		"status=%d input_tokens=%.0f", rCount.Status, inpTok)

	// --- OpenAI Responses ---
	oaiHdr := map[string]string{"Authorization": "Bearer " + sk}
	rResp := postJSON(t, g.URL+"/responses", oaiHdr, map[string]any{
		"model":        "gpt-5",
		"instructions": "请严格输出",
		"input":        "你好 Responses",
		"tools": []any{
			map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "web_search"},
		},
	})
	reqs = fake.ChatRequests()
	respUp := reqs[len(reqs)-1]
	respUpMsgs, _ := respUp["messages"].([]any)
	upSys, _ := respUpMsgs[0].(map[string]any)["content"].(string)
	uObj, _ := rResp.Body["usage"].(map[string]any)
	itd, _ := uObj["input_tokens_details"].(map[string]any)
	cachedNum, _ := itd["cached_tokens"].(float64)
	rep.Check("Responses 非流式 + instructions→system + 别名 gpt-* + 丢弃非函数工具头 + cached_tokens",
		rResp.Status == 200 && rResp.Body["object"] == "response" && rResp.Body["status"] == "completed" &&
			rResp.Body["model"] == "gpt-5" && strings.Contains(upSys, "请严格输出") &&
			rResp.Header.Get("X-Gatehouse-Dropped-Tools") == "web_search" && cachedNum > 0,
		"status=%d model=%v dropped=%q cached=%.0f", rResp.Status, rResp.Body["model"], rResp.Header.Get("X-Gatehouse-Dropped-Tools"), cachedNum)

	// Responses round 2 function_call + function_call_output
	rRespR2 := postJSON(t, g.URL+"/responses", oaiHdr, map[string]any{
		"model": "gpt-5",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "用工具"},
			map[string]any{"type": "function_call", "call_id": "call_99", "name": "echo", "arguments": `{"echo":"hi"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_99", "output": "done"},
		},
	})
	reqs = fake.ChatRequests()
	r2RespUp := reqs[len(reqs)-1]
	r2RespMsgs, _ := r2RespUp["messages"].([]any)
	hasAsstTC, hasToolOut := false, false
	for _, rm := range r2RespMsgs {
		mo, _ := rm.(map[string]any)
		if mo["role"] == "assistant" && mo["tool_calls"] != nil {
			hasAsstTC = true
		}
		if mo["role"] == "tool" && mo["tool_call_id"] == "call_99" && mo["content"] == "done" {
			hasToolOut = true
		}
	}
	rep.Check("Responses function_call / function_call_output 转换",
		rRespR2.Status == 200 && hasAsstTC && hasToolOut, "status=%d asst=%v tool=%v", rRespR2.Status, hasAsstTC, hasToolOut)

	// Responses streaming with #think + function_call
	rRespStream := postJSON(t, g.URL+"/responses", oaiHdr, map[string]any{
		"model":  "gpt-5",
		"stream": true,
		"input":  "流式 #think",
		"tools":  []any{map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object"}}},
	})
	rEvs := parseSSE(rRespStream.Raw)
	var rEvNames []string
	seqOK := true
	hasReasonEv, hasMsgEv, hasFuncEv := false, false, false
	for i, ev := range rEvs {
		rEvNames = append(rEvNames, ev.Event)
		if sn, ok := ev.Data["sequence_number"].(float64); !ok || int(sn) != i {
			seqOK = false
		}
		switch ev.Event {
		case "response.reasoning_summary_text.delta":
			hasReasonEv = true
		case "response.output_text.delta":
			hasMsgEv = true
		case "response.function_call_arguments.delta":
			hasFuncEv = true
		}
	}
	rep.Check("Responses 流式事件顺序（reasoning + message + function_call + sequence_number）",
		rRespStream.Status == 200 && len(rEvNames) >= 15 &&
			rEvNames[0] == "response.created" &&
			rEvNames[1] == "response.in_progress" &&
			rEvNames[len(rEvNames)-1] == "response.completed" &&
			seqOK && hasReasonEv && hasMsgEv && hasFuncEv,
		"count=%d seqOK=%v reason=%v msg=%v fn=%v", len(rEvNames), seqOK, hasReasonEv, hasMsgEv, hasFuncEv)

	// previous_response_id -> 400
	rPrev := postJSON(t, g.URL+"/responses", oaiHdr, map[string]any{
		"model":                "gpt-5",
		"input":                "hi",
		"previous_response_id": "resp_123",
	})
	rep.Check("Responses previous_response_id → 400",
		rPrev.Status == 400 && rPrev.ErrCode() == "previous_response_not_supported",
		"status=%d code=%s", rPrev.Status, rPrev.ErrCode())

	// Rate limit check on compat protocols (rpm=1 on a fresh key)
	limOut := g.CLI(t, "apikey", "create", "--name", "compat-rpm1", "--rpm", "1", "--json")
	var limKey struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(limOut[strings.Index(limOut, "{"):]), &limKey); err != nil {
		t.Fatalf("apikey create lim: %s", limOut)
	}
	rLim1 := postJSON(t, g.URL+"/messages", map[string]string{"x-api-key": limKey.Key}, map[string]any{
		"model": "claude-sonnet-4-5", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "1"}},
	})
	rLim2 := postJSON(t, g.URL+"/messages", map[string]string{"x-api-key": limKey.Key}, map[string]any{
		"model": "claude-sonnet-4-5", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "2"}},
	})
	rLimErr, _ := rLim2.Body["error"].(map[string]any)
	rep.Check("compat 协议受 RPM 限额约束（第 2 次 429 rate_limit_error）",
		rLim1.Status == 200 && rLim2.Status == 429 && rLimErr["type"] == "rate_limit_error",
		"s1=%d s2=%d err=%v", rLim1.Status, rLim2.Status, rLimErr)

	// Verify request_log recorded messages and responses protocols
	db, err := sql.Open("sqlite", "file:"+filepath.Join(g.Dir, "gatehouse.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var msgCnt, respCnt, cachedSum int
	_ = db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE protocol='messages' AND status=200`).Scan(&msgCnt)
	_ = db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE protocol='responses' AND status=200`).Scan(&respCnt)
	_ = db.QueryRow(`SELECT COALESCE(SUM(cached_tokens),0) FROM request_log`).Scan(&cachedSum)
	rep.Check("request_log 记录了 messages/responses 协议及 cached_tokens",
		msgCnt >= 4 && respCnt >= 3 && cachedSum > 0,
		"messages=%d responses=%d cached_sum=%d", msgCnt, respCnt, cachedSum)

	rep.Note("Anthropic 流式事件序列：`%s`", strings.Join(evNames, " → "))
	rep.Note("Responses 流式事件序列：`%s`", strings.Join(rEvNames, " → "))
}
