package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const g4Config = `
listen: {{LISTEN}}
mode: personal
providers:
  - name: p
    base_url: %s
    target: local
    models: [model-a, model-b]
default_group: g
routing_groups:
  g:
    members: [p/model-a]
`

func TestG4Tokens(t *testing.T) {
	rep := newReport(t, "G4-tokens", "G4 令牌",
		"`gatehouse token issue` 签发限范围的短时效令牌，网关按令牌校验签名、有效期、吊销与范围。")
	fake := startFake(t, "p")
	g := startGateway(t, fmt.Sprintf(g4Config, fake.URL), nil)
	ask := func(tok, model string) Response {
		return g.Chat(t, tok, map[string]any{"model": model, "messages": msgs("user", "hi")}, nil)
	}
	issue := func(args ...string) string {
		out := g.CLI(t, append([]string{"token", "issue", "--json"}, args...)...)
		var v struct{ Token string }
		if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &v); err != nil {
			t.Fatalf("issue output: %s", out)
		}
		return v.Token
	}

	rep.Check("本机令牌零配置可用", ask(g.Token, "p/model-a").Status == 200, "首次 serve 自动签发 local.token")

	tokA := issue("--name", "only-a", "--models", "p/model-a", "--ttl", "1h")
	r := ask(tokA, "p/model-a")
	rep.Check("限定模型 A：请求 A → 200", r.Status == 200, "status=%d", r.Status)
	n := len(fake.Requests())
	r = ask(tokA, "p/model-b")
	rep.Check("限定模型 A：请求 B → 403", r.Status == 403 && r.ErrCode() == "model_not_allowed" && len(fake.Requests()) == n, "status=%d code=%s，上游未收到", r.Status, r.ErrCode())
	r = ask(tokA, "g")
	rep.Check("限定模型 A：请求未授权的路由组 → 403", r.Status == 403, "status=%d code=%s", r.Status, r.ErrCode())

	tokG := issue("--name", "group-g", "--groups", "g", "--ttl", "1h")
	rep.Check("限定路由组：请求组 → 200", ask(tokG, "g").Status == 200, "")
	rep.Check("限定路由组：直连模型 → 403", ask(tokG, "p/model-b").Status == 403, "")

	tokShort := issue("--name", "short", "--ttl", "1s")
	time.Sleep(1100 * time.Millisecond)
	r = ask(tokShort, "p/model-a")
	rep.Check("过期 → 401", r.Status == 401 && r.ErrCode() == "token_expired", "status=%d code=%s", r.Status, r.ErrCode())

	tokRev := issue("--name", "to-revoke", "--ttl", "1h")
	rep.Check("吊销前可用", ask(tokRev, "p/model-a").Status == 200, "")
	g.CLI(t, "token", "revoke", "to-revoke")
	r = ask(tokRev, "p/model-a")
	rep.Check("吊销后 → 401", r.Status == 401 && r.ErrCode() == "token_revoked", "status=%d code=%s", r.Status, r.ErrCode())

	r = ask("", "p/model-a")
	rep.Check("无令牌 → 401", r.Status == 401 && r.ErrCode() == "missing_token", "status=%d code=%s", r.Status, r.ErrCode())
	forged := tokA[:len(tokA)-4] + "AAAA"
	r = ask(forged, "p/model-a")
	rep.Check("篡改签名 → 401", r.Status == 401, "status=%d code=%s", r.Status, r.ErrCode())

	tokBudget := issue("--name", "budget", "--max-tokens", "5", "--ttl", "1h")
	first := ask(tokBudget, "p/model-a")
	second := ask(tokBudget, "p/model-a")
	rep.Check("token 上限用尽 → 403", first.Status == 200 && second.Status == 403 && second.ErrCode() == "token_budget_exceeded", "第一次 %d，第二次 %d %s", first.Status, second.Status, second.ErrCode())

	r = g.Chat(t, tokA, map[string]any{"model": "p/model-a", "messages": msgs("user", "x")}, map[string]string{"X-Gatehouse-Data-Class": "confidential"})
	rep.Check("数据分级越权 → 403", r.Status == 403 && r.ErrCode() == "data_class_not_allowed", "令牌上限 internal，请求 confidential：status=%d", r.Status)

	recs, _ := g.AuditRecords(t)
	denied := 0
	for _, rec := range recs {
		if s, _ := rec["status"].(float64); s == 401 || s == 403 {
			denied++
		}
	}
	rep.Check("拒绝均有审计记录", denied >= 8, "审计中 401/403 共 %d 条", denied)
	rep.Note("令牌列表：\n\n```\n%s```", g.CLI(t, "token", "list"))
}
