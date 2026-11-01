package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const g5Config = `
listen: {{LISTEN}}
mode: org
providers:
  - name: local
    base_url: %s
    target: local
    models: [a, b]
  - name: cloud
    base_url: %s
    target: external
    models: [strong]
  - name: plug
    plugin: plugprov
    target: local
    models: [x]
default_group: default
routing_groups:
  default:
    type: order
    members: [cloud/strong, local/a, local/b]
  external-only:
    type: order
    members: [cloud/strong]
  ruled:
    type: order
    members: [local/a]
    rules:
      - name: batch
        source: batch
        use: local/b
plugins:
  - name: cloister
    kind: auth_policy
    path: {{FAKEPLUGIN}}
    args: [policy, "{{DIR}}/tokens.json"]
    enabled: %v
  - name: audit-file
    kind: audit_sink
    path: {{FAKEPLUGIN}}
    args: [audit, "{{DIR}}/sink.jsonl"]
  - name: plugprov
    kind: provider
    path: {{FAKEPLUGIN}}
    args: [provider]
  - name: tagger
    kind: hooks
    path: {{FAKEPLUGIN}}
    args: [hooks]
redaction:
  mode_by_target: { local: audit, external: mask }
`

const g5Tokens = `{
  "tok-conf":   {"tenant": "org_acme", "user": "u_zhang", "department": "研发部", "data_class": "confidential", "allowed_groups": ["default", "external-only"], "models": ["plug/x"]},
  "tok-int":    {"tenant": "org_acme", "user": "u_li", "department": "市场部", "data_class": "internal", "allowed_groups": ["default", "ruled"], "models": ["plug/x"]},
  "tok-narrow": {"tenant": "org_acme", "user": "u_wang", "department": "财务部", "data_class": "internal", "allowed_groups": []},
  "tok-broke":  {"tenant": "org_acme", "user": "u_zhao", "department": "研发部", "data_class": "internal", "allowed_groups": ["default"], "tokens_remaining": 0},
  "tok-deny":   {"action": "deny"},
  "tok-forbid": {"action": "forbid"},
  "tok-crash":  {"action": "crash"}
}`

