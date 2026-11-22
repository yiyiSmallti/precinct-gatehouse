package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const g7Config = `
listen: {{LISTEN}}
mode: personal
%sproviders:
  - name: p
    base_url: %s
    target: local
    models: [model-a, model-b]
default_group: g
routing_groups:
  g:
    members: [p/model-a]
`

func startGatewayInDir(t *testing.T, dir, cfg string) *Gateway {
	t.Helper()
	addr := freePort(t)
	cfg = strings.ReplaceAll(cfg, "{{LISTEN}}", addr)
	cfg = strings.ReplaceAll(cfg, "{{DIR}}", filepath.ToSlash(dir))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{Dir: dir, URL: "http://" + addr + "/v1", log: &bytes.Buffer{}}
	g.cmd = exec.Command(gatehouse, "serve", "--config", filepath.Join(dir, "config.yaml"))
	g.cmd.Stdout, g.cmd.Stderr = g.log, g.log
	if err := g.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Stop() })
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			b, _ := os.ReadFile(filepath.Join(dir, "local.token"))
			g.Token = strings.TrimSpace(string(b))
			return g
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("gateway did not start:\n%s", g.log.String())
	return nil
}

func TestG7KeysAndLimits(t *testing.T) {
	rep := newReport(t, "G7-keys-limits", "G7 多 Key 与次数限额",
		"验证 CLI 管理 `sk-` API Key、启用/禁用/重置、RPM/RPD/并发限额、重启后 RPD 恢复、ght1 并发不再串行，以及请求体上限配置。")

	fake := startFake(t, "p")
	cfgTpl := fmt.Sprintf(g7Config, "", fake.URL)
	g := startGateway(t, cfgTpl, nil)

	ask := func(gw *Gateway, tok, model, msg string) Response {
		return gw.Chat(t, tok, map[string]any{"model": model, "messages": msgs("user", msg)}, nil)
	}
	createKey := func(gw *Gateway, args ...string) (plain, id string) {
		out := gw.CLI(t, append([]string{"apikey", "create", "--json"}, args...)...)
		var v struct {
			Key    string `json:"key"`
			APIKey struct {
				ID string `json:"id"`
			} `json:"api_key"`
		}
		idx := strings.Index(out, "{")
		if idx < 0 || json.Unmarshal([]byte(out[idx:]), &v) != nil {
			t.Fatalf("apikey create output: %s", out)
		}
		return v.Key, v.APIKey.ID
	}

	// 1. CLI create + model scope + disable/enable + reset
	k1, k1ID := createKey(g, "--name", "dev-a", "--models", "p/model-a")
	r := ask(g, k1, "p/model-a", "hi")
	rep.Check("CLI 创建 sk- key 并调用允许模型 → 200", strings.HasPrefix(k1, "sk-") && r.Status == 200, "prefix=%s status=%d", k1[:9], r.Status)

	r = ask(g, k1, "p/model-b", "hi")
	rep.Check("越权模型 → 403", r.Status == 403 && r.ErrCode() == "model_not_allowed", "status=%d code=%s", r.Status, r.ErrCode())

	g.CLI(t, "apikey", "disable", k1ID)
	r = ask(g, k1, "p/model-a", "after disable")
	rep.Check("disable 后 → 401 key_disabled", r.Status == 401 && r.ErrCode() == "key_disabled", "status=%d code=%s", r.Status, r.ErrCode())

	g.CLI(t, "apikey", "enable", k1ID)
	r = ask(g, k1, "p/model-a", "after enable")
	rep.Check("enable 后恢复 → 200", r.Status == 200, "status=%d", r.Status)

	out := g.CLI(t, "apikey", "reset", k1ID, "--json")
	var resetOut struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &resetOut); err != nil {
		t.Fatalf("reset output: %s", out)
	}
	oldRes := ask(g, k1, "p/model-a", "old key")
	newRes := ask(g, resetOut.Key, "p/model-a", "new key")
	rep.Check("reset 后旧 key 401、新 key 200", oldRes.Status == 401 && newRes.Status == 200, "old=%d new=%d", oldRes.Status, newRes.Status)

	// 2. RPM=2
	kRPM, _ := createKey(g, "--name", "rpm2", "--rpm", "2")
	r1 := ask(g, kRPM, "p/model-a", "rpm 1")
	r2 := ask(g, kRPM, "p/model-a", "rpm 2")
	r3 := ask(g, kRPM, "p/model-a", "rpm 3")
	ra, _ := strconv.Atoi(r3.Header.Get("Retry-After"))
	rep.Check("RPM=2：第 3 次 429 rate_limit_exceeded 且带 Retry-After",
		r1.Status == 200 && r2.Status == 200 && r3.Status == 429 && r3.ErrCode() == "rate_limit_exceeded" && ra > 0,
		"statuses=%d,%d,%d code=%s retry_after=%s", r1.Status, r2.Status, r3.Status, r3.ErrCode(), r3.Header.Get("Retry-After"))

	// 3. Concurrency=1 with #slow
	kConc, _ := createKey(g, "--name", "conc1", "--concurrency", "1")
	var wg sync.WaitGroup
	concRes := make([]Response, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			concRes[idx] = ask(g, kConc, "p/model-a", fmt.Sprintf("#slow %d", idx))
		}(i)
		if i == 0 {
			time.Sleep(150 * time.Millisecond)
		}
	}
	wg.Wait()
	s200, s429 := 0, 0
	for _, cr := range concRes {
		if cr.Status == 200 {
			s200++
		} else if cr.Status == 429 && cr.ErrCode() == "concurrency_limit_exceeded" {
			s429++
		}
	}
	rep.Check("Concurrency=1：并发两次 #slow 一成功一 429", s200 == 1 && s429 == 1, "200=%d 429=%d", s200, s429)

	// 4. Upstream 503 failure does not consume daily quota
	kFailQuota, _ := createKey(g, "--name", "rpd1-fail", "--rpd", "1")
	fake.SetDown(true)
	rFail := ask(g, kFailQuota, "p/model-a", "during outage")
	fake.SetDown(false)
	rAfter := ask(g, kFailQuota, "p/model-a", "after recovery")
	rep.Check("上游 503 失败不占每日额度", rFail.Status == 502 && rAfter.Status == 200, "during_outage=%d after=%d", rFail.Status, rAfter.Status)

	// 5. ght1 token with --max-tokens runs concurrently (no whole-request lock)
	outTok := g.CLI(t, "token", "issue", "--name", "conc-ght", "--max-tokens", "10000", "--ttl", "1h", "--json")
	var tokOut struct{ Token string }
	if err := json.Unmarshal([]byte(outTok[strings.Index(outTok, "{"):]), &tokOut); err != nil {
		t.Fatalf("token issue: %s", outTok)
	}
	start := time.Now()
	var wgTok sync.WaitGroup
	tokStatuses := make([]int, 2)
	for i := 0; i < 2; i++ {
		wgTok.Add(1)
		go func(idx int) {
			defer wgTok.Done()
			tokStatuses[idx] = ask(g, tokOut.Token, "p/model-a", fmt.Sprintf("#slow ght %d", idx)).Status
		}(i)
	}
	wgTok.Wait()
	elapsed := time.Since(start)
	rep.Check("带 --max-tokens 的 ght1 并发两次 #slow < 1.8s（不再串行）",
		tokStatuses[0] == 200 && tokStatuses[1] == 200 && elapsed < 1800*time.Millisecond,
		"statuses=%v elapsed=%v", tokStatuses, elapsed.Round(time.Millisecond))

	// 6. RPD=3 + persistence across restart (separate gateway directory so DB is clean)
	rpdDir := t.TempDir()
	gRPD := startGatewayInDir(t, rpdDir, cfgTpl)
	kRPD, _ := createKey(gRPD, "--name", "rpd3", "--rpd", "3")
	var rpdOK int
	for i := 0; i < 3; i++ {
		if ask(gRPD, kRPD, "p/model-a", fmt.Sprintf("rpd %d", i)).Status == 200 {
			rpdOK++
		}
	}
	rRPD4 := ask(gRPD, kRPD, "p/model-a", "rpd 4")
	gRPD.Stop()
	gRPD2 := startGatewayInDir(t, rpdDir, cfgTpl)
	rRPD5 := ask(gRPD2, kRPD, "p/model-a", "rpd 5 after restart")
	rep.Check("RPD=3：第 4 次 429，重启网关后仍是 429",
		rpdOK == 3 && rRPD4.Status == 429 && rRPD4.ErrCode() == "daily_limit_exceeded" && rRPD5.Status == 429 && rRPD5.ErrCode() == "daily_limit_exceeded",
		"ok=%d 4th=%d(%s) 5th_restart=%d(%s)", rpdOK, rRPD4.Status, rRPD4.ErrCode(), rRPD5.Status, rRPD5.ErrCode())

	// 7. max_body_mb: default 32MB rejects 40MB -> 413; max_body_mb: 0 allows 40MB -> 200
	bigText := strings.Repeat("a", 40<<20)
	bigPayload := map[string]any{"model": "p/model-a", "messages": msgs("user", bigText)}
	rDefBig := g.Chat(t, resetOut.Key, bigPayload, nil)
	gUnlim := startGateway(t, fmt.Sprintf(g7Config, "max_body_mb: 0\n", fake.URL), nil)
	kUnlim, _ := createKey(gUnlim, "--name", "unlim")
	rUnlimBig := gUnlim.Chat(t, kUnlim, bigPayload, nil)
	rep.Check("默认 32MB 限制 40MB→413；max_body_mb: 0 允许 40MB→200",
		rDefBig.Status == 413 && rUnlimBig.Status == 200,
		"default=%d unlimited=%d", rDefBig.Status, rUnlimBig.Status)

	rep.Note("API Key 列表：\n\n```\n%s```", g.CLI(t, "apikey", "list"))
}
