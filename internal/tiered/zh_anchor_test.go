package tiered

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInjectZhThinkAnchor_StringUser(t *testing.T) {
	body := []byte(`{"model":"DeepSeek-V4.1-Flash-1017","reasoning_effort":"max","messages":[{"role":"system","content":"【强制规范】每次在 <think>...</think> 块中进行内部推理思考时，必须全程使用简体中文撰写。"},{"role":"user","content":"Why is the sky blue?"}]}`)
	out, changed := injectZhThinkAnchor(body)
	if !changed {
		t.Fatalf("expected changed=true")
	}
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(parsed.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(parsed.Messages))
	}
	if parsed.Messages[1].Content != "Why is the sky blue?" {
		t.Fatalf("user message should remain untouched, got %q", parsed.Messages[1].Content)
	}
	if parsed.Messages[2].Role != "system" || !strings.Contains(parsed.Messages[2].Content, zhThinkAnchorMarker) {
		t.Fatalf("unexpected trailing anchor: %+v", parsed.Messages[2])
	}
	out2, changed2 := injectZhThinkAnchor(out)
	if changed2 || string(out2) != string(out) {
		t.Fatalf("expected idempotent second call")
	}
}

func TestInjectZhThinkAnchor_ToolRoleAndArrayContent(t *testing.T) {
	bodyTool := []byte(`{"model":"m","messages":[{"role":"system","content":"【强制规范】必须全程使用简体中文思考"},{"role":"assistant","content":"calling tool"},{"role":"tool","tool_call_id":"call_1","content":"{\"ok\":true}"}]}`)
	outTool, changed := injectZhThinkAnchor(bodyTool)
	if !changed {
		t.Fatalf("expected tool turn anchored")
	}
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(outTool, &parsed); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(parsed.Messages) != 4 || parsed.Messages[2].Content != `{"ok":true}` || parsed.Messages[3].Role != "system" {
		t.Fatalf("unexpected tool messages: %+v", parsed.Messages)
	}

	bodyArr := []byte(`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"【强制规范】必须使用简体中文在 <think> 中思考"}]},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}},{"type":"text","text":"Describe this image"}]}]}`)
	outArr, changed := injectZhThinkAnchor(bodyArr)
	if !changed || !strings.Contains(string(outArr), zhThinkAnchorMarker) {
		t.Fatalf("expected array user turn anchored")
	}
}

func TestInjectZhThinkAnchor_Skips(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no_zh_system", `{"model":"m","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hi"}]}`},
		{"reasoning_none", `{"model":"m","reasoning_effort":"none","messages":[{"role":"system","content":"【强制规范】简体中文"},{"role":"user","content":"Hi"}]}`},
		{"enable_thinking_false", `{"model":"m","enable_thinking":false,"messages":[{"role":"system","content":"【强制规范】简体中文"},{"role":"user","content":"Hi"}]}`},
		{"max_tokens_1_warmup", `{"model":"m","max_tokens":1,"messages":[{"role":"system","content":"【强制规范】简体中文"},{"role":"user","content":"Hi"}]}`},
		{"assistant_last", `{"model":"m","messages":[{"role":"system","content":"【强制规范】简体中文"},{"role":"user","content":"Hi"},{"role":"assistant","content":"Sure"}]}`},
	}
	for _, tc := range cases {
		_, changed := injectZhThinkAnchor([]byte(tc.body))
		if changed {
			t.Errorf("case %s: expected changed=false, got true", tc.name)
		}
	}
}
