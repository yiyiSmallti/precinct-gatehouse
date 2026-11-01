package e2e

import (
	"fmt"
	"strings"
	"testing"
)

const g1Config = `
listen: {{LISTEN}}
mode: personal
providers:
  - name: local
    base_url: %s
    target: local
    models: [small, backup, a, b]
  - name: cloud
    base_url: %s
    target: external
    models: [strong]
    pricing: { strong: { input: 2, output: 8 } }
default_group: default
routing_groups:
  default:
    type: order
    members: [cloud/strong, local/backup]
    classifier: local/small
    classifier_timeout: 2s
    rules:
      - name: chat
        intent: "闲聊、问候、简单问答"
        use: local/small
        useEffort: low
      - name: long-input
        tokens_gt: 3000
        use: cloud/strong
        useEffort: high
      - name: reasoning
        intent: "多步推理或写长文"
        use: cloud/strong
        useEffort: medium
  pool:
    type: usage
    members: [local/a, local/b]
redaction:
  mode_by_target: { local: audit, external: mask }
`

func TestG1Routing(t *testing.T) {
	rep := newReport(t, "G1-routing", "G1 个人模式路由",
		"两个假提供方：`local`（本地目标，托管分类器 small、备用 backup）与 `cloud`（外部目标，强模型 strong）。默认组 `order: [cloud/strong, local/backup]`，三条规则：闲聊意图 → local/small，估算 token > 3000 → cloud/strong，多步推理/长文意图 → cloud/strong。")
	local := startFake(t, "local")
	cloud := startFake(t, "cloud")
	local.Classify = func(u string) string {
		switch {
		case strings.Contains(u, "你好") || strings.Contains(u, "天气"):
			return "1"
		case strings.Contains(u, "分析") || strings.Contains(u, "长文"):
			return "2"
		}
		return "0"
	}
	g := startGateway(t, fmt.Sprintf(g1Config, local.URL, cloud.URL), nil)

	// 1. chit-chat -> small model
	r := g.Chat(t, g.Token, map[string]any{"model": "auto", "messages": msgs("user", "你好，今天天气怎么样？")}, nil)
	rep.Check("闲聊 → 小模型", r.Status == 200 && header(r, "X-Gatehouse-Model") == "local/small" && header(r, "X-Gatehouse-Rule") == "chat",
		"status=%d model=%s rule=%s effort=%s", r.Status, header(r, "X-Gatehouse-Model"), header(r, "X-Gatehouse-Rule"), header(r, "X-Gatehouse-Effort"))
	last := local.ChatRequests()
	rep.Check("useEffort 下发为 reasoning_effort", len(last) > 0 && last[len(last)-1]["reasoning_effort"] == "low",
		"local/small 收到 reasoning_effort=%v", last[len(last)-1]["reasoning_effort"])

	// 2. long multi-step request by intent -> strong model
	r = g.Chat(t, g.Token, map[string]any{"model": "auto", "messages": msgs("user", "请分析这份季度报告并写一篇长文总结")}, nil)
	rep.Check("长文/推理意图 → 强模型", r.Status == 200 && header(r, "X-Gatehouse-Model") == "cloud/strong" && header(r, "X-Gatehouse-Rule") == "reasoning",
		"status=%d model=%s rule=%s effort=%s", r.Status, header(r, "X-Gatehouse-Model"), header(r, "X-Gatehouse-Rule"), header(r, "X-Gatehouse-Effort"))

	// 3. very long input -> token rule, before any classifier call
	before := local.ClassifierCalls()
	long := strings.Repeat("这是一段很长的材料。", 400)
	r = g.Chat(t, g.Token, map[string]any{"model": "auto", "messages": msgs("user", long)}, nil)
	rep.Check("长输入（token 规则）→ 强模型", r.Status == 200 && header(r, "X-Gatehouse-Model") == "cloud/strong" && header(r, "X-Gatehouse-Rule") == "long-input",
		"status=%d model=%s rule=%s", r.Status, header(r, "X-Gatehouse-Model"), header(r, "X-Gatehouse-Rule"))
	rep.Check("长输入：分类器最多调用一次", local.ClassifierCalls() <= before+1,
		"分类器调用 %d → %d（闲聊意图规则排在 token 规则之前，所以会问一次分类器）", before, local.ClassifierCalls())

	// 4. same turn: tool round trip reuses the decision, no new classification
	turn := []any{
		map[string]any{"role": "user", "content": "请分析这份季度报告并写一篇长文总结"},
	}
	r1 := g.Chat(t, g.Token, map[string]any{"model": "auto", "messages": turn}, map[string]string{"X-Gatehouse-Session": "s-turn"})
	calls := local.ClassifierCalls()
	turn = append(turn,
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "t1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "t1", "content": "报告内容……"})
	r2 := g.Chat(t, g.Token, map[string]any{"model": "auto", "messages": turn}, map[string]string{"X-Gatehouse-Session": "s-turn"})
	rep.Check("同一回合决策固定", r2.Status == 200 && header(r2, "X-Gatehouse-Model") == header(r1, "X-Gatehouse-Model") && local.ClassifierCalls() == calls,
		"第一次 %s，工具回合后 %s；分类器调用 %d → %d", header(r1, "X-Gatehouse-Model"), header(r2, "X-Gatehouse-Model"), calls, local.ClassifierCalls())

	// 5. strong model down -> order group fails over to backup
	cloud.SetDown(true)
	r = g.Chat(t, g.Token, map[string]any{"model": "auto", "messages": msgs("user", "再分析一下下季度的长文预测")}, nil)
	rep.Check("强模型宕机 → order 组切到备用", r.Status == 200 && header(r, "X-Gatehouse-Model") == "local/backup",
		"status=%d model=%s rule=%s", r.Status, header(r, "X-Gatehouse-Model"), header(r, "X-Gatehouse-Rule"))
	// while cooling down, the next request skips the failed model
	n := len(cloud.Requests())
	r = g.Chat(t, g.Token, map[string]any{"model": "auto", "messages": msgs("user", "第三份长文分析")}, nil)
	rep.Check("冷却期内跳过故障模型", r.Status == 200 && header(r, "X-Gatehouse-Model") == "local/backup" && len(cloud.Requests()) == n,
		"model=%s，强模型在冷却期内收到请求 %d 次", header(r, "X-Gatehouse-Model"), len(cloud.Requests())-n)
	cloud.SetDown(false)

	// 6. usage group alternates by tokens routed
	var seq []string
	for i := 0; i < 4; i++ {
		r = g.Chat(t, g.Token, map[string]any{"model": "pool", "messages": msgs("user", fmt.Sprintf("第 %d 个请求", i))}, nil)
		seq = append(seq, header(r, "X-Gatehouse-Model"))
	}
	rep.Check("usage 组按用量分摊", seq[0] != seq[1] && seq[1] != seq[2] && seq[2] != seq[3], "依次命中 %s", strings.Join(seq, " → "))

	// usage group inside one turn: the tool round trip keeps the first pick
	poolTurn := []any{map[string]any{"role": "user", "content": "同一回合的 usage 组"}}
	p1 := g.Chat(t, g.Token, map[string]any{"model": "pool", "messages": poolTurn}, nil)
	poolTurn = append(poolTurn,
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "t1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "t1", "content": "ok"})
	p2 := g.Chat(t, g.Token, map[string]any{"model": "pool", "messages": poolTurn}, nil)
	rep.Check("usage 组同一回合不换模型", header(p1, "X-Gatehouse-Model") == header(p2, "X-Gatehouse-Model"),
		"%s → %s", header(p1, "X-Gatehouse-Model"), header(p2, "X-Gatehouse-Model"))

	// 7. direct model, streaming passthrough for a local target
	r = g.Chat(t, g.Token, map[string]any{"model": "local/small", "stream": true, "messages": msgs("user", "流式测试")}, nil)
	rep.Check("本地目标流式透传", r.Status == 200 && strings.Contains(string(r.Raw), "data: [DONE]") && strings.Contains(string(r.Raw), "流式测试") && !strings.Contains(string(r.Raw), `"usage"`),
		"Content-Type=%s；客户端未要 usage，网关读取后丢弃 usage 块", r.Header.Get("Content-Type"))

	// 8. external target with a streaming client: buffered upstream, SSE to client
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "stream": true, "messages": msgs("user", "外部流式")}, nil)
	cr := cloud.ChatRequests()
	upstreamStream := cr[len(cr)-1]["stream"]
	rep.Check("外部目标整条缓冲、对客户端仍是 SSE", r.Status == 200 && upstreamStream == nil && strings.Contains(string(r.Raw), "data: [DONE]"),
		"上游请求 stream=%v，客户端收到 SSE", upstreamStream)

	// decision log
	recs, _ := g.AuditRecords(t)
	var b strings.Builder
	b.WriteString("## 决策记录（来自审计文件）\n\n| 请求模型 | 组 | 规则 | 理由 | 最终模型 | 尝试 |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, rec := range recs {
		var tries []string
		if at, ok := rec["attempts"].([]any); ok {
			for _, a := range at {
				am := a.(map[string]any)
				tries = append(tries, fmt.Sprintf("%v:%v", am["model"], am["status"]))
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", str(rec["requested_model"]), str(rec["group"]), str(rec["rule"]), mdEscape(str(rec["rule_reason"])), str(rec["model"]), strings.Join(tries, ", "))
	}
	rep.Note("%s", b.String())
}
