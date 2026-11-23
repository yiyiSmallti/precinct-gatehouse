package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const g13Config = `
listen: {{LISTEN}}
mode: personal
admin:
  listen: %s
meter:
  otlp:
    endpoint: http://127.0.0.1:9
    interval: 1h
    headers:
      X-Collector-Tenant: g13-e2e
providers:
  - name: p
    base_url: %s
    target: local
    models: [model-a]
default_group: g
routing_groups:
  g:
    members: [p/model-a]
`

// startGatewayLogFile starts serve with stdout in a file, so the test can read
// the one-time setup code while the process is still running.
func startGatewayLogFile(t *testing.T, dir, cfg, adminBase string) (*Gateway, string) {
	t.Helper()
	addr := freePort(t)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(strings.ReplaceAll(cfg, "{{LISTEN}}", addr)), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, fmt.Sprintf("serve-%d.log", time.Now().UnixNano()))
	lf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lf.Close() })
	g := &Gateway{Dir: dir, URL: "http://" + addr + "/v1", log: &bytes.Buffer{}}
	g.cmd = exec.Command(gatehouse, "serve", "--config", filepath.Join(dir, "config.yaml"))
	g.cmd.Stdout, g.cmd.Stderr = lf, lf
	if err := g.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Stop() })
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r1, e1 := http.Get("http://" + addr + "/healthz")
		r2, e2 := http.Get(adminBase + "/admin/")
		if e1 == nil {
			r1.Body.Close()
		}
		if e2 == nil {
			r2.Body.Close()
		}
		if e1 == nil && e2 == nil {
			b, _ := os.ReadFile(filepath.Join(dir, "local.token"))
			g.Token = strings.TrimSpace(string(b))
			return g, logPath
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(logPath)
	t.Fatalf("gateway did not start:\n%s", b)
	return nil, ""
}

var setupCodeRe = regexp.MustCompile(`setup code ([A-Z0-9]{10})`)

