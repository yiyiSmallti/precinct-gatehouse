package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

const g17Config = `
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
  - name: plug
    plugin: plugprov
    target: local
    models: [x]
default_group: plugfirst
routing_groups:
  plugfirst:
    type: order
    members: [plug/x, local/small]
plugins:
  - name: ner
    kind: detector
    path: {{FAKEDET}}
    args: ["{{DIR}}"]
  - name: plugprov
    kind: provider
    path: {{FAKEPLUGIN}}
    args: [provider, "{{DIR}}"]
redaction:
  mode_by_target: { local: audit, external: mask }
  detectors: [ner]
  fail_closed: true
`

const g17OrgConfig = `
listen: {{LISTEN}}
mode: org
providers:
  - name: local
    base_url: %s
    target: local
    models: [a]
default_group: default
routing_groups:
  default:
    type: order
    members: [local/a]
plugins:
  - name: cloister
    kind: auth_policy
    path: {{FAKEPLUGIN}}
    args: [policy, "{{DIR}}/tokens.json"]
  - name: tagger
    kind: hooks
    path: {{FAKEPLUGIN}}
    args: [hooks, "{{DIR}}"]
redaction:
  mode_by_target: { local: audit, external: mask }
`

const g17Tokens = `{
  "tok-int":   {"tenant": "org_acme", "user": "u_li", "department": "市场部", "data_class": "internal", "allowed_groups": ["default"]},
  "tok-crash": {"action": "crash"}
}`

type g17Health struct {
	Status               string   `json:"status"`
	DetectorsUnavailable []string `json:"detectors_unavailable"`
	Policy               *struct {
		Available bool `json:"available"`
	} `json:"policy"`
	Plugins []struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		State    string `json:"state"`
		Restarts int    `json:"restarts"`
	} `json:"plugins"`
}

func (h g17Health) plugin(name string) string {
	for _, p := range h.Plugins {
		if p.Name == name {
			return fmt.Sprintf("%s/restarts=%d", p.State, p.Restarts)
		}
	}
	return "-"
}

func g17Healthz(t *testing.T, g *Gateway) (g17Health, string) {
	raw := getJSON(t, strings.TrimSuffix(g.URL, "/v1")+"/healthz", "")
	var h g17Health
	json.Unmarshal([]byte(raw), &h)
	return h, raw
}

// g17Procs lists the plugin processes started for the gateway in dir (their
// args carry dir); nil on Windows.
func g17Procs(dir string) []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	out, _ := exec.Command("ps", "-axo", "pid=,command=").Output()
	var ps []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, dir) && (strings.Contains(l, "fakedetector") || strings.Contains(l, "fakeplugin")) {
			ps = append(ps, strings.Fields(l)[0])
		}
	}
	return ps
}

// g17Until repeats call until ok or the limit, returning the elapsed time and the last response.
func g17Until(limit time.Duration, call func() Response, ok func(Response) bool) (time.Duration, Response) {
	start := time.Now()
	var r Response
	for time.Since(start) < limit {
		if r = call(); ok(r) {
			return time.Since(start), r
		}
		time.Sleep(100 * time.Millisecond)
	}
	return time.Since(start), r
}

