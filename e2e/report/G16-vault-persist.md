# G16 Vault 加密落盘

结果：**通过** · 运行于 2026-11-25 00:06:25 PST · darwin/arm64

`redaction.vault_persist: true`：占位符对照表写穿到 `<data_dir>/gatehouse.db`（`vault_buckets` / `vault_entries`，值为 AES-256-GCM 密文，密钥由本机 `user.key` 派生）。外部目标 `cloud` 为 mask 模式，规则识别手机号、词典识别 `天穹计划`。第一轮带明文请求，随后完整重启 gatehouse（同一 data_dir），第二轮用户只发上一轮的占位符，假上游原样回显，断言网关从磁盘回填。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 第一轮：上游只见占位符，客户端收到回填 | ✅ | 上游收到："请联系{{PHONE_ggegf2nv}}，项目{{TERM_253u5536}}明天启动"；客户端收到："[cloud/strong] 请联系13800138000，项目天穹计划明天启动" |
| 对照表写入 gatehouse.db | ✅ | vault_entries 行数=2 |
| 数据库文件（含 -wal/-shm）里搜不到明文 | ✅ | 命中：无 |
| 完整重启后：假上游回显上一轮占位符 → 回填为明文 | ✅ | 上游收到："上一轮的号码是 {{PHONE_ggegf2nv}}，项目是 {{TERM_253u5536}}"；客户端收到："[cloud/strong] 上一轮的号码是 13800138000，项目是 天穹计划"；X-Gatehouse-Redaction: mask |
| 重启后同一明文得到同一占位符（user.key 跨重启不变） | ✅ | 重启后上游收到："请联系{{PHONE_ggegf2nv}}，项目{{TERM_253u5536}}明天启动" |
| 其他会话拿同样占位符 → 不回填（桶按用户+会话隔离） | ✅ | 客户端收到："[cloud/strong] 上一轮的号码是 {{PHONE_ggegf2nv}}，项目是 {{TERM_253u5536}}" |
| 两个进程共用 gatehouse.db：12 组并发请求一边写一边读，全部回填 | ✅ | 失败：无 |
| 重启与并发之后数据库仍无明文 | ✅ | 命中：无 |
| 审计与网关日志不含明文 | ✅ | 审计 13775 字节，日志 5173 字节 |
| vault_ttl 过期：重启后不回填，磁盘行同步删除 | ✅ | 过期前 vault_entries=18，过期后=0；客户端收到："[cloud/strong] 上一轮的号码是 {{PHONE_ggegf2nv}}，项目是 {{TERM_253u5536}}" |
| 对照：vault_persist 关闭（默认）时重启后占位符不回填、不落盘 | ✅ | 客户端收到："[cloud/strong] 上一轮的号码是 {{PHONE_e3y7d5so}}"；vault_entries=0 |

占位符哈希键 `user.key` 保存在 data_dir（0600），重启后不变，所以同一值在重启前后得到同一占位符；落盘的桶 id 与密文密钥都由它派生（HMAC 域分离）。第 2 步用两个 `gatehouse serve` 进程同时打开同一个 data_dir 来模拟 FD 热切换期间新旧进程共用 gatehouse.db。
