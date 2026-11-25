package e2e

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const g16Config = `
listen: {{LISTEN}}
mode: personal
providers:
  - name: cloud
    base_url: %s
    target: external
    models: [strong]
redaction:
  mode_by_target: { local: audit, external: mask }
  terms: ["天穹计划"]
  vault_persist: %v
  vault_ttl: %s
`

const (
	g16Phone = "13800138000"
	g16Term  = "天穹计划"
)

func TestG16VaultPersist(t *testing.T) {
	rep := newReport(t, "G16-vault-persist", "G16 Vault 加密落盘",
		"`redaction.vault_persist: true`：占位符对照表写穿到 `<data_dir>/gatehouse.db`（`vault_buckets` / `vault_entries`，值为 AES-256-GCM 密文，密钥由本机 `user.key` 派生）。外部目标 `cloud` 为 mask 模式，规则识别手机号、词典识别 `天穹计划`。第一轮带明文请求，随后完整重启 gatehouse（同一 data_dir），第二轮用户只发上一轮的占位符，假上游原样回显，断言网关从磁盘回填。")
	cloud := startFake(t, "cloud")
	sess := map[string]string{"X-Gatehouse-Session": "g16-s1"}
	first := map[string]any{"model": "cloud/strong", "messages": msgs("user", "请联系"+g16Phone+"，项目"+g16Term+"明天启动")}

	// 1. round one, then a full restart
	cfg := fmt.Sprintf(g16Config, cloud.URL, true, "24h")
	g := startGateway(t, cfg, nil)
	dir := g.Dir
	r := g.Chat(t, g.Token, first, sess)
	sent := cloud.ChatRequests()
	up := lastUser(sent[len(sent)-1])
	phs := placeholderRe.FindAllString(up, -1)
	phone, term := firstOf(phs, "{{PHONE_"), firstOf(phs, "{{TERM_")
	rep.Check("第一轮：上游只见占位符，客户端收到回填", r.Status == 200 && phone != "" && term != "" && !strings.Contains(up, g16Phone) && !strings.Contains(up, g16Term) && strings.Contains(r.Content(), g16Phone) && strings.Contains(r.Content(), g16Term),
		"上游收到：%q；客户端收到：%q", up, r.Content())
	rows := g16Count(t, dir, `SELECT COUNT(*) FROM vault_entries`)
	rep.Check("对照表写入 gatehouse.db", rows == 2, "vault_entries 行数=%d", rows)
	leak := g16Plaintext(dir)
	rep.Check("数据库文件（含 -wal/-shm）里搜不到明文", leak == "", "命中：%s", nz(leak))
	g.Stop()

	g = startGatewayIn(t, dir, "config.yaml", cfg, nil)
	echo := "上一轮的号码是 " + phone + "，项目是 " + term
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", echo)}, sess)
	sent = cloud.ChatRequests()
	up = lastUser(sent[len(sent)-1])
	rep.Check("完整重启后：假上游回显上一轮占位符 → 回填为明文", r.Status == 200 && strings.Contains(up, phone) && strings.Contains(r.Content(), g16Phone) && strings.Contains(r.Content(), g16Term) && !strings.Contains(r.Content(), "{{"),
		"上游收到：%q；客户端收到：%q；X-Gatehouse-Redaction: %s", up, r.Content(), header(r, "X-Gatehouse-Redaction"))
	g.Chat(t, g.Token, first, map[string]string{"X-Gatehouse-Session": "g16-again"})
	sent = cloud.ChatRequests()
	again := lastUser(sent[len(sent)-1])
	rep.Check("重启后同一明文得到同一占位符（user.key 跨重启不变）", strings.Contains(again, phone) && strings.Contains(again, term), "重启后上游收到：%q", again)
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", echo)}, map[string]string{"X-Gatehouse-Session": "g16-other"})
	rep.Check("其他会话拿同样占位符 → 不回填（桶按用户+会话隔离）", r.Status == 200 && strings.Contains(r.Content(), phone) && !strings.Contains(r.Content(), g16Phone),
		"客户端收到：%q", r.Content())

	// 2. two processes on one database, as during an FD handoff upgrade
	g2 := startGatewayIn(t, dir, "config-b.yaml", cfg, nil)
	const n = 12
	var wg sync.WaitGroup
	results := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			src, dst := g, g2
			if i%2 == 1 {
				src, dst = g2, g
			}
			phoneI := fmt.Sprintf("139%08d", 10000000+i)
			s := map[string]string{"X-Gatehouse-Session": fmt.Sprintf("g16-par-%d", i)}
			marker := fmt.Sprintf("号码#%d ", i)
			a := src.Chat(t, src.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", marker+phoneI)}, s)
			if a.Status != 200 || !strings.Contains(a.Content(), phoneI) {
				results[i] = fmt.Sprintf("#%d 写入方 status=%d", i, a.Status)
				return
			}
			ph := ""
			for _, req := range cloud.ChatRequests() {
				if u := lastUser(req); strings.HasPrefix(u, marker) {
					ph = placeholderRe.FindString(u)
				}
			}
			b := dst.Chat(t, dst.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "回显 "+ph)}, s)
			if b.Status != 200 || !strings.Contains(b.Content(), phoneI) {
				results[i] = fmt.Sprintf("#%d 另一进程 status=%d 内容=%q", i, b.Status, b.Content())
			}
		}(i)
	}
	wg.Wait()
	var bad []string
	for _, r := range results {
		if r != "" {
			bad = append(bad, r)
		}
	}
	logs := g.log.String() + g2.log.String()
	rep.Check(fmt.Sprintf("两个进程共用 gatehouse.db：%d 组并发请求一边写一边读，全部回填", n), len(bad) == 0 && !g16VaultErr.MatchString(logs),
		"失败：%s", nz(strings.Join(bad, "；")))
	g2.Stop()

	// 3. nothing in plaintext anywhere on disk or in logs
	g.Stop()
	leak = g16Plaintext(dir)
	_, audit := g.AuditRecords(t)
	rep.Check("重启与并发之后数据库仍无明文", leak == "", "命中：%s", nz(leak))
	rep.Check("审计与网关日志不含明文", !strings.Contains(audit, g16Phone) && !strings.Contains(audit, g16Term) && !strings.Contains(logs, g16Phone) && !strings.Contains(logs, g16Term),
		"审计 %d 字节，日志 %d 字节", len(audit), len(logs))

	// 4. TTL: an idle bucket expires on disk too
	short := fmt.Sprintf(g16Config, cloud.URL, true, "2s")
	g = startGatewayIn(t, dir, "config.yaml", short, nil)
	sessT := map[string]string{"X-Gatehouse-Session": "g16-ttl"}
	g.Chat(t, g.Token, first, sessT)
	before := g16Count(t, dir, `SELECT COUNT(*) FROM vault_entries`)
	g.Stop()
	time.Sleep(2500 * time.Millisecond)
	g = startGatewayIn(t, dir, "config.yaml", short, nil)
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", echo)}, sessT)
	after := g16Count(t, dir, `SELECT COUNT(*) FROM vault_entries`)
	rep.Check("vault_ttl 过期：重启后不回填，磁盘行同步删除", r.Status == 200 && strings.Contains(r.Content(), phone) && !strings.Contains(r.Content(), g16Phone) && after < before,
		"过期前 vault_entries=%d，过期后=%d；客户端收到：%q", before, after, r.Content())
	g.Stop()

	// 5. control: without vault_persist the restart loses the mapping
	cloud2 := startFake(t, "cloud")
	off := fmt.Sprintf(g16Config, cloud2.URL, false, "24h")
	g = startGateway(t, off, nil)
	g.Chat(t, g.Token, first, sess)
	sent = cloud2.ChatRequests()
	up = lastUser(sent[len(sent)-1])
	phs = placeholderRe.FindAllString(up, -1)
	phoneOff := firstOf(phs, "{{PHONE_")
	dirOff := g.Dir
	g.Stop()
	g = startGatewayIn(t, dirOff, "config.yaml", off, nil)
	r = g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "上一轮的号码是 "+phoneOff)}, sess)
	rows = g16Count(t, dirOff, `SELECT COUNT(*) FROM vault_entries`)
	rep.Check("对照：vault_persist 关闭（默认）时重启后占位符不回填、不落盘", phoneOff != "" && strings.Contains(r.Content(), phoneOff) && !strings.Contains(r.Content(), g16Phone) && rows == 0,
		"客户端收到：%q；vault_entries=%d", r.Content(), rows)
	rep.Note("占位符哈希键 `user.key` 保存在 data_dir（0600），重启后不变，所以同一值在重启前后得到同一占位符；落盘的桶 id 与密文密钥都由它派生（HMAC 域分离）。第 2 步用两个 `gatehouse serve` 进程同时打开同一个 data_dir 来模拟 FD 热切换期间新旧进程共用 gatehouse.db。")
}

var g16VaultErr = regexp.MustCompile(`vault (write|read|touch|sweep) failed|database is locked|SQLITE_BUSY`)

func g16Count(t *testing.T, dir, q string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "gatehouse.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		return -1
	}
	return n
}

// g16Plaintext returns the files in dir/gatehouse.db* that contain a plaintext value.
func g16Plaintext(dir string) string {
	files, _ := filepath.Glob(filepath.Join(dir, "gatehouse.db*"))
	var hits []string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, v := range []string{g16Phone, g16Term} {
			if bytes.Contains(b, []byte(v)) {
				hits = append(hits, filepath.Base(f)+":"+v)
			}
		}
	}
	return strings.Join(hits, ", ")
}

func nz(s string) string {
	if s == "" {
		return "无"
	}
	return s
}
