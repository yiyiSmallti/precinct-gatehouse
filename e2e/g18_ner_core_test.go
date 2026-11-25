package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// G18 covers what the core needs before a model-based (NER) detector can run:
// per-source min_confidence and max_action, the detector result cache, and
// on_error: rules_only. fakedetector stands in for the model: PERSON for
// 张三/李四/王五 scored 0.99 and 赵六 scored 0.6.
const g18Config = `
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
plugins:
  - name: ner
    kind: detector
    path: {{FAKEDET}}
    args: ["{{DIR}}"]
redaction:
  mode_by_target: { local: audit, external: mask }
  detectors: [ner]
  categories: { person: block, secret: block }
  sources:
    ner:
%s
`

const (
	g18Phone  = "13800138000"
	g18Phone2 = "13900139000"
	g18Secret = "OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz123456"
)

func TestG18NERCore(t *testing.T) {
	rep := newReport(t, "G18-ner-core", "G18 NER 接入前的核心能力",
		"检测器插件 `ner`（e2e/fakedetector）模拟模型检测器：张三/李四/王五 记 PERSON、置信度 0.99，赵六 记 PERSON、置信度 0.6。"+
			"`categories` 把 person 与 secret 都设为 block。网关 A 给 `ner` 配 `min_confidence: {default: 0.8}`、`max_action: mask`、`timeout: 500ms`、`on_error: rules_only`；"+
			"网关 B 只配 `timeout: 500ms`（默认 fail_closed、无动作上限）作对照，再热重载为 `max_action: audit` 做影子运行。插件调用次数取自插件写的调用记录（只记长度）。")
	local := startFake(t, "local")
	cloud := startFake(t, "cloud")

	// ---------- gateway A ----------
	srcA := "      min_confidence: { default: 0.8 }\n      max_action: mask\n      timeout: 500ms\n      on_error: rules_only"
	g := startGateway(t, fmt.Sprintf(g18Config, local.URL, cloud.URL, srcA), nil)
	calls := func() int { return g18Calls(g.Dir) }

	// 1. threshold and action cap
	turn1 := msgs("user", "请通知张三和赵六明天参会")
	r := g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": turn1}, nil)
	sent := g18Last(cloud)
	rep.Check("max_action: mask：person 设为 block 时 NER 结果只脱敏，不拦截", r.Status == 200 && strings.Contains(sent, "{{PERSON_") && !strings.Contains(sent, "张三"),
		"status=%d，外部收到：%s", r.Status, sent)
	rep.Check("min_confidence 0.8：置信度 0.6 的赵六不处理", strings.Contains(sent, "赵六"), "外部收到：%s", sent)
	rep.Check("回填人名", strings.Contains(r.Content(), "张三"), "客户端收到：%s", r.Content())
	reply1 := r.Content()

	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "配置是 "+g18Secret)}, nil)
	rep.Check("规则命中不受 NER 上限影响：secret 仍拦截", r.Status == 400 && r.ErrCode() == "redaction_blocked", "status=%d code=%s", r.Status, r.ErrCode())

	// 2. detector result cache across turns
	c0 := calls()
	turn2 := append(append([]any{}, turn1...), map[string]any{"role": "assistant", "content": reply1}, map[string]any{"role": "user", "content": "再提醒王五"})
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": turn2}, nil)
	c1 := calls()
	sent = g18Last(cloud)
	rep.Check("第二轮只扫新增的 2 条消息（首条用缓存）", r.Status == 200 && c1-c0 == 2 && !strings.Contains(sent, "张三") && !strings.Contains(sent, "王五"),
		"status=%d，本轮插件调用 %d 次，外部收到：%s", r.Status, c1-c0, sent)
	turn3 := append(append([]any{}, turn2...), map[string]any{"role": "assistant", "content": r.Content()}, map[string]any{"role": "user", "content": "李四也要来"})
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": turn3}, nil)
	c2 := calls()
	rep.Check("第三轮 5 条消息仍只调用 2 次", r.Status == 200 && c2-c1 == 2, "status=%d，本轮插件调用 %d 次", r.Status, c2-c1)
	hz := g18Healthz(g)
	rep.Check("/healthz 报告缓存命中", g18Num(hz, "detect_cache", "hits") >= 3, "detect_cache=%v", hz["detect_cache"])

	// 3. hot reload of min_confidence: cached results, new threshold
	cfgPath := filepath.Join(g.Dir, "config.yaml")
	cur, _ := os.ReadFile(cfgPath)
	os.WriteFile(cfgPath, bytes.Replace(cur, []byte("default: 0.8"), []byte("default: 0.5"), 1), 0o600)
	g.cmd.Process.Signal(syscall.SIGHUP)
	rep.Check("热重载生效", g18WaitLog(g, "config reloaded", 2*time.Second), "")
	c3 := calls()
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": turn1}, nil)
	sent = g18Last(cloud)
	rep.Check("阈值降到 0.5 后赵六被脱敏，且没有重新调用插件", r.Status == 200 && !strings.Contains(sent, "赵六") && calls()-c3 == 0,
		"status=%d，插件调用 %d 次，外部收到：%s", r.Status, calls()-c3, sent)

	// 4. audit samples carry source and confidence, never values
	recs, raw := g.AuditRecords(t)
	var high, low map[string]any // 张三 (0.99) before the reload, 赵六 (0.6) after it
	for _, rec := range recs {
		red, _ := rec["redaction"].(map[string]any)
		ss, _ := red["samples"].([]any)
		for _, s := range ss {
			m, _ := s.(map[string]any)
			if m["source"] != "ner" || m["action"] != "mask" {
				continue
			}
			switch c := g18F(m["confidence"]); {
			case c > 0.9 && high == nil:
				high = m
			case c > 0.5 && c < 0.7:
				low = m
			}
		}
	}
	rep.Check("审计样本记录来源与置信度", high != nil && low != nil, "0.99 的样本：%v；重载后 0.6 的样本：%v", high, low)
	rep.Check("审计不含原文", !strings.Contains(raw, "张三") && !strings.Contains(raw, "赵六") && !strings.Contains(raw, "sk-proj-abcdefghij"), "审计 %d 字节", len(raw))

	// 5. timeout under rules_only: one timeout per request, rules still apply
	c4 := calls()
	start := time.Now()
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": []any{
		map[string]any{"role": "system", "content": "背景资料 #hang-detector"},
		map[string]any{"role": "user", "content": "张三的电话 " + g18Phone},
		map[string]any{"role": "assistant", "content": "收到"},
		map[string]any{"role": "user", "content": "李四的电话 " + g18Phone2},
	}}, nil)
	el := time.Since(start)
	sent = g18Last(cloud)
	rep.Check("插件超时 → 继续发送，规则照常脱敏手机号", r.Status == 200 && !strings.Contains(sent, g18Phone) && !strings.Contains(sent, g18Phone2) && strings.Contains(sent, "{{PHONE_"),
		"status=%d，外部收到：%s", r.Status, sent)
	rep.Check("整请求只等一次超时，后续文本跳过插件", el < 2*time.Second && calls()-c4 == 1, "耗时 %v，插件调用 %d 次", el.Round(time.Millisecond), calls()-c4)
	rep.Check("退化后人名按规则处理（规则不识别人名，原样发出）", strings.Contains(sent, "张三"), "这是 rules_only 的代价，记在审计告警里")
	time.Sleep(200 * time.Millisecond)
	_, raw = g.AuditRecords(t)
	rep.Check("审计记录退化告警", strings.Contains(raw, "continued without it (on_error: rules_only)"), "")

	// 6. plugin crash under rules_only: no 503 during the restart window
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "#crash-detector 电话 "+g18Phone)}, nil)
	sent = g18Last(cloud)
	rep.Check("插件进程崩溃 → 仍发送，手机号已脱敏", r.Status == 200 && !strings.Contains(sent, g18Phone), "status=%d code=%s", r.Status, r.ErrCode())
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "李四 "+g18Phone2)}, nil)
	sent = g18Last(cloud)
	rep.Check("重启窗口内请求不返回 503", r.Status == 200 && !strings.Contains(sent, g18Phone2), "status=%d，外部收到：%s", r.Status, sent)
	running := false
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if g18PluginState(g18Healthz(g), "ner") == "running" {
			running = true
			break
		}
	}
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "王五明天到")}, nil)
	sent = g18Last(cloud)
	rep.Check("插件重启后恢复 NER 脱敏", running && r.Status == 200 && !strings.Contains(sent, "王五"), "running=%v status=%d，外部收到：%s", running, r.Status, sent)
	g.Stop()

	// ---------- gateway B: defaults, then a shadow run ----------
	cloud2 := startFake(t, "cloud")
	g = startGateway(t, fmt.Sprintf(g18Config, local.URL, cloud2.URL, "      timeout: 500ms"), nil)
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "请通知张三")}, nil)
	rep.Check("对照：无 max_action 时 person: block 拦截 NER 结果", r.Status == 400 && r.ErrCode() == "redaction_blocked", "status=%d code=%s", r.Status, r.ErrCode())
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "背景 #hang-detector")}, nil)
	rep.Check("对照：默认 fail_closed 下插件超时 → 503", r.Status == 503 && r.ErrCode() == "redaction_unavailable" && len(cloud2.Requests()) == 0,
		"status=%d code=%s，外部收到 %d 次", r.Status, r.ErrCode(), len(cloud2.Requests()))

	cfgPath = filepath.Join(g.Dir, "config.yaml")
	cur, _ = os.ReadFile(cfgPath)
	os.WriteFile(cfgPath, bytes.Replace(cur, []byte("      timeout: 500ms"), []byte("      timeout: 500ms\n      max_action: audit"), 1), 0o600)
	g.cmd.Process.Signal(syscall.SIGHUP)
	rep.Check("热重载为影子运行（max_action: audit）", g18WaitLog(g, "config reloaded", 2*time.Second), "")
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "张三的电话 "+g18Phone)}, nil)
	sent = g18Last(cloud2)
	rep.Check("影子运行：NER 结果只计数、原样发出，规则照常脱敏", r.Status == 200 && strings.Contains(sent, "张三") && !strings.Contains(sent, g18Phone),
		"status=%d，X-Gatehouse-Redaction: %s，外部收到：%s", r.Status, header(r, "X-Gatehouse-Redaction"), sent)
	time.Sleep(200 * time.Millisecond)
	recs, _ = g.AuditRecords(t)
	var shadow []any
	if len(recs) > 0 {
		red, _ := recs[len(recs)-1]["redaction"].(map[string]any)
		shadow, _ = red["samples"].([]any)
	}
	ok := false
	for _, s := range shadow {
		if m, _ := s.(map[string]any); m["source"] == "ner" && m["action"] == "audit" && m["kind"] == "PERSON" {
			ok = true
		}
	}
	b, _ := json.Marshal(shadow)
	rep.Check("影子运行的审计样本：source=ner、action=audit", ok, "%s", b)
	g.Stop()

	// ---------- gateway C: failover keeps the degraded state ----------
	down := startFake(t, "down")
	down.SetDown(true)
	cloud3 := startFake(t, "cloud")
	g = startGateway(t, fmt.Sprintf(g18FailoverConfig, down.URL, cloud3.URL), nil)
	c5 := g18Calls(g.Dir)
	start = time.Now()
	r = g.Chat(t, g.Token, map[string]any{"model": "pair", "messages": msgs("user", "背景 #hang-detector 电话 "+g18Phone)}, nil)
	el = time.Since(start)
	sent = g18Last(cloud3)
	rep.Check("故障转移到第二个候选时不再等检测器超时", r.Status == 200 && header(r, "X-Gatehouse-Model") == "cloud/strong" && len(down.Requests()) == 1 && g18Calls(g.Dir)-c5 == 1 && el < 1500*time.Millisecond && !strings.Contains(sent, g18Phone),
		"status=%d 模型=%s，第一个候选收到 %d 次，插件调用 %d 次，耗时 %v", r.Status, header(r, "X-Gatehouse-Model"), len(down.Requests()), g18Calls(g.Dir)-c5, el.Round(time.Millisecond))
	time.Sleep(200 * time.Millisecond)
	recs, _ = g.AuditRecords(t)
	alerted := false
	if len(recs) > 0 {
		b, _ := json.Marshal(recs[len(recs)-1]["redaction"])
		alerted = strings.Contains(string(b), "on_error: rules_only")
	}
	rep.Check("最终审计记录仍含退化告警（失败发生在第一个候选）", alerted, "")

	// classifier redaction shares the request scope
	c6, cls := g18Calls(g.Dir), cloud3.ClassifierCalls()
	start = time.Now()
	r = g.Chat(t, g.Token, map[string]any{"model": "smart", "messages": msgs("user", "#hang-detector 电话 "+g18Phone2)}, nil)
	el = time.Since(start)
	sent = g18Last(cloud3)
	rep.Check("意图路由：分类器脱敏时插件超时，正式请求不再等第二次", r.Status == 200 && cloud3.ClassifierCalls()-cls == 1 && g18Calls(g.Dir)-c6 == 1 && el < 1500*time.Millisecond && !strings.Contains(sent, g18Phone2),
		"status=%d，分类器调用 %d 次，插件调用 %d 次，耗时 %v", r.Status, cloud3.ClassifierCalls()-cls, g18Calls(g.Dir)-c6, el.Round(time.Millisecond))
	time.Sleep(200 * time.Millisecond)
	recs, _ = g.AuditRecords(t)
	alerted = false
	if len(recs) > 0 {
		b, _ := json.Marshal(recs[len(recs)-1]["redaction"])
		alerted = strings.Contains(string(b), "on_error: rules_only")
	}
	rep.Check("审计记录含分类器阶段的退化告警", alerted, "")

	// a NaN score must not cost the request its audit record
	before := len(recs)
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "请联系孙七")}, nil)
	sent = g18Last(cloud3)
	time.Sleep(200 * time.Millisecond)
	recs, _ = g.AuditRecords(t)
	var nan map[string]any
	if len(recs) > before {
		red, _ := recs[len(recs)-1]["redaction"].(map[string]any)
		ss, _ := red["samples"].([]any)
		if len(ss) > 0 {
			nan, _ = ss[0].(map[string]any)
		}
	}
	rep.Check("插件返回 NaN 置信度：按未打分处理，照常脱敏且审计照常写入", r.Status == 200 && !strings.Contains(sent, "孙七") && nan != nil && nan["source"] == "ner" && nan["confidence"] == nil,
		"status=%d，审计新增 %d 条，样本：%v", r.Status, len(recs)-before, nan)
	g.Stop()

	// ---------- reserved names ----------
	dir := t.TempDir()
	cfg := strings.ReplaceAll(fmt.Sprintf(g18Config, local.URL, cloud.URL, "      max_action: audit"), "name: ner", "name: rules")
	cfg = strings.ReplaceAll(cfg, "detectors: [ner]", "detectors: [rules]")
	cfg = strings.ReplaceAll(cfg, "    ner:\n", "    rules:\n")
	cfg = strings.ReplaceAll(strings.ReplaceAll(cfg, "{{LISTEN}}", "127.0.0.1:0"), "{{FAKEDET}}", fakeDetPath)
	cfg = strings.ReplaceAll(cfg, "{{DIR}}", filepath.ToSlash(dir))
	cfgFile := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgFile, []byte(cfg), 0o600)
	out, err := exec.Command(gatehouse, "config", "check", "--config", cfgFile).CombinedOutput()
	rep.Check("检测器插件不能命名为 rules / dictionary（否则其 max_action 会作用到内置规则）", err != nil && strings.Contains(string(out), "plugins.rules: name is reserved"), "%s", g18Problems(string(out)))
}

