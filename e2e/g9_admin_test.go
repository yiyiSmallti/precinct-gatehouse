package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const g9Config = `
listen: {{LISTEN}}
mode: personal
admin:
  listen: %s
  session_ttl: 1h
providers:
  - name: ds
    base_url: %s
    target: local
    models: [deepseek-chat]
  - name: cloud
    base_url: %s
    target: external
    models: [deepseek-chat]
default_group: main
routing_groups:
  main:
    members: [ds/deepseek-chat]
model_aliases:
  - match: "claude-sonnet-4-5"
    use: main
redaction:
  mode_by_target:
    local: off
    external: mask
`

func TestG9Admin(t *testing.T) {
	rep := newReport(t, "G9-admin", "G9 Web 控制面板与管理 API",
		"验证管理员密码设置、登录鉴权与防暴力破解、CSRF 头校验、API Key 在线增改启停立即生效、请求日志、概览/用量/渠道/脱敏策略接口以及内嵌前端页面。")

	fake := startFake(t, "ds")
	adminAddr := freePort(t)
	g := startGateway(t, fmt.Sprintf(g9Config, adminAddr, fake.URL, fake.URL), nil)
	adminBase := "http://" + adminAddr

	// Wait briefly for admin listener if still binding right after /healthz.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(adminBase + "/admin/"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 1. CLI set password via stdin
	cmd := exec.Command(gatehouse, "admin", "set-password", "--config", filepath.Join(g.Dir, "config.yaml"))
	cmd.Stdin = strings.NewReader("s3cr3t-pass!\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("admin set-password: %v\n%s", err, out)
	}
	rep.Check("CLI 设置管理员密码", true, "admin set-password 成功")

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	doAdm := func(method, path string, body any, extraHdr map[string]string) (int, map[string]any, string) {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, adminBase+path, rdr)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range extraHdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		return resp.StatusCode, obj, string(raw)
	}

	// 2. Unauthenticated -> 401
	sUnauth, _, _ := doAdm(http.MethodGet, "/admin/api/keys", nil, nil)
	rep.Check("未登录访问 API → 401", sUnauth == 401, "status=%d", sUnauth)

	// 3. Wrong password -> 401 + rate limit after 5 fails from same forwarded IP
	sBad, _, _ := doAdm(http.MethodPost, "/admin/api/login", map[string]string{"username": "admin", "password": "wrong"}, map[string]string{"X-Real-IP": "10.9.9.9"})
	for i := 0; i < 4; i++ {
		_, _, _ = doAdm(http.MethodPost, "/admin/api/login", map[string]string{"username": "admin", "password": "wrong"}, map[string]string{"X-Real-IP": "10.9.9.9"})
	}
	sRL, _, _ := doAdm(http.MethodPost, "/admin/api/login", map[string]string{"username": "admin", "password": "wrong"}, map[string]string{"X-Real-IP": "10.9.9.9"})
	rep.Check("错误密码 → 401 且同 IP 失败 5 次后 → 429", sBad == 401 && sRL == 429, "bad=%d rate_limited=%d", sBad, sRL)

	// 4. Correct login -> 200 + cookie
	sLogin, loginBody, _ := doAdm(http.MethodPost, "/admin/api/login", map[string]string{"username": "admin", "password": "s3cr3t-pass!"}, map[string]string{"X-Real-IP": "10.9.9.10"})
	sMe, meBody, _ := doAdm(http.MethodGet, "/admin/api/me", nil, nil)
	rep.Check("正确密码登录拿到 cookie", sLogin == 200 && loginBody["username"] == "admin" && sMe == 200 && meBody["username"] == "admin", "login=%d me=%d", sLogin, sMe)

	// 5. POST without X-Gatehouse-Admin -> 403
	sCSRF, _, _ := doAdm(http.MethodPost, "/admin/api/keys", map[string]any{"name": "csrf-test"}, nil)
	rep.Check("不带 X-Gatehouse-Admin 的 POST → 403", sCSRF == 403, "status=%d", sCSRF)

	// 6. Create key via API -> call /v1/chat/completions -> PATCH rpm -> 429 -> disable -> 401
	admHdr := map[string]string{"X-Gatehouse-Admin": "1"}
	sCreate, createBody, rawCreate := doAdm(http.MethodPost, "/admin/api/keys", map[string]any{
		"name":   "web-key",
		"models": []string{"ds/deepseek-chat"},
		"groups": []string{"main"},
	}, admHdr)
	plain, _ := createBody["key"].(string)
	ak, _ := createBody["api_key"].(map[string]any)
	keyID, _ := ak["id"].(string)
	if sCreate != 201 || plain == "" || keyID == "" {
		t.Fatalf("create key failed: status=%d body=%s", sCreate, rawCreate)
	}

	r1 := g.Chat(t, plain, map[string]any{"model": "ds/deepseek-chat", "messages": msgs("user", "hello web key")}, nil)
	sPatch, _, _ := doAdm(http.MethodPatch, "/admin/api/keys/"+keyID, map[string]any{"rpm": 2}, admHdr)
	r2 := g.Chat(t, plain, map[string]any{"model": "ds/deepseek-chat", "messages": msgs("user", "second")}, nil)
	r3 := g.Chat(t, plain, map[string]any{"model": "ds/deepseek-chat", "messages": msgs("user", "third")}, nil)
	rep.Check("Web 创建 key 调用成功 + PATCH rpm=2 无需重发即刻生效",
		r1.Status == 200 && sPatch == 200 && r2.Status == 200 && r3.Status == 429,
		"r1=%d patch=%d r2=%d r3=%d", r1.Status, sPatch, r2.Status, r3.Status)

	sDis, _, _ := doAdm(http.MethodPatch, "/admin/api/keys/"+keyID, map[string]any{"enabled": false}, admHdr)
	rDis := g.Chat(t, plain, map[string]any{"model": "ds/deepseek-chat", "messages": msgs("user", "after disable")}, nil)
	rep.Check("Web 禁用 key 后立即 → 401", sDis == 200 && rDis.Status == 401 && rDis.ErrCode() == "key_disabled",
		"patch=%d status=%d code=%s", sDis, rDis.Status, rDis.ErrCode())

	// 7. Logs contain previous requests
	sLogs, logsBody, _ := doAdm(http.MethodGet, "/admin/api/logs?key="+keyID+"&limit=10", nil, nil)
	logList, _ := logsBody["logs"].([]any)
	rep.Check("/admin/api/logs 返回请求记录", sLogs == 200 && len(logList) >= 4, "status=%d count=%d", sLogs, len(logList))

	// 8. Overview, Usage, Channels
	sOv, ovBody, _ := doAdm(http.MethodGet, "/admin/api/overview", nil, nil)
	ovToday, _ := ovBody["today"].(map[string]any)
	ovHourly, _ := ovBody["hourly"].([]any)
	sUse, useBody, _ := doAdm(http.MethodGet, "/admin/api/usage?by=key", nil, nil)
	useRows, _ := useBody["rows"].([]any)
	sCh, chBody, _ := doAdm(http.MethodGet, "/admin/api/channels", nil, nil)
	chProvs, _ := chBody["providers"].([]any)
	chAliases, _ := chBody["model_aliases"].([]any)
	rep.Check("/overview、/usage、/channels 返回 200 且结构正确",
		sOv == 200 && len(ovHourly) == 24 && ovToday["requests"].(float64) >= 4 &&
			sUse == 200 && len(useRows) >= 1 &&
			sCh == 200 && len(chProvs) == 2 && len(chAliases) == 1,
		"ov=%d(hourly=%d) use=%d(rows=%d) ch=%d(provs=%d)", sOv, len(ovHourly), sUse, len(useRows), sCh, len(chProvs))

	// 9. GET /admin/ HTML includes js/app.js and GET /admin/js/app.js serves JS
	respHTML, err := http.Get(adminBase + "/admin/")
	if err != nil {
		t.Fatal(err)
	}
	htmlBytes, _ := io.ReadAll(respHTML.Body)
	respHTML.Body.Close()
	respJS, err := http.Get(adminBase + "/admin/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	jsBytes, _ := io.ReadAll(respJS.Body)
	respJS.Body.Close()
	rep.Check("GET /admin/ 返回含 js/app.js 的 HTML",
		respHTML.StatusCode == 200 && strings.Contains(string(htmlBytes), "/admin/js/app.js") &&
			respJS.StatusCode == 200 && len(jsBytes) > 1000,
		"html=%d js=%d(%d bytes)", respHTML.StatusCode, respJS.StatusCode, len(jsBytes))

	// 10. Redaction info & live try
	sRed, redBody, _ := doAdm(http.MethodGet, "/admin/api/redaction", nil, nil)
	mat, _ := redBody["matrix"].(map[string]any)
	matLoc, _ := mat["local"].(map[string]any)
	matExt, _ := mat["external"].(map[string]any)
	rep.Check("GET /admin/api/redaction 返回矩阵 local/internal=off, external/internal=mask",
		sRed == 200 && matLoc["internal"] == "off" && matExt["internal"] == "mask",
		"status=%d local=%v external=%v", sRed, matLoc["internal"], matExt["internal"])

	sTryCSRF, _, _ := doAdm(http.MethodPost, "/admin/api/redaction/try", map[string]any{
		"text": "13812345678", "target": "external", "data_class": "internal",
	}, nil)
	sTry, tryBody, _ := doAdm(http.MethodPost, "/admin/api/redaction/try", map[string]any{
		"text": "请联系 13812345678", "target": "external", "data_class": "internal",
	}, admHdr)
	tryText, _ := tryBody["text"].(string)
	tryRep, _ := tryBody["report"].(map[string]any)
	tryCounts, _ := tryRep["counts"].(map[string]any)
	phoneCnt, _ := tryCounts["phone"].(float64)
	rep.Check("POST /admin/api/redaction/try 脱敏手机号且校验 CSRF",
		sTryCSRF == 403 && sTry == 200 && strings.Contains(tryText, "{{PHONE_") && phoneCnt == 1,
		"csrf=%d status=%d phone=%.0f text=%q", sTryCSRF, sTry, phoneCnt, tryText)
}