func TestG5OrgMode(t *testing.T) {
	rep := newReport(t, "G5-org-mode", "G5 org 模式",
		"`mode: org`，策略插件 `cloister` 用 e2e/fakeplugin 的假实现（按令牌返回租户、用户、部门与分级）。默认组 `order: [cloud/strong, local/a, local/b]`，cloud 是外部目标。同时挂上 AuditSink 插件（写文件）、Provider 插件（`plug/x`）和 Hooks 插件（在最后一条用户消息后加 `[hooked:<部门>]`）。")
	local := startFake(t, "local")
	cloud := startFake(t, "cloud")
	g := startGateway(t, fmt.Sprintf(g5Config, local.URL, cloud.URL, true), map[string]string{"tokens.json": g5Tokens})

	// 1. confidential department -> local only
	n := len(cloud.Requests())
	r := g.Chat(t, "tok-conf", map[string]any{"model": "auto", "messages": msgs("user", "季度预算明细")}, nil)
	rep.Check("confidential → 只走本地组", r.Status == 200 && header(r, "X-Gatehouse-Model") == "local/a" && len(cloud.Requests()) == n,
		"status=%d model=%s，外部模型收到 %d 次请求", r.Status, header(r, "X-Gatehouse-Model"), len(cloud.Requests())-n)
	recs, _ := g.AuditRecords(t)
	last := recs[len(recs)-1]
	rep.Check("部门与分级写入审计", last["department"] == "研发部" && last["data_class"] == "confidential" && last["tenant"] == "org_acme" && last["user"] == "u_zhang",
		"tenant=%v user=%v department=%v data_class=%v policy_version=%v", last["tenant"], last["user"], last["department"], last["data_class"], last["policy_version"])
	lr := local.ChatRequests()
	rep.Check("Hooks 插件改写请求（带部门上下文）", strings.Contains(lastUser(lr[len(lr)-1]), "[hooked:研发部]"), "上游收到：%q", lastUser(lr[len(lr)-1]))

	// a client header cannot lower the policy's class
	n = len(cloud.Requests())
	r = g.Chat(t, "tok-conf", map[string]any{"model": "auto", "messages": msgs("user", "试图降级")}, map[string]string{"X-Gatehouse-Data-Class": "public"})
	rep.Check("请求头不能把分级降到策略之下", r.Status == 200 && header(r, "X-Gatehouse-Model") == "local/a" && len(cloud.Requests()) == n,
		"status=%d model=%s", r.Status, header(r, "X-Gatehouse-Model"))

	// confidential on an external-only group -> refused, not sent
	r = g.Chat(t, "tok-conf", map[string]any{"model": "external-only", "messages": msgs("user", "机密")}, nil)
	rep.Check("confidential 指定外部组 → 拒绝", r.Status == 403 && len(cloud.Requests()) == n, "status=%d code=%s", r.Status, errCode(r))

	// 2. internal department may use the external model
	r = g.Chat(t, "tok-int", map[string]any{"model": "auto", "messages": msgs("user", "写一段产品介绍")}, nil)
	rep.Check("internal → 可用外部强模型", r.Status == 200 && header(r, "X-Gatehouse-Model") == "cloud/strong", "status=%d model=%s", r.Status, header(r, "X-Gatehouse-Model"))

	// 3. every local model down -> refuse, never downgrade to external
	local.SetDown(true)
	n = len(cloud.Requests())
	r = g.Chat(t, "tok-conf", map[string]any{"model": "auto", "messages": msgs("user", "本地全挂时的机密请求")}, nil)
	rep.Check("本地组全部宕机 → 拒绝不降级", r.Status == 503 && errCode(r) == "local_targets_unavailable" && len(cloud.Requests()) == n,
		"status=%d code=%s，外部模型收到 %d 次请求", r.Status, errCode(r), len(cloud.Requests())-n)
	local.SetDown(false)
	time.Sleep(10 * time.Millisecond)

	// 4. policy decisions
	r = g.Chat(t, "tok-deny", map[string]any{"model": "auto", "messages": msgs("user", "hi")}, nil)
	rep.Check("策略拒绝令牌 → 401", r.Status == 401, "status=%d code=%s", r.Status, errCode(r))
	r = g.Chat(t, "tok-forbid", map[string]any{"model": "auto", "messages": msgs("user", "hi")}, nil)
	rep.Check("策略越权 → 403", r.Status == 403, "status=%d code=%s", r.Status, errCode(r))
	r = g.Chat(t, "tok-narrow", map[string]any{"model": "auto", "messages": msgs("user", "hi")}, nil)
	rep.Check("不在 allowed_groups 的组 → 403", r.Status == 403 && errCode(r) == "model_not_allowed", "status=%d code=%s", r.Status, errCode(r))
	r = g.Chat(t, "tok-int", map[string]any{"model": "local/b", "messages": msgs("user", "hi")}, nil)
	rep.Check("allowed_groups 内的直连模型可用", r.Status == 200, "status=%d model=%s", r.Status, header(r, "X-Gatehouse-Model"))
	r = g.Chat(t, "tok-int", map[string]any{"model": "ruled", "messages": msgs("user", "hi")}, map[string]string{"X-Gatehouse-Source": "batch"})
	rep.Check("允许组的规则目标（不在 members 里）照常生效", r.Status == 200 && header(r, "X-Gatehouse-Model") == "local/b", "status=%d model=%s", r.Status, header(r, "X-Gatehouse-Model"))
	r = g.Chat(t, "tok-broke", map[string]any{"model": "auto", "messages": msgs("user", "hi")}, nil)
	rep.Check("策略预算为 0 → 403", r.Status == 403 && errCode(r) == "token_budget_exceeded", "status=%d code=%s", r.Status, errCode(r))

	// 5. provider plugin, JSON and streaming
	r = g.Chat(t, "tok-int", map[string]any{"model": "plug/x", "messages": msgs("user", "插件提供方")}, nil)
	rep.Check("Provider 插件（非流式）", r.Status == 200 && strings.Contains(string(r.Raw), "[plugin/x]"), "status=%d", r.Status)
	r = g.Chat(t, "tok-int", map[string]any{"model": "plug/x", "stream": true, "messages": msgs("user", "插件流式")}, nil)
	rep.Check("Provider 插件（流式）", r.Status == 200 && strings.Contains(string(r.Raw), "[plugin/x]") && strings.Contains(string(r.Raw), "data: [DONE]"), "status=%d", r.Status)

	// 6. a hook may not change the model
	r = g.Chat(t, "tok-int", map[string]any{"model": "auto", "messages": msgs("user", "#hookmodel")}, nil)
	rep.Check("Hooks 改 model → 拒绝", r.Status == 500 && errCode(r) == "hook_invalid", "status=%d code=%s", r.Status, errCode(r))

	// 7. /v1/models lists what the policy allows
	mr := getJSON(t, g.URL+"/models", "tok-int")
	rep.Check("/v1/models 按策略列出", strings.Contains(mr, `"auto"`) && strings.Contains(mr, `"plug/x"`) && !strings.Contains(mr, `"external-only"`), "%s", mr)

	// 8. AuditSink plugin receives the records
	sinkPath := filepath.Join(g.Dir, "sink.jsonl")
	var sunk []map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		sunk = readJSONL(sinkPath)
		if len(sunk) >= 3 {
			break
		}
	}
	fileRecs, _ := g.AuditRecords(t)
	hasDept := false
	for _, s := range sunk {
		if s["department"] == "研发部" {
			hasDept = true
		}
	}
	rep.Check("AuditSink 插件收到记录（与本地文件一致）", len(sunk) > 0 && len(sunk) <= len(fileRecs) && hasDept, "插件收到 %d 条，本地文件 %d 条", len(sunk), len(fileRecs))

	// 9. the policy plugin dies -> every request 503, cached contexts included
	r = g.Chat(t, "tok-crash", map[string]any{"model": "auto", "messages": msgs("user", "hi")}, nil)
	crash := r.Status
	time.Sleep(300 * time.Millisecond)
	n = len(local.Requests()) + len(cloud.Requests())
	r = g.Chat(t, "tok-int", map[string]any{"model": "auto", "messages": msgs("user", "插件死后")}, nil)
	rep.Check("策略插件进程退出 → 503（含已缓存的令牌）", crash == 503 && r.Status == 503 && errCode(r) == "policy_unavailable" && len(local.Requests())+len(cloud.Requests()) == n,
		"崩溃那次 status=%d；之后 status=%d code=%s", crash, r.Status, errCode(r))
	hz := getJSON(t, strings.TrimSuffix(g.URL, "/v1")+"/healthz", "")
	rep.Check("healthz 报告策略不可用", strings.Contains(hz, `"available":false`), "%s", hz)

	// 10. policy plugin disabled at start -> 503 from the first request
	g2 := startGateway(t, fmt.Sprintf(g5Config, local.URL, cloud.URL, false), map[string]string{"tokens.json": g5Tokens})
	r = g2.Chat(t, "tok-int", map[string]any{"model": "auto", "messages": msgs("user", "hi")}, nil)
	rep.Check("策略插件禁用 → 503", r.Status == 503 && errCode(r) == "policy_unavailable", "status=%d code=%s", r.Status, errCode(r))
}

func errCode(r Response) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(r.Raw, &e)
	return e.Error.Code
}

func getJSON(t *testing.T, url, tok string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func readJSONL(path string) []map[string]any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}
