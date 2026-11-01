package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const g3Config = `
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
    enabled: %v
redaction:
  mode_by_target: { local: audit, external: mask }
  detectors: [ner]
  fail_closed: true
`

func TestG3FailClosed(t *testing.T) {
	rep := newReport(t, "G3-fail-closed", "G3 fail-closed",
		"检测器插件 `ner`（e2e/fakedetector，go-plugin gRPC 子进程，识别人名 PERSON）被声明为必需（`redaction.detectors: [ner]`）。分三种情况：插件正常、配置里禁用、运行中进程被杀。")
	local := startFake(t, "local")
	cloud := startFake(t, "cloud")
	body := map[string]any{"model": "cloud/strong", "messages": msgs("user", "请提醒张三明天开会")}

	// A. plugin running: PERSON masked by the plugin
	g := startGateway(t, fmt.Sprintf(g3Config, local.URL, cloud.URL, true), nil)
	r := g.Chat(t, g.Token, body, nil)
	sent := cloud.Requests()
	rep.Check("插件正常：人名经插件脱敏", r.Status == 200 && len(sent) == 1 && !strings.Contains(string(sent[0]), "张三") && strings.Contains(string(sent[0]), "{{PERSON_"),
		"status=%d，外部收到 %d 次，X-Gatehouse-Redaction: %s", r.Status, len(sent), header(r, "X-Gatehouse-Redaction"))
	rep.Check("插件正常：回填人名", strings.Contains(r.Content(), "张三"), "客户端收到：%s", r.Content())

	// C. plugin process dies while the gateway runs
	if runtime.GOOS == "linux" {
		pid := strconv.Itoa(g.cmd.Process.Pid)
		exec.Command("pkill", "-KILL", "-P", pid, "-f", "fakedetector").Run()
		time.Sleep(300 * time.Millisecond)
		n := len(cloud.Requests())
		r = g.Chat(t, g.Token, body, nil)
		rep.Check("运行中插件进程被杀 → 503", r.Status == 503 && r.ErrCode() == "redaction_unavailable" && len(cloud.Requests()) == n,
			"status=%d code=%s，外部新增请求 %d", r.Status, r.ErrCode(), len(cloud.Requests())-n)
	}
	g.Stop()

	// B. plugin disabled in config
	cloud2 := startFake(t, "cloud")
	g = startGateway(t, fmt.Sprintf(g3Config, local.URL, cloud2.URL, false), nil)
	r = g.Chat(t, g.Token, body, nil)
	rep.Check("禁用检测器插件 → 503", r.Status == 503 && r.ErrCode() == "redaction_unavailable", "status=%d code=%s message=%v", r.Status, r.ErrCode(), r.Body["error"])
	rep.Check("外部假服务未收到任何请求", len(cloud2.Requests()) == 0, "外部收到 %d 次", len(cloud2.Requests()))
	resp, err := http.Get(strings.TrimSuffix(g.URL, "/v1") + "/healthz")
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		rep.Check("/healthz 标出降级", strings.Contains(string(b), `"degraded"`), "%s", b)
	}
	r = g.Chat(t, g.Token, map[string]any{"model": "local/small", "messages": msgs("user", "请提醒张三明天开会")}, nil)
	rep.Check("本地目标 audit 模式不受影响", r.Status == 200, "status=%d（audit 只记不改，不要求检测器）", r.Status)
}
