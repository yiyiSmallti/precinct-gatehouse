package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const g2Config = `
listen: {{LISTEN}}
mode: personal
providers:
  - name: local
    base_url: %s
    target: local
    models: [small]
  - name: cloud
    base_url: %s
    target: external
    models: [strong]
redaction:
  mode_by_target: { local: audit, external: mask }
  categories: { phone: mask, id_card: mask, bank_card: mask, term: mask, email: mask, secret: block }
  dictionary: {{DIR}}/words.txt
  terms: [""]   # an empty inline term must be ignored, not loop forever
  never_mask: ["team@example.com"]
`

const (
	g2Phone   = "13812345678"
	g2ID      = "110101199003071233"
	g2Card    = "6222021234567890128"
	g2Term    = "星辰科技"
	g2Order   = "13900001111"
	g2Never   = "team@example.com"
	g2Message = "请给星辰科技的联系人回电 " + g2Phone + "，身份证 " + g2ID + "，卡号 " + g2Card + "。订单号 " + g2Order + " 已发货。抄送 " + g2Never + "。"
)

func TestG2Redaction(t *testing.T) {
	rep := newReport(t, "G2-redaction", "G2 脱敏",
		"外部目标 `cloud/strong` 模式为 mask，本地目标为 audit。请求含手机号、身份证、银行卡、客户词典词「星辰科技」、订单号干扰项（11 位，形似手机号）、Never-mask 地址。断言外部假服务收到的请求体、回填后的响应与工具参数、审计文件。")
	local := startFake(t, "local")
	cloud := startFake(t, "cloud")
	g := startGateway(t, fmt.Sprintf(g2Config, local.URL, cloud.URL), map[string]string{"words.txt": "# 客户词典\n" + g2Term + "\n"})

	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "echo", "description": "把内容原样发给 " + g2Phone, "parameters": map[string]any{"type": "object",
		"properties": map[string]any{"to": map[string]any{"type": "string", "description": "收件号码，例如 " + g2Phone}}}}}}
	r := g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("system", "你是助理。", "user", g2Message), "tools": tools,
		"dsh_session_log": "完整会话日志 " + g2Phone}, map[string]string{"X-Gatehouse-Session": "s1"})
	if r.Status != 200 {
		t.Fatalf("status %d: %s", r.Status, r.Raw)
	}
	sent := cloud.Requests()
	out := string(sent[len(sent)-1])
	for _, nv := range [][2]string{{"手机号", g2Phone}, {"身份证", g2ID}, {"银行卡", g2Card}, {"词典词", g2Term}} {
		rep.Check("出站不含"+nv[0], !strings.Contains(out, nv[1]), "外部假服务收到的请求体中 %s 出现 %d 次", nv[0], strings.Count(out, nv[1]))
	}
	phs := placeholderRe.FindAllString(out, -1)
	kinds := map[string]bool{}
	for _, p := range phs {
		kinds[p[2:strings.LastIndex(p, "_")]] = true
	}
	rep.Check("出站带占位符", kinds["PHONE"] && kinds["ID_CARD"] && kinds["BANK_CARD"] && kinds["TERM"], "占位符 %s", strings.Join(uniq(phs), " "))
	rep.Check("订单号未被误脱", strings.Contains(out, "订单号 "+g2Order), "订单号后的 11 位数字原样发出")
	rep.Check("dsh_session_log 被剥掉", !strings.Contains(out, "dsh_session_log"), "该字段带完整明文会话日志，出站前删除")
	rep.Check("Never-mask 原样", strings.Contains(out, g2Never), "%s 原样发出", g2Never)
	rep.Check("工具描述与参数 schema 描述也脱敏", !strings.Contains(out, "原样发给 "+g2Phone) && !strings.Contains(out, "例如 "+g2Phone) && strings.Contains(out, `"to":`),
		"function.description 与 parameters.properties.*.description 中的手机号已替换，属性名保留")

	rep.Check("响应文本回填为原值", strings.Contains(r.Content(), g2Message), "客户端收到：%s", r.Content())
	var args map[string]string
	jsonErr := json.Unmarshal([]byte(r.ToolArgs()), &args)
	rep.Check("tool_calls 参数回填且仍是合法 JSON", jsonErr == nil && args["echo"] == g2Message, "arguments=%s", r.ToolArgs())
	rep.Check("响应头说明脱敏", strings.HasPrefix(header(r, "X-Gatehouse-Redaction"), "mask;"), "X-Gatehouse-Redaction: %s", header(r, "X-Gatehouse-Redaction"))

	// same value, next turn: same placeholder
	r2 := g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", g2Message, "assistant", "好的", "user", "再确认一次号码 "+g2Phone)}, map[string]string{"X-Gatehouse-Session": "s1"})
	sent = cloud.Requests()
	out2 := string(sent[len(sent)-1])
	phone1 := firstOf(phs, "{{PHONE_")
	rep.Check("跨回合同一值同一占位符", r2.Status == 200 && phone1 != "" && strings.Count(out2, phone1) >= 2, "%s 在第二回合出现 %d 次", phone1, strings.Count(out2, phone1))

	// tool-call arguments in history: JSON-escaped values and numbers
	hist := []any{
		map[string]any{"role": "user", "content": "查一下客户"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "t1", "type": "function",
			"function": map[string]any{"name": "lookup", "arguments": `{"company":"\u661f\u8fb0\u79d1\u6280","phone":13812345678}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "t1", "content": "已找到"},
	}
	rh := g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": hist}, nil)
	sent = cloud.Requests()
	var outReq struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct{ Arguments string } `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	json.Unmarshal(sent[len(sent)-1], &outReq)
	histArgs := ""
	for _, m := range outReq.Messages {
		for _, tc := range m.ToolCalls {
			histArgs = tc.Function.Arguments
		}
	}
	var parsed map[string]any
	rep.Check("历史工具参数按 JSON 解码后脱敏", rh.Status == 200 && json.Unmarshal([]byte(histArgs), &parsed) == nil &&
		!strings.Contains(histArgs, "13812345678") && !strings.Contains(histArgs, `\u661f`) && strings.Contains(histArgs, "{{TERM_") && strings.Contains(histArgs, "{{PHONE_"),
		"出站 arguments=%s", histArgs)

	// a contextual secret inside tool arguments is still seen with its key
	n0 := len(cloud.Requests())
	rs := g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": []any{
		map[string]any{"role": "user", "content": "登录"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "t2", "type": "function",
			"function": map[string]any{"name": "login", "arguments": `{"user":"ops","password":"hunter2hunter2"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "t2", "content": "ok"},
	}}, nil)
	rep.Check("工具参数里带键名的密码仍按 secret 处理", rs.Status == 400 && rs.ErrCode() == "redaction_blocked" && len(cloud.Requests()) == n0,
		"status=%d code=%s", rs.Status, rs.ErrCode())

	// local target in audit mode: unchanged, but counted
	r3 := g.Chat(t, g.Token, map[string]any{"model": "local/small", "messages": msgs("user", g2Message)}, nil)
	ls := local.Requests()
	rep.Check("本地目标 audit：原样发出、只计数", r3.Status == 200 && strings.Contains(string(ls[len(ls)-1]), g2Phone) && strings.HasPrefix(header(r3, "X-Gatehouse-Redaction"), "audit;"),
		"X-Gatehouse-Redaction: %s", header(r3, "X-Gatehouse-Redaction"))

	// secret -> block, nothing sent
	n := len(cloud.Requests())
	r4 := g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "我的 key 是 sk-proj-abcdefghijklmnopqrstuvwxyz123456")}, nil)
	rep.Check("密钥类别 block → 400 且不出站", r4.Status == 400 && r4.ErrCode() == "redaction_blocked" && len(cloud.Requests()) == n, "status=%d code=%s message=%v", r4.Status, r4.ErrCode(), r4.Body["error"])

	// confidential data only goes to local models
	r5 := g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "机密")}, map[string]string{"X-Gatehouse-Data-Class": "confidential"})
	rep.Check("confidential 不出外部（拒绝而不降级）", r5.Status == 403 && len(cloud.Requests()) == n, "status=%d code=%s", r5.Status, r5.ErrCode())

	// audit holds category counts only
	recs, raw := g.AuditRecords(t)
	leaked := []string{}
	for _, v := range []string{g2Phone, g2ID, g2Card, g2Term, "sk-proj-abcdefghij"} {
		if strings.Contains(raw, v) {
			leaked = append(leaked, v)
		}
	}
	rep.Check("审计只含类别计数，无原文", len(leaked) == 0 && !strings.Contains(raw, "{{"), "审计文件 %d 条，泄漏值 %v，含占位符=%v", len(recs), leaked, strings.Contains(raw, "{{"))
	if len(recs) > 0 {
		b, _ := json.Marshal(recs[0]["redaction"])
		rep.Note("首条审计记录的 redaction 字段：\n\n```json\n%s\n```", b)
	}
}

func uniq(s []string) []string {
	m := map[string]bool{}
	var out []string
	for _, x := range s {
		if !m[x] {
			m[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func firstOf(s []string, prefix string) string {
	for _, x := range s {
		if strings.HasPrefix(x, prefix) {
			return x
		}
	}
	return ""
}

// TestRedactionFixtures runs every sample in fixtures/redaction/samples.tsv
// through `gatehouse redact try` and reports per-sample results: the
// repeatable evidence for recall and false positives of the rule pack.
func TestRedactionFixtures(t *testing.T) {
	rep := newReport(t, "redaction-fixtures", "脱敏规则包样本集",
		"`fixtures/redaction/samples.tsv` 每行：期望类别（逗号分隔，`-` 表示不应检出）、文本。经 `gatehouse redact try --json` 检测，比较检出类别与期望。")
	dir := t.TempDir()
	cfg := "mode: personal\nproviders:\n  - name: x\n    base_url: https://example.com/v1\nredaction:\n  categories: { secret: mask }\n  terms: [星辰科技]\n  never_mask: [team@example.com]\n"
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600)
	f, err := os.Open("fixtures/redaction/samples.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	tp, fn, fp := 0, 0, 0
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		want, text, _ := strings.Cut(line, "\t")
		cmd := exec.Command(gatehouse, "redact", "try", "--json", "--config", filepath.Join(dir, "config.yaml"), text)
		b, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		var res struct {
			Report struct {
				Samples []struct{ Kind string } `json:"samples"`
			} `json:"report"`
		}
		json.Unmarshal(b, &res)
		got := map[string]bool{}
		for _, s := range res.Report.Samples {
			got[strings.ToLower(s.Kind)] = true
		}
		wantSet := map[string]bool{}
		if want != "-" {
			for _, k := range strings.Split(want, ",") {
				wantSet[k] = true
			}
		}
		var miss, extra []string
		for k := range wantSet {
			if got[k] {
				tp++
			} else {
				miss = append(miss, k)
				fn++
			}
		}
		for k := range got {
			if !wantSet[k] {
				extra = append(extra, k)
				fp++
			}
		}
		gotList := keys(got)
		rep.Check(text, len(miss) == 0 && len(extra) == 0, "期望 %s，检出 %s%s%s", want, strings.Join(gotList, ","), cond(len(miss) > 0, "；漏检 "+strings.Join(miss, ",")), cond(len(extra) > 0, "；误报 "+strings.Join(extra, ",")))
	}
	rep.Note("合计：命中 %d，漏检 %d，误报 %d。", tp, fn, fp)
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"-"}
	}
	return out
}

func cond(ok bool, s string) string {
	if ok {
		return s
	}
	return ""
}
