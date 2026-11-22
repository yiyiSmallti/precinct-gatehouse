package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

const g11Config = `
listen: {{LISTEN}}
mode: personal
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

func getUsageToken(t *testing.T, baseRoot, path, token string) (int, map[string]any, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseRoot+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

func TestG11NewAPIMigrate(t *testing.T) {
	rep := newReport(t, "G11-newapi-migrate", "G11 存量密钥导入与 New-API 用量接口兼容",
		"验证通过 stdin 导入既有 sk- 明文密钥（不回显明文、拒绝重复导入）、导入密钥正常调用 chat 接口，以及 GET /api/usage/token/（含无斜杠路径）返回 New-API 兼容格式。")

	fake := startFake(t, "p")
	g := startGateway(t, fmt.Sprintf(g11Config, fake.URL), nil)
	baseRoot := strings.TrimSuffix(g.URL, "/v1")

	const importedPlain = "sk-AbCdEf0123456789GhIjKlMnOpQrStUvWxYz0123456789Ab"

	// 1. apikey import succeeds and never echoes plaintext
	impOut, impErr := g.CLIStdin(importedPlain+"\n", "apikey", "import", "--name", "default-prod", "--note", "from-newapi")
	rep.Check("1. apikey import 导入 48 字符旧密钥成功且输出不含明文",
		impErr == nil && !strings.Contains(impOut, importedPlain) && strings.Contains(impOut, "imported "),
		"err=%v out=%q", impErr, impOut)

	// 2. Imported key works for chat completion
	rChat := g.Chat(t, importedPlain, map[string]any{"model": "p/model-a", "messages": msgs("user", "hello")}, nil)
	rep.Check("2. 导入的密钥可正常完成 chat 请求 (200)",
		rChat.Status == 200, "status=%d body=%s", rChat.Status, rChat.Raw)

	// 3. Re-importing same key fails
	dupOut, dupErr := g.CLIStdin(importedPlain+"\n", "apikey", "import", "--name", "dup-prod")
	rep.Check("3. 重复导入相同明文密钥被拒绝",
		dupErr != nil && strings.Contains(dupOut, "already exists"),
		"err=%v out=%q", dupErr, dupOut)

	// 4. GET /api/usage/token/ and /api/usage/token return 200 with expected schema
	sSlash, mSlash, rawSlash := getUsageToken(t, baseRoot, "/api/usage/token/", importedPlain)
	dSlash, _ := mSlash["data"].(map[string]any)
	sNoSlash, mNoSlash, rawNoSlash := getUsageToken(t, baseRoot, "/api/usage/token", importedPlain)
	rep.Check("4. GET /api/usage/token/ 与无斜杠路径均返回 200 及 unlimited_quota=true",
		sSlash == 200 && mSlash["code"] == true && dSlash["name"] == "default-prod" &&
			dSlash["unlimited_quota"] == true && sNoSlash == 200 && mNoSlash["code"] == true,
		"slash=%d(%s) noslash=%d(%s)", sSlash, rawSlash, sNoSlash, rawNoSlash)

	// 5. Invalid key returns 401 with code=false
	sBad, mBad, rawBad := getUsageToken(t, baseRoot, "/api/usage/token/", "sk-invalid000000000000000000000000000000")
	rep.Check("5. 错误密钥调用 /api/usage/token/ 返回 401 且 code=false",
		sBad == 401 && mBad["code"] == false, "status=%d raw=%s", sBad, rawBad)

	// 6. Key imported with --rpd 5 reports unlimited_quota=false and total_granted=5
	const rpdPlain = "sk-RpdFive0123456789GhIjKlMnOpQrStUvWxYz0123456789"
	_, rpdErr := g.CLIStdin(rpdPlain+"\n", "apikey", "import", "--name", "rpd-key", "--rpd", "5")
	_ = g.Chat(t, rpdPlain, map[string]any{"model": "p/model-a", "messages": msgs("user", "count 1")}, nil)
	sRPD, mRPD, rawRPD := getUsageToken(t, baseRoot, "/api/usage/token/", rpdPlain)
	dRPD, _ := mRPD["data"].(map[string]any)
	granted, _ := dRPD["total_granted"].(float64)
	used, _ := dRPD["total_used"].(float64)
	rep.Check("6. --rpd 5 导入密钥返回 unlimited_quota=false 与 total_granted=5",
		rpdErr == nil && sRPD == 200 && dRPD["unlimited_quota"] == false && granted == 5 && used == 1,
		"err=%v status=%d raw=%s", rpdErr, sRPD, rawRPD)
}
