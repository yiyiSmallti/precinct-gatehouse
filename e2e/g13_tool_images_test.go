package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

const g13Config = `
listen: {{LISTEN}}
mode: personal
providers:
  - name: ds
    base_url: %s
    target: local
    models: [deepseek-chat]
default_group: main
routing_groups:
  main:
    members: [ds/deepseek-chat]
model_aliases:
  - match: "gpt-*"
    use: ds/deepseek-chat
redaction:
  mode_by_target: { local: mask }
  categories: { phone: mask, id_card: mask }
`

// g13DataURI returns a PNG data URI whose base64 payload is payloadLen bytes.
func g13DataURI(seed int64, payloadLen int) string {
	raw := make([]byte, payloadLen*3/4)
	rand.New(rand.NewSource(seed)).Read(raw)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

func TestG13ToolImages(t *testing.T) {
	rep := newReport(t, "G13-tool-images", "G13 工具图片与大 base64（issue #11）",
		"本地目标开启 `mask` 脱敏。`/v1/responses` 一轮内 4 个并行工具调用分别以内容数组、单个图片对象、JSON 编码字符串、内嵌 data URI 文本返回图片，并带 Codex Desktop 的 `additional_tools` 输入项；另以 Chat Completions 发送夹着 ~3 MB base64 图片的文本。断言假上游收到的消息结构、脱敏结果与耗时。")
	fake := startFake(t, "ds")
	g := startGateway(t, fmt.Sprintf(g13Config, fake.URL), nil)
	hdr := map[string]string{"Authorization": "Bearer " + g.Token}

	big := g13DataURI(1, 3<<20)
	smallB := g13DataURI(2, 8<<10)
	smallC := g13DataURI(3, 8<<10)
	smallD := g13DataURI(4, 8<<10)
	jsonC, _ := json.Marshal([]any{
		map[string]any{"type": "input_text", "text": "截图 C"},
		map[string]any{"type": "input_image", "image_url": smallC},
	})
	call := func(id string) map[string]any {
		return map[string]any{"type": "function_call", "call_id": id, "name": "screenshot", "arguments": "{}"}
	}
	start := time.Now()
	r := postJSON(t, g.URL+"/responses", hdr, map[string]any{
		"model": "gpt-5",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "看图"},
			map[string]any{"type": "additional_tools", "tools": []any{
				map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}},
			}},
			call("call_a"), call("call_b"), call("call_c"), call("call_d"),
			map[string]any{"type": "function_call_output", "call_id": "call_a", "output": []any{
				map[string]any{"type": "input_text", "text": "截图 A，联系 " + g2Phone},
				map[string]any{"type": "input_image", "image_url": big},
			}},
			map[string]any{"type": "function_call_output", "call_id": "call_b", "output": map[string]any{
				"type": "input_image", "image_url": map[string]any{"url": smallB, "detail": "high"},
			}},
			map[string]any{"type": "function_call_output", "call_id": "call_c", "output": string(jsonC)},
			map[string]any{"type": "function_call_output", "call_id": "call_d", "output": "日志 D " + smallD + " 结束"},
			map[string]any{"type": "message", "role": "user", "content": "继续"},
		},
		"tools": []any{map[string]any{"type": "function", "name": "screenshot", "parameters": map[string]any{"type": "object"}}},
	})
	respElapsed := time.Since(start)
	rep.Check("Responses 含 `additional_tools` 输入项与 4 路工具图片返回 200", r.Status == 200, "status=%d body=%.200s", r.Status, r.Raw)
	if r.Status != 200 {
		t.FailNow()
	}

	reqs := fake.ChatRequests()
	up := reqs[len(reqs)-1]
	upMsgs, _ := up["messages"].([]any)
	var roles []string
	for _, m := range upMsgs {
		roles = append(roles, fmt.Sprint(m.(map[string]any)["role"]))
	}
	wantRoles := "user,assistant,tool,tool,tool,tool,user,user"
	rep.Check("消息顺序：4 条 tool 紧跟 assistant.tool_calls，图片在其后的一条 user 消息里",
		strings.Join(roles, ",") == wantRoles, "roles=%s", strings.Join(roles, ","))
	if strings.Join(roles, ",") != wantRoles {
		t.FailNow()
	}

	toolText := map[string]string{}
	toolOK := true
	for _, m := range upMsgs[2:6] {
		mo := m.(map[string]any)
		s, isStr := mo["content"].(string)
		toolText[fmt.Sprint(mo["tool_call_id"])] = s
		if !isStr || strings.Contains(s, "data:image/") {
			toolOK = false
		}
	}
	rep.Check("tool 消息只含纯文本，无 base64",
		toolOK && strings.HasPrefix(toolText["call_a"], "截图 A") && strings.Contains(toolText["call_b"], "image returned by tool") &&
			toolText["call_c"] == "截图 C" && toolText["call_d"] == "日志 D [image] 结束",
		"a=%q b=%q c=%q d=%q", toolText["call_a"], toolText["call_b"], toolText["call_c"], toolText["call_d"])
	rep.Check("tool 文本中的手机号仍被脱敏",
		!strings.Contains(toolText["call_a"], g2Phone), "a=%q", toolText["call_a"])

	var urls []string
	var detailB string
	parts, _ := upMsgs[6].(map[string]any)["content"].([]any)
	for _, p := range parts {
		po := p.(map[string]any)
		if po["type"] == "image_url" {
			iu := po["image_url"].(map[string]any)
			u := fmt.Sprint(iu["url"])
			urls = append(urls, u)
			if u == smallB {
				detailB = fmt.Sprint(iu["detail"])
			}
		}
	}
	rep.Check("4 张图片原样进入 user 消息的 image_url 块（含 detail）",
		len(urls) == 4 && urls[0] == big && urls[1] == smallB && urls[2] == smallC && urls[3] == smallD && detailB == "high",
		"images=%d big_intact=%v detail_b=%q", len(urls), len(urls) > 0 && urls[0] == big, detailB)

	var toolNames []string
	upTools, _ := up["tools"].([]any)
	for _, tl := range upTools {
		fn, _ := tl.(map[string]any)["function"].(map[string]any)
		toolNames = append(toolNames, fmt.Sprint(fn["name"]))
	}
	rep.Check("`additional_tools` 中的函数工具并入上游 tools",
		strings.Join(toolNames, ",") == "screenshot,lookup", "tools=%s", strings.Join(toolNames, ","))

	// Inline ~3 MB base64 inside plain text with PII on both sides.
	text := "联系 " + g2Phone + " 看图 " + big + " 身份证 " + g2ID + " 结束"
	start = time.Now()
	rc := g.Chat(t, g.Token, map[string]any{"model": "ds/deepseek-chat", "messages": msgs("user", text)}, nil)
	chatElapsed := time.Since(start)
	reqs = fake.ChatRequests()
	sent := lastUser(reqs[len(reqs)-1])
	rep.Check("Chat 文本内 3 MB base64 在 mask 模式下不拖慢请求（< 1s，修复前本机 1.29s、线上 90s）",
		rc.Status == 200 && chatElapsed < time.Second, "status=%d elapsed=%s", rc.Status, chatElapsed.Round(time.Millisecond))
	rep.Check("base64 前后的手机号、身份证仍被脱敏，图片数据原样保留",
		!strings.Contains(sent, g2Phone) && !strings.Contains(sent, g2ID) && strings.Contains(sent, big),
		"phone_masked=%v id_masked=%v image_intact=%v", !strings.Contains(sent, g2Phone), !strings.Contains(sent, g2ID), strings.Contains(sent, big))

	rep.Note("耗时（含 HTTP 往返与假上游）：Responses 4 路工具图片 %s；Chat 内嵌 3 MB base64 %s。", respElapsed.Round(time.Millisecond), chatElapsed.Round(time.Millisecond))
}
