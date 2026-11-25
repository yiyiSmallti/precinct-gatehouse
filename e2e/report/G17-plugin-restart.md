# G17 插件崩溃自动重启

结果：**通过** · 运行于 2026-11-25 00:06:36 PST · darwin/arm64

每个插件由 supervisor 监控，进程退出后按指数退避重启（1 s 起、翻倍、上限 30 s；稳定运行 30 s 后回到 1 s），重启成功后原子替换 client 与实现。假插件收到指令即 `os.Exit(3)`：检测器 `#crash-detector`、Provider `#crash-provider`、Hooks `#crash-hook`、策略令牌 `tok-crash`。断言崩溃窗口内 fail-closed、上游收不到请求，退避后恢复，`/healthz` 报告状态与重启次数，关停后不留子进程。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 启动：/healthz 列出插件状态 | ✅ | status=ok ner=running/restarts=0 plugprov=running/restarts=0；子进程 2 个 |
| 基线：人名经检测器插件脱敏并回填 | ✅ | status=200 客户端收到：[cloud/strong] 请提醒张三明天开会 |
| 检测器崩溃 → 窗口内 503 redaction_unavailable，外部未收到请求 | ✅ | 崩溃那次 503/redaction_unavailable；紧接着 503/redaction_unavailable；外部新增 0 |
| 窗口内 /healthz：degraded，ner 在重启 | ✅ | {"config_version":1,"detectors_unavailable":["ner"],"mode":"personal","plugins":[{"name":"plugprov","kind":"provider","state":"running","restarts":0},{"name":"ner","kind":"detector","state":"restarting","restarts":0}],"status":"degraded","version":"0.1.0-dev"} |
| 退避后恢复 200 并正确脱敏、回填 | ✅ | 崩溃后 1.1 s 恢复（首次退避 1 s）；客户端收到：[cloud/strong] 请提醒张三明天开会 |
| 恢复后 /healthz：ok，ner 重启 1 次 | ✅ | status=ok ner=running/restarts=1 |
| 短时间内再次崩溃 → 退避翻倍 | ✅ | 第二次 2.1 s 恢复（退避 2 s） |
| Provider 插件崩溃 → 按可重试错误处理（直连 502，组内故障转移到下一个成员） | ✅ | 直连 502/upstream_unavailable；组请求 200 model=local/small |
| Provider 插件恢复 | ✅ | 1.1 s 后 status=200 plugprov=running/restarts=1 |
| 重启写日志 | ✅ | 日志含 exited/restarted 记录 |
| 重启后旧进程不残留 | ✅ | 启动时 [14686 14690]，三次崩溃后 [14754 14766] |
| 网关关停后无子进程残留、不再重启 | ✅ | 残留 [] |
| org 基线：策略与 Hooks 正常 | ✅ | status=200 客户端收到：[local/a] org 请求 [hooked:市场部] |
| 策略插件崩溃 → 窗口内 503 policy_unavailable，不用 5 s 策略缓存，上游未收到请求 | ✅ | 崩溃那次 503；紧接着（tok-int 刚缓存过）503/policy_unavailable；上游新增 0 |
| 窗口内 /healthz：unavailable，policy.available=false | ✅ | {"config_version":1,"detectors_unavailable":null,"mode":"org","plugins":[{"name":"tagger","kind":"hooks","state":"running","restarts":0},{"name":"cloister","kind":"auth_policy","state":"restarting","restarts":0}],"policy":{"available":false,"plugin":"cloister"},"status":"unavailable","version":"0.1.0-dev"} |
| 策略插件退避后恢复 | ✅ | 1.1 s 后 status=200 cloister=running/restarts=1 |
| Hooks 插件崩溃 → 窗口内 503，上游未收到请求 | ✅ | 崩溃那次 503/hook_failed；紧接着 503/hook_unavailable |
| Hooks 插件恢复并继续改写请求 | ✅ | 1.1 s 后 status=200 客户端收到：[local/a] org 请求 [hooked:市场部] |
| org 网关关停后无子进程残留 | ✅ | 残留 [] |