const g18FailoverConfig = `
listen: {{LISTEN}}
mode: personal
providers:
  - name: down
    base_url: %s
    target: external
    models: [strong]
  - name: cloud
    base_url: %s
    target: external
    models: [strong]
routing_groups:
  pair:
    type: order
    members: [down/strong, cloud/strong]
  smart:
    type: order
    members: [cloud/strong]
    classifier: cloud/strong
    classifier_timeout: 5s
    rules:
      - name: chat
        intent: "闲聊、问候"
        use: cloud/strong
plugins:
  - name: ner
    kind: detector
    path: {{FAKEDET}}
    args: ["{{DIR}}"]
redaction:
  mode_by_target: { external: mask }
  detectors: [ner]
  sources:
    ner: { timeout: 500ms, on_error: rules_only }
`

func g18Calls(dir string) int {
	b, _ := os.ReadFile(filepath.Join(dir, "fakedetector.calls"))
	return strings.Count(string(b), "\n")
}

func g18Last(f *FakeProvider) string {
	rs := f.Requests()
	if len(rs) == 0 {
		return ""
	}
	var m map[string]any
	json.Unmarshal(rs[len(rs)-1], &m)
	b, _ := json.Marshal(m["messages"])
	return string(b)
}

func g18Healthz(g *Gateway) map[string]any {
	resp, err := http.Get(strings.TrimSuffix(g.URL, "/v1") + "/healthz")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func g18Num(m map[string]any, obj, key string) float64 {
	o, _ := m[obj].(map[string]any)
	return g18F(o[key])
}

func g18F(v any) float64 {
	f, _ := v.(float64)
	return f
}

func g18PluginState(hz map[string]any, name string) string {
	ps, _ := hz["plugins"].([]any)
	for _, p := range ps {
		if m, _ := p.(map[string]any); m["name"] == name {
			s, _ := m["state"].(string)
			return s
		}
	}
	return ""
}

func g18WaitLog(g *Gateway, s string, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(g.log.String(), s) {
			return true
		}
	}
	return false
}

// g18Problems keeps the problem lines of `config check` output (not the temp path).
func g18Problems(out string) string {
	var ps []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "- ") {
			ps = append(ps, l)
		}
	}
	return strings.Join(ps, "；")
}
