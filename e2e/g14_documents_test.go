package e2e

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

const g14Config = `
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
  - match: "claude-*"
    use: ds/deepseek-chat
`

const (
	g14DocxText = "季度报告正文：营收增长百分之十二。"
	g14PDFText  = "Gatehouse quarterly PDF revenue"
)

// g14Docx builds a .docx whose body is two paragraphs.
func g14Docx() []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("[Content_Types].xml")
	w.Write([]byte(`<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`))
	w, _ = zw.Create("word/document.xml")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		`<w:p><w:r><w:t>` + g14DocxText + `</w:t></w:r></w:p><w:p><w:r><w:t xml:space="preserve">第二段 </w:t></w:r><w:r><w:t>结束</w:t></w:r></w:p></w:body></w:document>`))
	zw.Close()
	return buf.Bytes()
}

// g14PDF builds a one-page PDF showing text, with a correct xref table.
func g14PDF(text string) []byte {
	stream := "BT /F1 18 Tf 50 700 Td (" + text + ") Tj ET"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func g14Rand(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func g14Roles(ms []any) string {
	var r []string
	for _, m := range ms {
		r = append(r, fmt.Sprint(m.(map[string]any)["role"]))
	}
	return strings.Join(r, ",")
}

func TestG14Documents(t *testing.T) {
	rep := newReport(t, "G14-documents", "G14 文档块、Codex 输入项与二进制工具输出（issue #11 补充需求）",
		"假上游记录网关转发的 Chat Completions 请求。覆盖 `/v1/messages` 的 `document` 块与 `tool_result` 图片、`/v1/responses` 的 `input_file`、`custom` 工具与 Codex 输入项、assistant 文本与工具调用合并，以及所有协议共用的二进制工具输出拦截。")
	fake := startFake(t, "ds")
	g := startGateway(t, fmt.Sprintf(g14Config, fake.URL), nil)
	createOut := g.CLI(t, "apikey", "create", "--name", "doc-key", "--json")
	var keyRes struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(createOut[strings.Index(createOut, "{"):]), &keyRes); err != nil {
		t.Fatalf("apikey create: %s", createOut)
	}
	antHdr := map[string]string{"x-api-key": keyRes.Key}
	oaiHdr := map[string]string{"Authorization": "Bearer " + keyRes.Key}

	docx := base64.StdEncoding.EncodeToString(g14Docx())
	pdfB64 := base64.StdEncoding.EncodeToString(g14PDF(g14PDFText))
	badPDF := base64.StdEncoding.EncodeToString(append([]byte("%PDF-1.7\n"), g14Rand(7, 30<<10)...))
	png := base64.StdEncoding.EncodeToString(g14Rand(8, 6<<10))
	lastUp := func() (map[string]any, string) {
		reqs := fake.Requests()
		raw := reqs[len(reqs)-1]
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return m, string(raw)
	}

	// --- Anthropic /v1/messages ---
	doc := func(mt, data, title string) map[string]any {
		return map[string]any{"type": "document", "title": title, "source": map[string]any{"type": "base64", "media_type": mt, "data": data}}
	}
	rA := postJSON(t, g.URL+"/messages", antHdr, map[string]any{
		"model": "claude-sonnet-4-5", "max_tokens": 256,
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				doc("application/vnd.openxmlformats-officedocument.wordprocessingml.document", docx, "报告.docx"),
				doc("application/pdf", pdfB64, "report.pdf"),
				doc("application/pdf", badPDF, "scan.pdf"),
				map[string]any{"type": "text", "text": "总结这几个文件"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "tu_1", "name": "shot", "input": map[string]any{}},
				map[string]any{"type": "tool_use", "id": "tu_2", "name": "read", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": []any{
					map[string]any{"type": "text", "text": "截图完成"},
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}},
				}},
				map[string]any{"type": "tool_result", "tool_use_id": "tu_2", "content": []any{doc("application/pdf", pdfB64, "tool.pdf")}},
				map[string]any{"type": "text", "text": "继续"},
			}},
		},
	})
	upA, rawA := lastUp()
	msgsA, _ := upA["messages"].([]any)
	first, _ := msgsA[0].(map[string]any)["content"].([]any)
	var firstText strings.Builder
	for _, p := range first {
		firstText.WriteString(fmt.Sprint(p.(map[string]any)["text"]) + "\n")
	}
	ft := firstText.String()
	rep.Check("`/v1/messages` document：docx 解出中文正文、PDF 解出文本、无法解析的 PDF 换成提示",
		rA.Status == 200 && strings.Contains(ft, g14DocxText) && strings.Contains(ft, "第二段 结束") && strings.Contains(ft, g14PDFText) &&
			strings.Contains(ft, "未解析的二进制文档（scan.pdf"),
		"status=%d text=%.300q", rA.Status, ft)
	rep.Check("`/v1/messages` 不向上游透传文档原始 base64",
		!strings.Contains(rawA, docx[:200]) && !strings.Contains(rawA, pdfB64[:120]) && !strings.Contains(rawA, badPDF[:200]),
		"docx=%v pdf=%v bad=%v", strings.Contains(rawA, docx[:200]), strings.Contains(rawA, pdfB64[:120]), strings.Contains(rawA, badPDF[:200]))
	rolesA := g14Roles(msgsA)
	toolA1, _ := msgsA[2].(map[string]any)["content"].(string)
	toolA2, _ := msgsA[3].(map[string]any)["content"].(string)
	carrierA, _ := msgsA[4].(map[string]any)["content"].([]any)
	carrierImg := ""
	for _, p := range carrierA {
		if po := p.(map[string]any); po["type"] == "image_url" {
			carrierImg = fmt.Sprint(po["image_url"].(map[string]any)["url"])
		}
	}
	rep.Check("`/v1/messages` tool_result 图片拆到该批 tool 之后的 user 消息，tool_result 里的 PDF 转为文本",
		rolesA == "user,assistant,tool,tool,user,user" && toolA1 == "截图完成" && strings.Contains(toolA2, g14PDFText) &&
			carrierImg == "data:image/png;base64,"+png,
		"roles=%s tool1=%q tool2=%.80q image_ok=%v", rolesA, toolA1, toolA2, carrierImg == "data:image/png;base64,"+png)

	// --- OpenAI /v1/responses ---
	call := func(id, name string) map[string]any {
		return map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": "{}"}
	}
	rR := postJSON(t, g.URL+"/responses", oaiHdr, map[string]any{
		"model": "gpt-5",
		"input": []any{
			map[string]any{"type": "compaction", "summary": []any{map[string]any{"type": "summary_text", "text": "之前讨论了季度报告"}}, "encrypted_content": "opaque"},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_file", "filename": "报告.docx", "file_data": "data:application/vnd.openxmlformats-officedocument.wordprocessingml.document;base64," + docx},
				map[string]any{"type": "input_text", "text": "读一下"},
			}},
			map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed", "action": map[string]any{"type": "search", "query": "q"}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "我先读文件再改补丁"}}},
			call("call_r", "read"),
			map[string]any{"type": "custom_tool_call", "call_id": "call_p", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch"},
			map[string]any{"type": "local_shell_call", "call_id": "call_s", "status": "completed", "action": map[string]any{"type": "exec", "command": []any{"ls"}}},
			map[string]any{"type": "function_call_output", "call_id": "call_r", "output": []any{
				map[string]any{"type": "input_file", "filename": "tool.pdf", "file_data": "data:application/pdf;base64," + pdfB64},
			}},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_p", "output": `[{"type":"input_file","filename":"patch.pdf","file_data":"data:application/pdf;base64,` + pdfB64 + `"}]`},
			map[string]any{"type": "local_shell_call_output", "call_id": "call_s", "output": "README.md"},
			map[string]any{"type": "item_reference", "id": "msg_old"},
			map[string]any{"type": "message", "role": "user", "content": "继续"},
		},
		"tools": []any{
			map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch.", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: begin_patch"}},
		},
	})
	upR, rawR := lastUp()
	msgsR, _ := upR["messages"].([]any)
	rolesR := g14Roles(msgsR)
	rep.Check("`/v1/responses` 含 compaction/web_search_call/item_reference/custom_tool_call/local_shell_call 不再 400，跳过项写入 `X-Gatehouse-Dropped-Items`",
		rR.Status == 200 && rR.Header.Get("X-Gatehouse-Dropped-Items") == "web_search_call, item_reference",
		"status=%d dropped_items=%q body=%.150s", rR.Status, rR.Header.Get("X-Gatehouse-Dropped-Items"), rR.Raw)
	asst, _ := msgsR[2].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	var tcNames, tcArgs []string
	for _, tc := range tcs {
		fn := tc.(map[string]any)["function"].(map[string]any)
		tcNames = append(tcNames, fmt.Sprint(fn["name"]))
		tcArgs = append(tcArgs, fmt.Sprint(fn["arguments"]))
	}
	rep.Check("assistant 文本与随后的工具调用合并为一条 assistant 消息；custom/local_shell 调用映射为 tool_calls",
		rolesR == "user,user,assistant,tool,tool,tool,user" && asst["content"] == "我先读文件再改补丁" &&
			strings.Join(tcNames, ",") == "read,apply_patch,local_shell" && strings.Contains(tcArgs[1], `"input":"*** Begin Patch`),
		"roles=%s content=%v calls=%s args=%q", rolesR, asst["content"], strings.Join(tcNames, ","), tcArgs)
	comp, _ := msgsR[0].(map[string]any)["content"].(string)
	userR, _ := msgsR[1].(map[string]any)["content"].([]any)
	var userRText strings.Builder
	for _, p := range userR {
		userRText.WriteString(fmt.Sprint(p.(map[string]any)["text"]))
	}
	toolR1, _ := msgsR[3].(map[string]any)["content"].(string)
	toolR2, _ := msgsR[4].(map[string]any)["content"].(string)
	rep.Check("compaction 摘要保留为文本；input_file（消息与工具输出）解析为文本且无原始 base64；JSON 字符串形式的 input_file 工具输出也转为文本",
		strings.Contains(comp, "之前讨论了季度报告") && strings.Contains(userRText.String(), g14DocxText) &&
			strings.Contains(toolR1, g14PDFText) && strings.Contains(toolR2, "[文档：patch.pdf]") && strings.Contains(toolR2, g14PDFText) && !strings.Contains(rawR, docx[:200]) && !strings.Contains(rawR, pdfB64[:120]),
		"compaction=%q user=%.80q tool_read=%.60q tool_patch=%q", comp, userRText.String(), toolR1, toolR2)
	var fnTool map[string]any
	upTools, _ := upR["tools"].([]any)
	for _, tl := range upTools {
		if fn := tl.(map[string]any)["function"].(map[string]any); fn["name"] == "apply_patch" {
			fnTool = fn
		}
	}
	params, _ := fnTool["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	rep.Check("`custom` 工具转为单参数 `input` 的 function，描述带上 grammar",
		fnTool != nil && props["input"] != nil && strings.Contains(fmt.Sprint(fnTool["description"]), "start: begin_patch") &&
			rR.Header.Get("X-Gatehouse-Dropped-Tools") == "",
		"function=%v dropped_tools=%q", fnTool != nil, rR.Header.Get("X-Gatehouse-Dropped-Tools"))

	// the model calls the custom tool: the client gets a custom_tool_call back
	customReq := map[string]any{
		"model": "gpt-5", "input": "改一下 README",
		"tools": []any{map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch."}},
	}
	rC := postJSON(t, g.URL+"/responses", oaiHdr, customReq)
	var gotItem map[string]any
	outs, _ := rC.Body["output"].([]any)
	for _, o := range outs {
		if om := o.(map[string]any); om["name"] == "apply_patch" {
			gotItem = om
		}
	}
	rep.Check("非流式：对 custom 工具的调用还原为 `custom_tool_call`，input 为原始文本",
		rC.Status == 200 && gotItem["type"] == "custom_tool_call" && gotItem["input"] == "改一下 README" && gotItem["arguments"] == nil,
		"status=%d item=%v", rC.Status, gotItem)
	customReq["stream"] = true
	rS := postJSON(t, g.URL+"/responses", oaiHdr, customReq)
	var evNames []string
	var doneItem map[string]any
	inputDone := ""
	for _, ev := range parseSSE(rS.Raw) {
		evNames = append(evNames, ev.Event)
		switch ev.Event {
		case "response.custom_tool_call_input.done":
			inputDone = fmt.Sprint(ev.Data["input"])
		case "response.output_item.done":
			if it, _ := ev.Data["item"].(map[string]any); it["name"] == "apply_patch" {
				doneItem = it
			}
		}
	}
	joined := strings.Join(evNames, " ")
	rep.Check("流式：custom 工具发 `custom_tool_call_input.delta/done`，不发 `function_call_arguments.*`",
		rS.Status == 200 && inputDone == "改一下 README" && doneItem["type"] == "custom_tool_call" &&
			strings.Contains(joined, "response.custom_tool_call_input.delta") && !strings.Contains(joined, "function_call_arguments"),
		"status=%d input=%q item_type=%v", rS.Status, inputDone, doneItem["type"])

	// the name arrives after the id and part of the arguments
	lateReq := map[string]any{
		"model": "gpt-5", "input": "改一下 #late-name", "stream": true,
		"tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}},
	}
	lateEvents := func(r Response) (types []string, input string, joined string) {
		var names []string
		for _, ev := range parseSSE(r.Raw) {
			names = append(names, ev.Event)
			it, _ := ev.Data["item"].(map[string]any)
			if ev.Event == "response.output_item.added" || ev.Event == "response.output_item.done" {
				if it["call_id"] != nil {
					types = append(types, fmt.Sprint(it["type"]))
				}
			}
			if ev.Event == "response.custom_tool_call_input.done" {
				input = fmt.Sprint(ev.Data["input"])
			}
		}
		return types, input, strings.Join(names, " ")
	}
	rL := postJSON(t, g.URL+"/responses", oaiHdr, lateReq)
	lTypes, lInput, lJoined := lateEvents(rL)
	lateReq["tools"] = []any{map[string]any{"type": "function", "name": "apply_patch", "parameters": map[string]any{"type": "object"}}}
	rLF := postJSON(t, g.URL+"/responses", oaiHdr, lateReq)
	fTypes, _, fJoined := lateEvents(rLF)
	var fArgs string
	for _, ev := range parseSSE(rLF.Raw) {
		if ev.Event == "response.function_call_arguments.delta" {
			fArgs += fmt.Sprint(ev.Data["delta"])
		}
	}
	rep.Check("流式：工具名晚于 id 和部分参数到达时，仍按名称分类（custom 与 function 各自事件完整一致）",
		rL.Status == 200 && strings.Join(lTypes, ",") == "custom_tool_call,custom_tool_call" && lInput == "改一下 #late-name" &&
			!strings.Contains(lJoined, "function_call_arguments") &&
			strings.Join(fTypes, ",") == "function_call,function_call" && fArgs == `{"input":"改一下 #late-name"}` &&
			!strings.Contains(fJoined, "custom_tool_call_input"),
		"custom_items=%v custom_input=%q function_items=%v function_args=%q", lTypes, lInput, fTypes, fArgs)

	// --- binary tool output guard (all protocols) ---
	garbage := string(g14Rand(9, 24<<10))                                                                   // invalid UTF-8 → U+FFFD after JSON
	zipText := "PK\x03\x04" + strings.Repeat("word/document.xml docProps ", 400) + string(g14Rand(10, 300)) // mostly printable
	var normal strings.Builder
	for normal.Len() < 24<<10 {
		normal.WriteString("2026-11-23 10:00:01 \x1b[32mINFO\x1b[0m 处理请求 id=42 耗时 12ms\nfunc main() {\n\tfmt.Println(\"你好\")\n}\n")
	}
	rB := g.Chat(t, keyRes.Key, map[string]any{"model": "ds/deepseek-chat", "messages": []any{
		map[string]any{"role": "user", "content": "读文件"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}},
			map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}},
			map[string]any{"id": "c3", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": garbage},
		map[string]any{"role": "tool", "tool_call_id": "c2", "content": zipText},
		map[string]any{"role": "tool", "tool_call_id": "c3", "content": normal.String()},
	}}, nil)
	upB, _ := lastUp()
	msgsB, _ := upB["messages"].([]any)
	tb1, _ := msgsB[2].(map[string]any)["content"].(string)
	tb2, _ := msgsB[3].(map[string]any)["content"].(string)
	tb3, _ := msgsB[4].(map[string]any)["content"].(string)
	rep.Check("乱码（U+FFFD 占比高）工具输出被替换为网关提示",
		rB.Status == 200 && strings.HasPrefix(tb1, "[网关保护") && len(tb1) < 1200, "status=%d len=%d head=%.60q", rB.Status, len(tb1), tb1)
	rep.Check("`PK\\x03\\x04` 开头且后接非可打印流的工具输出被替换", strings.HasPrefix(tb2, "[网关保护"), "head=%.60q", tb2)
	rep.Check("正常中文/代码/彩色日志（24 KiB）不受影响，响应头只计 2 条",
		tb3 == normal.String() && rB.Header.Get("X-Gatehouse-Binary-Tool-Output") == "2",
		"normal_intact=%v header=%q", tb3 == normal.String(), rB.Header.Get("X-Gatehouse-Binary-Tool-Output"))
}
