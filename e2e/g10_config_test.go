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
	"strings"
	"sync"
	"testing"
	"time"
)

const g10Config = `
listen: {{LISTEN}}
mode: personal
admin:
  listen: %s
  session_ttl: 1h
providers:
  - name: p1
    base_url: %s
    target: local
    models: [m1]
default_group: main
routing_groups:
  main:
    type: order
    members: [p1/m1]
redaction:
  mode_by_target:
    local: off
    external: mask
  categories:
    secret: block
`

func TestG10ConfigReload(t *testing.T) {
	rep := newReport(t, "G10-config-reload", "G10 配置热重载与在线配置管理",
		"验证分节保存、原始 YAML 保存、校验与冲突检测、外部文件监听自动重载、版本历史与回滚、Provider Key 数据库存储、测试连通性、在途请求无损以及门禁流统计。")

	fake1 := startFake(t, "p1")
	fake2 := startFake(t, "p2")
	adminAddr := freePort(t)
	g := startGateway(t, fmt.Sprintf(g10Config, adminAddr, fake1.URL), nil)
	adminBase := "http://" + adminAddr

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(adminBase + "/admin/"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	pwCmd := exec.Command(gatehouse, "admin", "set-password", "--config", filepath.Join(g.Dir, "config.yaml"))
	pwCmd.Stdin = strings.NewReader("admin12345\n")
	if out, err := pwCmd.CombinedOutput(); err != nil {
		t.Fatalf("admin set-password: %v\n%s", err, out)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	doAdm := func(method, path string, body any) (int, map[string]any, string) {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, adminBase+path, rdr)
		req.Header.Set("X-Gatehouse-Admin", "1")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return resp.StatusCode, m, string(raw)
	}

	sLogin, _, rawLogin := doAdm(http.MethodPost, "/admin/api/login", map[string]string{"username": "admin", "password": "admin12345"})
	if sLogin != 200 {
		t.Fatalf("login failed: %d %s", sLogin, rawLogin)
	}

	// Read initial config
	sCfg, cfg0, _ := doAdm(http.MethodGet, "/admin/api/config", nil)
	hash0, _ := cfg0["hash"].(string)
	ver0, _ := cfg0["version"].(float64)
	if sCfg != 200 || hash0 == "" {
		t.Fatalf("get config failed: %d", sCfg)
	}

	// 1. Add provider p2 via sections/providers -> works without restart, version +1, history backup created
	provs := []map[string]any{
		{"name": "p1", "base_url": fake1.URL, "target": "local", "models": []string{"m1"}},
		{"name": "p2", "base_url": fake2.URL, "target": "local", "models": []string{"m2"}},
	}
	sPutP, putPBody, rawPutP := doAdm(http.MethodPut, "/admin/api/config/sections/providers", map[string]any{
		"base_hash": hash0,
		"data":      provs,
	})
	ver1, _ := putPBody["version"].(float64)
	rP2 := g.Chat(t, g.Token, map[string]any{"model": "p2/m2", "messages": msgs("user", "hello p2")}, nil)
	_, histBody, _ := doAdm(http.MethodGet, "/admin/api/config/history", nil)
	histList, _ := histBody["history"].([]any)
	rep.Check("1. sections/providers 新增 p2 热生效，version+1 且产生备份",
		sPutP == 200 && ver1 == ver0+1 && rP2.Status == 200 && len(histList) == 1,
		"put=%d ver=%.0f->%.0f p2=%d backups=%d raw=%s", sPutP, ver0, ver1, rP2.Status, len(histList), rawPutP)

	firstBackup := ""
	if len(histList) > 0 {
		firstBackup, _ = histList[0].(map[string]any)["name"].(string)
	}

	// 2. sections/redaction sets local -> mask -> redaction/try and live chat immediately mask phone
	_, cfg1, _ := doAdm(http.MethodGet, "/admin/api/config", nil)
	hash1, _ := cfg1["hash"].(string)
	sPutRed, _, _ := doAdm(http.MethodPut, "/admin/api/config/sections/redaction", map[string]any{
		"base_hash": hash1,
		"data": map[string]any{
			"mode_by_target": map[string]string{"local": "mask", "external": "mask"},
			"categories":     map[string]string{"secret": "block"},
		},
	})
	_, tryBody, _ := doAdm(http.MethodPost, "/admin/api/redaction/try", map[string]any{
		"text": "手机 13812345678", "target": "local", "data_class": "internal",
	})
	tryText, _ := tryBody["text"].(string)
	rMaskChat := g.Chat(t, g.Token, map[string]any{"model": "p1/m1", "messages": msgs("user", "联系 13812345678")}, nil)
	upReqs := fake1.ChatRequests()
	lastMsg := ""
	if len(upReqs) > 0 {
		lastMsg = lastUser(upReqs[len(upReqs)-1])
	}
	rep.Check("2. sections/redaction 把 local 改为 mask 后 try 与真实请求立即脱敏",
		sPutRed == 200 && strings.Contains(tryText, "{{PHONE_") && rMaskChat.Status == 200 && strings.Contains(lastMsg, "{{PHONE_"),
		"put=%d try=%q upstream=%q", sPutRed, tryText, lastMsg)

	// 3. Invalid config (ftp://x) -> 400 with problems, old config stays active
	_, cfg2, _ := doAdm(http.MethodGet, "/admin/api/config", nil)
	hash2, _ := cfg2["hash"].(string)
	sBad, badBody, _ := doAdm(http.MethodPut, "/admin/api/config/sections/providers", map[string]any{
		"base_hash": hash2,
		"data":      []map[string]any{{"name": "p1", "base_url": "ftp://x", "models": []string{"m1"}}},
	})
	probs, _ := badBody["problems"].([]any)
	rStillOK := g.Chat(t, g.Token, map[string]any{"model": "p2/m2", "messages": msgs("user", "still ok")}, nil)
	rep.Check("3. 非法配置返回 400 + problems 且旧配置继续服务",
		sBad == 400 && badBody["error"] == "invalid_config" && len(probs) > 0 && rStillOK.Status == 200,
		"status=%d err=%v probs=%v p2=%d", sBad, badBody["error"], probs, rStillOK.Status)

	// 4. Stale base_hash -> 409 conflict
	sStale, staleBody, _ := doAdm(http.MethodPut, "/admin/api/config/sections/aliases", map[string]any{
		"base_hash": hash0,
		"data":      []map[string]any{{"match": "alias-x", "use": "p1/m1"}},
	})
	rep.Check("4. 过期 base_hash 返回 409 conflict",
		sStale == 409 && staleBody["error"] == "conflict" && staleBody["current_hash"] == hash2,
		"status=%d body=%v", sStale, staleBody)

	// 5. External edit to config.yaml -> auto-reloads within 5s with source="file"
	cfgPath := filepath.Join(g.Dir, "config.yaml")
	curBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	extBytes := append(curBytes, []byte("\nmodel_aliases:\n  - match: \"ext-alias\"\n    use: p1/m1\n")...)
	if err := os.WriteFile(cfgPath, extBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	fileReloaded := false
	for i := 0; i < 25; i++ {
		_, cNow, _ := doAdm(http.MethodGet, "/admin/api/config", nil)
		if cNow["source"] == "file" {
			fileReloaded = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	rAlias := g.Chat(t, g.Token, map[string]any{"model": "ext-alias", "messages": msgs("user", "via file alias")}, nil)
	rep.Check("5. 外部改写 config.yaml 在 5s 内自动生效且 source=file",
		fileReloaded && rAlias.Status == 200, "reloaded=%v alias_status=%d", fileReloaded, rAlias.Status)

	// 6. Rollback to first backup -> p2 disappears, p2/m2 -> 404
	sRB, _, _ := doAdm(http.MethodPost, "/admin/api/config/rollback", map[string]string{"name": firstBackup})
	rAfterRB := g.Chat(t, g.Token, map[string]any{"model": "p2/m2", "messages": msgs("user", "after rollback")}, nil)
	rep.Check("6. rollback 到首份备份后 p2 消失（p2/m2 → 404）",
		sRB == 200 && rAfterRB.Status == 404, "rollback=%d p2=%d", sRB, rAfterRB.Status)

	// 7. PUT provider key -> upstream receives Authorization: Bearer <key>, GET config has_key=true without plaintext
	const p1Secret = "upstream-secret-9988"
	sPK, _, _ := doAdm(http.MethodPut, "/admin/api/providers/p1/key", map[string]string{"key": p1Secret})
	_, cfgAfterKey, rawCfgAfterKey := doAdm(http.MethodGet, "/admin/api/config", nil)
	rWithKey := g.Chat(t, g.Token, map[string]any{"model": "p1/m1", "messages": msgs("user", "with key")}, nil)
	auths := fake1.AuthHeaders()
	lastAuth := ""
	if len(auths) > 0 {
		lastAuth = auths[len(auths)-1]
	}
	rawObj, _ := cfgAfterKey["raw"].(map[string]any)
	rawProvs, _ := rawObj["providers"].([]any)
	hasKey := false
	if len(rawProvs) > 0 {
		hasKey, _ = rawProvs[0].(map[string]any)["has_key"].(bool)
	}
	rep.Check("7. PUT provider key 生效且 GET config 不泄露明文",
		sPK == 200 && rWithKey.Status == 200 && lastAuth == "Bearer "+p1Secret && hasKey && !strings.Contains(rawCfgAfterKey, p1Secret),
		"put=%d auth=%q has_key=%v leaked=%v", sPK, lastAuth, hasKey, strings.Contains(rawCfgAfterKey, p1Secret))

	// 8. Modify listen via YAML -> succeeds and returns restart_required containing "listen"
	_, yamlGet, _ := doAdm(http.MethodGet, "/admin/api/config/yaml", nil)
	yamlStr, _ := yamlGet["yaml"].(string)
	yamlHash, _ := yamlGet["hash"].(string)
	// Replace listen line with a different port
	lines := strings.Split(yamlStr, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "listen:") {
			lines[i] = "listen: 127.0.0.1:19999"
			break
		}
	}
	sYamlPut, yamlPutBody, _ := doAdm(http.MethodPut, "/admin/api/config/yaml", map[string]string{
		"yaml":      strings.Join(lines, "\n"),
		"base_hash": yamlHash,
	})
	rrList, _ := yamlPutBody["restart_required"].([]any)
	hasListenRR := false
	for _, x := range rrList {
		if x == "listen" {
			hasListenRR = true
		}
	}
	rep.Check("8. 通过 YAML 修改 listen 成功且 restart_required 含 listen",
		sYamlPut == 200 && hasListenRR, "status=%d rr=%v", sYamlPut, rrList)

	// 9. In-flight #slow request completes 200 during concurrent hot reload
	var wg sync.WaitGroup
	var slowResp Response
	wg.Add(1)
	go func() {
		defer wg.Done()
		slowResp = g.Chat(t, g.Token, map[string]any{"model": "p1/m1", "messages": msgs("user", "#slow inflight")}, nil)
	}()
	time.Sleep(150 * time.Millisecond)
	_, cfgSlow, _ := doAdm(http.MethodGet, "/admin/api/config", nil)
	hSlow, _ := cfgSlow["hash"].(string)
	sReloadWhileSlow, _, _ := doAdm(http.MethodPut, "/admin/api/config/sections/aliases", map[string]any{
		"base_hash": hSlow,
		"data":      []map[string]any{{"match": "slow-alias", "use": "p1/m1"}},
	})
	wg.Wait()
	rep.Check("9. 热重载期间在途 #slow 请求正常 200 完成",
		sReloadWhileSlow == 200 && slowResp.Status == 200, "reload=%d slow=%d", sReloadWhileSlow, slowResp.Status)

	// 10. providers/test returns ok and latency
	sTest, testBody, _ := doAdm(http.MethodPost, "/admin/api/providers/test", map[string]string{"provider": "p1", "model": "m1"})
	testOK, _ := testBody["ok"].(bool)
	testLat, hasLat := testBody["latency_ms"].(float64)
	rep.Check("10. providers/test 返回 ok 与延迟",
		sTest == 200 && testOK && hasLat && testLat >= 0,
		"status=%d ok=%v latency=%.0fms", sTest, testOK, testLat)

	// 11. /admin/api/flow matches exact request counts across all gate stages
	// Generate controlled requests covering 401, 403, 429, redaction_blocked, upstream 502.
	outK := g.CLI(t, "apikey", "create", "--name", "flow-key", "--models", "p1/m1", "--rpm", "1", "--json")
	var fk struct{ Key string `json:"key"` }
	_ = json.Unmarshal([]byte(outK[strings.Index(outK, "{"):]), &fk)

	// 401 (bad key)
	_ = g.Chat(t, "sk-wrong-flow", map[string]any{"model": "p1/m1", "messages": msgs("user", "1")}, nil)
	// 403 (forbidden model)
	_ = g.Chat(t, fk.Key, map[string]any{"model": "main", "messages": msgs("user", "2")}, nil)
	// 200 (ok, uses the 1 rpm slot)
	_ = g.Chat(t, fk.Key, map[string]any{"model": "p1/m1", "messages": msgs("user", "3")}, nil)
	// 429 (rpm exceeded)
	_ = g.Chat(t, fk.Key, map[string]any{"model": "p1/m1", "messages": msgs("user", "4")}, nil)
	// redaction_blocked (secret: block via confidential? Wait: local redaction is off on initial rollback, use external? Or set local=mask with secret=block! Wait: let's add external or set local=mask via section first)
	_, cfgFlow, _ := doAdm(http.MethodGet, "/admin/api/config", nil)
	_, _, _ = doAdm(http.MethodPut, "/admin/api/config/sections/redaction", map[string]any{
		"base_hash": cfgFlow["hash"],
		"data": map[string]any{
			"mode_by_target": map[string]string{"local": "mask", "external": "mask"},
			"categories":     map[string]string{"secret": "block"},
		},
	})
	_ = g.Chat(t, g.Token, map[string]any{"model": "p1/m1", "messages": msgs("user", "password: supersecret123")}, nil)
	// upstream 502 (fake1 down)
	fake1.SetDown(true)
	_ = g.Chat(t, g.Token, map[string]any{"model": "p1/m1", "messages": msgs("user", "down")}, nil)
	fake1.SetDown(false)

	sFlow, flowBody, _ := doAdm(http.MethodGet, "/admin/api/flow?range=1h", nil)
	num := func(k string) int64 {
		v, _ := flowBody[k].(float64)
		return int64(v)
	}
	sumFlow := num("auth_rejected") + num("policy_rejected") + num("rate_limited") +
		num("redaction_blocked") + num("upstream_failed") + num("other_failed") + num("succeeded")
	rep.Check("11. /admin/api/flow 各阶段计数准确且总和等于 total",
		sFlow == 200 &&
			num("auth_rejected") == 1 &&
			num("policy_rejected") == 1 &&
			num("rate_limited") == 1 &&
			num("redaction_blocked") == 1 &&
			num("upstream_failed") == 1 &&
			num("other_failed") == 1 && // the 404 p2/m2 after rollback
			num("succeeded") >= 6 &&
			sumFlow == num("total"),
		"total=%d ok=%d 401=%d 403=%d 429=%d red=%d up=%d other=%d",
		num("total"), num("succeeded"), num("auth_rejected"), num("policy_rejected"),
		num("rate_limited"), num("redaction_blocked"), num("upstream_failed"), num("other_failed"))
}
