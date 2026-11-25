package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// G19 runs the real AstrLink Guard NER plugin (plugins/astrlink-detector)
// behind `gatehouse serve`. It needs the model and ONNX Runtime, which are
// not part of the repository, so it only runs when both are given:
//
//	ASTRLINK_DIR=<folder with model_int8.onnx(+_data), tokenizer.json, config.json>
//	ORT_LIB=<libonnxruntime shared library, >= 1.26>
//
// CI only builds the plugin; this test is run locally.
const g19Config = `
listen: {{LISTEN}}
mode: personal
providers:
  - name: cloud
    base_url: %s
    target: external
    models: [strong]
plugins:
  - name: astrlink
    kind: detector
    path: %s
    args: ["-model", "%s", "-ort", "%s"]
redaction:
  mode_by_target: { external: mask }
  detectors: [astrlink]
  categories: { secret: block }
  sources:
    astrlink:
      min_confidence: { default: 0.8, person: 0.9, date: 0.9, account: 0.7 }
      max_action: mask
      timeout: 30s
      on_error: rules_only
`

func TestG19AstrLink(t *testing.T) {
	model, ort := os.Getenv("ASTRLINK_DIR"), os.Getenv("ORT_LIB")
	if model == "" || ort == "" {
		t.Skip("set ASTRLINK_DIR and ORT_LIB to run the real NER plugin")
	}
	plug := filepath.Join(binDir, "astrlink-detector")
	build := exec.Command("go", "build", "-o", plug, ".")
	build.Dir = "../plugins/astrlink-detector"
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}

	rep := newReport(t, "G19-astrlink", "G19 AstrLink NER 插件（真实模型）",
		"`plugins/astrlink-detector` 加载 AstrLink Guard int8 模型（ONNX Runtime），作为检测器插件接入真实 `gatehouse serve`。"+
			"阈值、动作上限与超时退化都在网关 `redaction.sources.astrlink` 设置：`min_confidence {default 0.8, person 0.9, date 0.9, account 0.7}`、`max_action: mask`、`timeout: 30s`、`on_error: rules_only`；类别 `secret: block` 只对规则生效。"+
			"本测试需要本机模型与运行库（`ASTRLINK_DIR`、`ORT_LIB`），CI 只编译插件。")
	cloud := startFake(t, "cloud")
	start := time.Now()
	g := startGateway(t, fmt.Sprintf(g19Config, cloud.URL, plug, model, ort), nil)
	running := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if g18PluginState(g18Healthz(g), "astrlink") == "running" {
			running = true
			break
		}
	}
	rep.Check("插件启动并加载模型", running, "从启动网关到插件就绪 %v", time.Since(start).Round(time.Millisecond))

	chat := func(messages []any) (Response, time.Duration) {
		t0 := time.Now()
		r := g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": messages}, nil)
		return r, time.Since(t0)
	}

	// 1. Chinese and English PII, code left alone
	pii := "帮我给王建国写封邮件，他住在杭州市西湖区文澜东路88号翠微花园3栋2单元1201室。另外问下 Emily Carter 那个 PR 的进度：\n```go\nfunc (s *AccountService) Disable(ctx context.Context, id int64) error { return nil }\n```"
	r, el := chat(msgs("user", pii))
	sent := g18Last(cloud)
	rep.Check("中文人名、地址与英文人名被脱敏", r.Status == 200 && !strings.Contains(sent, "王建国") && !strings.Contains(sent, "文澜东路88号") && !strings.Contains(sent, "Emily Carter") && strings.Contains(sent, "{{PERSON_") && strings.Contains(sent, "{{ADDRESS_"),
		"status=%d，耗时 %v，X-Gatehouse-Redaction: %s，外部收到：%s", r.Status, el.Round(time.Millisecond), header(r, "X-Gatehouse-Redaction"), sent)
	rep.Check("代码块里的标识符原样保留", strings.Contains(sent, "AccountService") && strings.Contains(sent, "func (s *"), "")
	rep.Check("响应回填原文", strings.Contains(r.Content(), "王建国") && strings.Contains(r.Content(), "Emily Carter"), "客户端收到：%s", r.Content())

	// control characters the tokenizer deletes at the start must not shift the masked range
	r, _ = chat(msgs("user", "\x01\x01\x01请联系 Emily Carter 确认合同"))
	sent = g18Last(cloud)
	rep.Check("开头有被删除的控制字符时，人名仍完整脱敏", r.Status == 200 && !strings.Contains(sent, "Carter") && !strings.Contains(sent, "ter 确认") && strings.Contains(sent, "{{PERSON_"),
		"status=%d，外部收到：%s", r.Status, sent)

	// 2. a clean coding request goes out unchanged
	clean := "Why does `go vet` complain about this?\n```go\nfmt.Printf(\"%d\", name)\n```\nThe Jenkins job runs on Kubernetes and logs to Grafana."
	r, el = chat(msgs("user", clean))
	sent = g18Last(cloud)
	rep.Check("干净的编程请求原样发出", r.Status == 200 && !strings.Contains(sent, "{{"), "status=%d，耗时 %v，X-Gatehouse-Redaction: %s", r.Status, el.Round(time.Millisecond), header(r, "X-Gatehouse-Redaction"))

	// 3. a model-guessed secret is masked (max_action), a rule-matched one still blocks
	r, _ = chat(msgs("user", "我把 xnwp-8meAts-RCDC-p63Jz 贴在这，你帮我测一下这个接口能不能通。"))
	sent = g18Last(cloud)
	rep.Check("模型判出的密钥只脱敏、不拦截", r.Status == 200 && strings.Contains(sent, "{{NER_SECRET_") && !strings.Contains(sent, "xnwp-8meAts"), "status=%d，X-Gatehouse-Redaction: %s", r.Status, header(r, "X-Gatehouse-Redaction"))
	r, _ = chat(msgs("user", "配置是 "+g18Secret))
	rep.Check("规则命中的密钥仍拦截", r.Status == 400 && r.ErrCode() == "redaction_blocked", "status=%d code=%s", r.Status, r.ErrCode())

	// 4. a long conversation: the second turn scans only the new messages
	spec, err := os.ReadFile("../docs/SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for sb.Len() < 60_000 {
		sb.Write(spec)
		sb.WriteString("\n")
	}
	doc := sb.String() + "\n请把上面文档的要点发给王建国。"
	hits0 := g18Num(g18Healthz(g), "detect_cache", "hits")
	turn1 := msgs("user", doc)
	r1, el1 := chat(turn1)
	turn2 := append(append([]any{}, turn1...), map[string]any{"role": "assistant", "content": "好的。"}, map[string]any{"role": "user", "content": "再抄送 Emily Carter。"})
	r2, el2 := chat(turn2)
	sent = g18Last(cloud)
	hits := g18Num(g18Healthz(g), "detect_cache", "hits") - hits0
	rep.Check("长对话第二轮复用第一轮的检测结果", r1.Status == 200 && r2.Status == 200 && hits >= 1 && el2 < el1/3 && !strings.Contains(sent, "Emily Carter"),
		"文档约 %d 字符；第一轮 %v，第二轮 %v，缓存命中 +%.0f", len(doc), el1.Round(time.Millisecond), el2.Round(time.Millisecond), hits)

	// 5. audit: source and scores, no values
	time.Sleep(200 * time.Millisecond)
	recs, raw := g.AuditRecords(t)
	var ss []any
	for _, rec := range recs {
		red, _ := rec["redaction"].(map[string]any)
		s, _ := red["samples"].([]any)
		for _, x := range s {
			if m, _ := x.(map[string]any); m["source"] == "astrlink" && len(ss) < 6 {
				ss = append(ss, m)
			}
		}
	}
	b, _ := json.Marshal(ss)
	rep.Check("审计样本带来源与置信度，不含原文", len(ss) > 0 && !strings.Contains(raw, "王建国") && !strings.Contains(raw, "Emily Carter") && !strings.Contains(raw, "xnwp-8meAts"), "%s", b)

	if out, err := exec.Command("pgrep", "-f", plug).Output(); err == nil {
		for _, pid := range strings.Fields(string(out)) {
			rss, _ := exec.Command("ps", "-o", "rss=", "-p", pid).Output()
			rep.Note("插件进程 RSS：%s KiB", strings.TrimSpace(string(rss)))
		}
	}
}