func TestG13TokenQuota(t *testing.T) {
	rep := newReport(t, "G13-token-quota", "G13 首次设置与 Token 配额",
		"验证无管理员时用启动日志中的一次性设置码在网页创建账号；sk- Key 的每日/累计 Token 配额：越线后下一次 429 token_quota_exceeded 并带 Retry-After，重启后由用量账本继续生效，控制台“清零用量”和 CLI 调整配额立即解除；统计接口返回聚合数据。")

	fake := startFake(t, "p")
	adminAddr := freePort(t)
	adminBase := "http://" + adminAddr
	cfg := fmt.Sprintf(g13Config, adminAddr, fake.URL)
	dir := t.TempDir()
	g, logPath := startGatewayLogFile(t, dir, cfg, adminBase)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	adm := func(method, path string, body any) (int, map[string]any) {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, adminBase+path, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Gatehouse-Admin", "1")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		return resp.StatusCode, obj
	}

	// 1. First-run setup with the one-time code from the server log.
	var code string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && code == ""; time.Sleep(50 * time.Millisecond) {
		b, _ := os.ReadFile(logPath)
		if m := setupCodeRe.FindSubmatch(b); m != nil {
			code = string(m[1])
		}
	}
	rep.Check("未设密码时启动日志打印一次性设置码", code != "", "len=%d", len(code))
	st, body := adm(http.MethodGet, "/admin/api/setup", nil)
	rep.Check("GET /admin/api/setup → setup_required=true", st == 200 && body["setup_required"] == true, "status=%d body=%v", st, body)
	st, _ = adm(http.MethodPost, "/admin/api/setup", map[string]string{"code": "WRONGCODE2", "password": "setup-pass-1"})
	rep.Check("错误设置码 → 401", st == 401, "status=%d", st)
	st, _ = adm(http.MethodPost, "/admin/api/setup", map[string]string{"code": strings.ToLower(code), "password": "short"})
	rep.Check("密码少于 8 位 → 400", st == 400, "status=%d", st)
	st, _ = adm(http.MethodPost, "/admin/api/setup", map[string]string{"code": strings.ToLower(code), "password": "setup-pass-1"})
	stMe, _ := adm(http.MethodGet, "/admin/api/me", nil)
	rep.Check("正确设置码（大小写不敏感）→ 200 并直接登录", st == 200 && stMe == 200, "setup=%d me=%d", st, stMe)
	st, _ = adm(http.MethodPost, "/admin/api/setup", map[string]string{"code": code, "password": "setup-pass-2"})
	rep.Check("账号已存在后再次设置 → 409", st == 409, "status=%d", st)

	ask := func(gw *Gateway, key string, stream bool) Response {
		return gw.Chat(t, key, map[string]any{"model": "p/model-a", "stream": stream, "messages": msgs("user", "quota probe")}, nil)
	}
	createKey := func(args ...string) (plain, id string) {
		out := g.CLI(t, append([]string{"apikey", "create", "--json"}, args...)...)
		var v struct {
			Key    string `json:"key"`
			APIKey struct {
				ID string `json:"id"`
			} `json:"api_key"`
		}
		if i := strings.Index(out, "{"); i < 0 || json.Unmarshal([]byte(out[i:]), &v) != nil {
			t.Fatalf("apikey create output: %s", out)
		}
		return v.Key, v.APIKey.ID
	}
	keyView := func(id string) map[string]any {
		_, body := adm(http.MethodGet, "/admin/api/keys", nil)
		list, _ := body["keys"].([]any)
		for _, k := range list {
			if m, _ := k.(map[string]any); m["id"] == id {
				return m
			}
		}
		return nil
	}

	// 2. Daily quota: the crossing request completes, the next one is refused.
	kDay, kDayID := createKey("--name", "day1", "--tokens-daily", "1")
	r1 := ask(g, kDay, false)
	r2 := ask(g, kDay, false)
	ra, _ := strconv.Atoi(r2.Header.Get("Retry-After"))
	rep.Check("每日配额：越线那次 200，下一次 429 token_quota_exceeded",
		r1.Status == 200 && r2.Status == 429 && r2.ErrCode() == "token_quota_exceeded",
		"statuses=%d,%d code=%s", r1.Status, r2.Status, r2.ErrCode())
	rep.Check("每日配额 429 带 Retry-After（到本地午夜，≤ 86400 秒）", ra > 0 && ra <= 86400, "retry_after=%d", ra)
	n := len(fake.Requests())
	_ = ask(g, kDay, false)
	rep.Check("配额用尽后请求不到达上游", len(fake.Requests()) == n, "upstream requests %d → %d", n, len(fake.Requests()))
	kv := keyView(kDayID)
	today, _ := kv["tokens_today"].(float64)
	rep.Check("控制台密钥列表显示今日 Token 用量", today > 1, "tokens_today=%v", kv["tokens_today"])

	st, _ = adm(http.MethodPost, "/admin/api/keys/"+kDayID+"/reset-usage", nil)
	r3 := ask(g, kDay, false)
	rep.Check("控制台“清零用量”后同一把 Key 立即恢复 200（不换密钥）", st == 200 && r3.Status == 200, "reset=%d after=%d", st, r3.Status)

	// 3. Total quota counts streaming usage too.
	kTot, kTotID := createKey("--name", "total1", "--tokens-total", "1")
	s1 := ask(g, kTot, true)
	s2 := ask(g, kTot, false)
	rep.Check("累计配额：流式请求的用量也计入，下一次 429",
		s1.Status == 200 && s2.Status == 429 && s2.ErrCode() == "token_quota_exceeded",
		"stream=%d next=%d code=%s", s1.Status, s2.Status, s2.ErrCode())
	rep.Check("累计配额 429 不带 Retry-After（不会自动恢复）", s2.Header.Get("Retry-After") == "", "retry_after=%q", s2.Header.Get("Retry-After"))

	// 4. Admin PATCH validates limits.
	st, _ = adm(http.MethodPatch, "/admin/api/keys/"+kTotID, map[string]any{"tokens_daily": -5})
	rep.Check("负数配额被拒绝 → 400", st == 400, "status=%d", st)

	// 5. Stats endpoint aggregates the traffic above.
	st, stats := adm(http.MethodGet, "/admin/api/stats?days=1", nil)
	totals, _ := stats["totals"].(map[string]any)
	reqs, _ := totals["requests"].(float64)
	limited, _ := totals["rate_limited"].(float64)
	byKey, _ := stats["by_key"].([]any)
	rep.Check("统计接口返回请求数、限流数与按 Key 排行", st == 200 && reqs >= 5 && limited >= 3 && len(byKey) >= 2,
		"status=%d requests=%v rate_limited=%v by_key=%d", st, totals["requests"], totals["rate_limited"], len(byKey))

	st, statsM := adm(http.MethodGet, "/admin/api/stats?days=1&model=p/model-a", nil)
	totalsM, _ := statsM["totals"].(map[string]any)
	limitedM, _ := totalsM["rate_limited"].(float64)
	rep.Check("按模型筛选时保留路由前被拒的请求（配额 429 仍计入该模型）", st == 200 && limitedM == limited,
		"status=%d rate_limited all=%v model=%v", st, limited, totalsM["rate_limited"])

	// 5b. Saving the general section from the console keeps OTLP headers it does not edit.
	_, cfgView := adm(http.MethodGet, "/admin/api/config", nil)
	hash, _ := cfgView["hash"].(string)
	st, _ = adm(http.MethodPut, "/admin/api/config/sections/general", map[string]any{
		"base_hash": hash,
		"data":      map[string]any{"max_body_mb": 16, "meter": map[string]any{"otlp": map[string]any{"endpoint": "http://127.0.0.1:9", "interval": "2h"}}},
	})
	saved, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	rep.Check("控制台保存常规参数后 OTLP headers 不丢失", st == 200 && strings.Contains(string(saved), "X-Collector-Tenant: g13-e2e") && strings.Contains(string(saved), "max_body_mb: 16"),
		"status=%d headers_kept=%v", st, strings.Contains(string(saved), "X-Collector-Tenant"))

	// 6. Restart: the ledger keeps the total quota enforced.
	g.Stop()
	g2, _ := startGatewayLogFile(t, dir, cfg, adminBase)
	s3 := ask(g2, kTot, false)
	rep.Check("重启网关后累计配额仍生效 → 429", s3.Status == 429 && s3.ErrCode() == "token_quota_exceeded", "status=%d code=%s", s3.Status, s3.ErrCode())

	// 7. Raising the quota from the CLI lifts the block on the running gateway.
	g2.CLI(t, "apikey", "update", kTotID, "--tokens-total", "0")
	s4 := ask(g2, kTot, false)
	rep.Check("CLI 把累计配额改为 0（不限）后恢复 200", s4.Status == 200, "status=%d", s4.Status)
}