func TestG17PluginRestart(t *testing.T) {
	rep := newReport(t, "G17-plugin-restart", "G17 插件崩溃自动重启",
		"每个插件由 supervisor 监控，进程退出后按指数退避重启（1 s 起、翻倍、上限 30 s；稳定运行 30 s 后回到 1 s），重启成功后原子替换 client 与实现。假插件收到指令即 `os.Exit(3)`：检测器 `#crash-detector`、Provider `#crash-provider`、Hooks `#crash-hook`、策略令牌 `tok-crash`。断言崩溃窗口内 fail-closed、上游收不到请求，退避后恢复，`/healthz` 报告状态与重启次数，关停后不留子进程。")
	local := startFake(t, "local")
	cloud := startFake(t, "cloud")
	g := startGateway(t, fmt.Sprintf(g17Config, local.URL, cloud.URL), nil)
	// a new text each time: the detector result cache would otherwise answer
	// for text it has seen, and the request would not need the plugin at all
	seq := 0
	chat := func() Response {
		seq++
		body := map[string]any{"model": "cloud/strong", "messages": msgs("user", fmt.Sprintf("请提醒张三明天开会（第 %d 次）", seq))}
		return g.Chat(t, g.Token, body, nil)
	}
	masked := func(r Response) bool {
		reqs := cloud.ChatRequests()
		if r.Status != 200 || len(reqs) == 0 {
			return false
		}
		up := lastUser(reqs[len(reqs)-1])
		return !strings.Contains(up, "张三") && strings.Contains(up, "{{PERSON_") && strings.Contains(r.Content(), "张三")
	}

	// A. detector
	h, _ := g17Healthz(t, g)
	procs := g17Procs(g.Dir)
	rep.Check("启动：/healthz 列出插件状态", h.Status == "ok" && h.plugin("ner") == "running/restarts=0" && h.plugin("plugprov") == "running/restarts=0",
		"status=%s ner=%s plugprov=%s；子进程 %d 个", h.Status, h.plugin("ner"), h.plugin("plugprov"), len(procs))
	r := chat()
	rep.Check("基线：人名经检测器插件脱敏并回填", masked(r), "status=%d 客户端收到：%s", r.Status, r.Content())

	n := len(cloud.Requests())
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "张三 #crash-detector")}, nil)
	crash := r
	r = chat()
	h, raw := g17Healthz(t, g)
	rep.Check("检测器崩溃 → 窗口内 503 redaction_unavailable，外部未收到请求", crash.Status == 503 && crash.ErrCode() == "redaction_unavailable" && r.Status == 503 && r.ErrCode() == "redaction_unavailable" && len(cloud.Requests()) == n,
		"崩溃那次 %d/%s；紧接着 %d/%s；外部新增 %d", crash.Status, crash.ErrCode(), r.Status, r.ErrCode(), len(cloud.Requests())-n)
	rep.Check("窗口内 /healthz：degraded，ner 在重启", h.Status == "degraded" && strings.HasPrefix(h.plugin("ner"), "restarting") && strings.Contains(strings.Join(h.DetectorsUnavailable, ","), "ner"), "%s", raw)
	took, r := g17Until(15*time.Second, chat, func(r Response) bool { return r.Status == 200 })
	rep.Check("退避后恢复 200 并正确脱敏、回填", masked(r) && took >= 800*time.Millisecond, "崩溃后 %.1f s 恢复（首次退避 1 s）；客户端收到：%s", took.Seconds(), r.Content())
	h, _ = g17Healthz(t, g)
	rep.Check("恢复后 /healthz：ok，ner 重启 1 次", h.Status == "ok" && h.plugin("ner") == "running/restarts=1", "status=%s ner=%s", h.Status, h.plugin("ner"))

	g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "张三 #crash-detector")}, nil)
	took2, r := g17Until(15*time.Second, chat, func(r Response) bool { return r.Status == 200 })
	rep.Check("短时间内再次崩溃 → 退避翻倍", r.Status == 200 && took2 >= 1800*time.Millisecond && took2 > took, "第二次 %.1f s 恢复（退避 2 s）", took2.Seconds())

	// B. provider
	pr := g.Chat(t, g.Token, map[string]any{"model": "plug/x", "messages": msgs("user", "#crash-provider")}, nil)
	ln := len(local.Requests())
	fr := g.Chat(t, g.Token, map[string]any{"model": "plugfirst", "messages": msgs("user", "插件重启中")}, nil)
	rep.Check("Provider 插件崩溃 → 按可重试错误处理（直连 502，组内故障转移到下一个成员）", pr.Status == 502 && pr.ErrCode() == "upstream_unavailable" && fr.Status == 200 && header(fr, "X-Gatehouse-Model") == "local/small" && len(local.Requests()) == ln+1,
		"直连 %d/%s；组请求 %d model=%s", pr.Status, pr.ErrCode(), fr.Status, header(fr, "X-Gatehouse-Model"))
	took, r = g17Until(15*time.Second, func() Response {
		return g.Chat(t, g.Token, map[string]any{"model": "plug/x", "messages": msgs("user", "回来了吗")}, nil)
	}, func(r Response) bool { return r.Status == 200 })
	h, _ = g17Healthz(t, g)
	rep.Check("Provider 插件恢复", r.Status == 200 && strings.Contains(r.Content(), "[plugin/x]") && h.plugin("plugprov") == "running/restarts=1", "%.1f s 后 status=%d plugprov=%s", took.Seconds(), r.Status, h.plugin("plugprov"))

	logs := g.log.String()
	after := g17Procs(g.Dir)
	rep.Check("重启写日志", strings.Contains(logs, "plugin process exited; restarting") && strings.Contains(logs, "plugin restarted"), "日志含 exited/restarted 记录")
	if runtime.GOOS != "windows" {
		rep.Check("重启后旧进程不残留", len(procs) == 2 && len(after) == 2 && after[0] != procs[0], "启动时 %v，三次崩溃后 %v", procs, after)
	}
	g.Stop()
	time.Sleep(1500 * time.Millisecond)
	if runtime.GOOS != "windows" {
		left := g17Procs(g.Dir)
		rep.Check("网关关停后无子进程残留、不再重启", len(left) == 0, "残留 %v", left)
	}

	// C. org mode: policy and hooks
	og := startGateway(t, fmt.Sprintf(g17OrgConfig, local.URL), map[string]string{"tokens.json": g17Tokens})
	ochat := func() Response {
		return og.Chat(t, "tok-int", map[string]any{"model": "auto", "messages": msgs("user", "org 请求")}, nil)
	}
	r = ochat()
	rep.Check("org 基线：策略与 Hooks 正常", r.Status == 200 && strings.Contains(r.Content(), "[hooked:市场部]"), "status=%d 客户端收到：%s", r.Status, r.Content())
	crash = og.Chat(t, "tok-crash", map[string]any{"model": "auto", "messages": msgs("user", "hi")}, nil)
	n = len(local.Requests())
	r = ochat()
	h, raw = g17Healthz(t, og)
	rep.Check("策略插件崩溃 → 窗口内 503 policy_unavailable，不用 5 s 策略缓存，上游未收到请求", crash.Status == 503 && r.Status == 503 && errCode(r) == "policy_unavailable" && len(local.Requests()) == n,
		"崩溃那次 %d；紧接着（tok-int 刚缓存过）%d/%s；上游新增 %d", crash.Status, r.Status, errCode(r), len(local.Requests())-n)
	rep.Check("窗口内 /healthz：unavailable，policy.available=false", h.Status == "unavailable" && h.Policy != nil && !h.Policy.Available && strings.HasPrefix(h.plugin("cloister"), "restarting"), "%s", raw)
	took, r = g17Until(15*time.Second, ochat, func(r Response) bool { return r.Status == 200 })
	h, _ = g17Healthz(t, og)
	rep.Check("策略插件退避后恢复", r.Status == 200 && h.Policy != nil && h.Policy.Available && h.plugin("cloister") == "running/restarts=1", "%.1f s 后 status=%d cloister=%s", took.Seconds(), r.Status, h.plugin("cloister"))

	hc := og.Chat(t, "tok-int", map[string]any{"model": "auto", "messages": msgs("user", "#crash-hook")}, nil)
	n = len(local.Requests())
	r = ochat()
	rep.Check("Hooks 插件崩溃 → 窗口内 503，上游未收到请求", hc.Status == 503 && hc.ErrCode() == "hook_failed" && r.Status == 503 && r.ErrCode() == "hook_unavailable" && len(local.Requests()) == n,
		"崩溃那次 %d/%s；紧接着 %d/%s", hc.Status, hc.ErrCode(), r.Status, r.ErrCode())
	took, r = g17Until(15*time.Second, ochat, func(r Response) bool { return r.Status == 200 })
	rep.Check("Hooks 插件恢复并继续改写请求", r.Status == 200 && strings.Contains(r.Content(), "[hooked:市场部]"), "%.1f s 后 status=%d 客户端收到：%s", took.Seconds(), r.Status, r.Content())
	og.Stop()
	time.Sleep(1500 * time.Millisecond)
	if runtime.GOOS != "windows" {
		left := g17Procs(og.Dir)
		rep.Check("org 网关关停后无子进程残留", len(left) == 0, "残留 %v", left)
	}
}
