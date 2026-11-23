# G7 多 Key 与次数限额

结果：**通过** · 运行于 2026-11-22 20:38:37 PST · darwin/arm64

验证 CLI 管理 `sk-` API Key、启用/禁用/重置、RPM/RPD/并发限额、重启后 RPD 恢复、ght1 并发不再串行，以及请求体上限配置。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| CLI 创建 sk- key 并调用允许模型 → 200 | ✅ | prefix=sk-KjBe2m status=200 |
| 越权模型 → 403 | ✅ | status=403 code=model_not_allowed |
| disable 后 → 401 key_disabled | ✅ | status=401 code=key_disabled |
| enable 后恢复 → 200 | ✅ | status=200 |
| reset 后旧 key 401、新 key 200 | ✅ | old=401 new=200 |
| RPM=2：第 3 次 429 rate_limit_exceeded 且带 Retry-After | ✅ | statuses=200,200,429 code=rate_limit_exceeded retry_after=60 |
| Concurrency=1：并发两次 #slow 一成功一 429 | ✅ | 200=1 429=1 |
| 上游 503 失败不占每日额度 | ✅ | during_outage=502 after=200 |
| 带 --max-tokens 的 ght1 并发两次 #slow < 1.8s（不再串行） | ✅ | statuses=[200 200] elapsed=1.005s |
| RPD=3：第 4 次 429，重启网关后仍是 429 | ✅ | ok=3 4th=429(daily_limit_exceeded) 5th_restart=429(daily_limit_exceeded) |
| 默认 32MB 限制 40MB→413；max_body_mb: 0 允许 40MB→200 | ✅ | default=413 unlimited=200 |

API Key 列表：

```
ID                    NAME       PREFIX     STATE    RPM/RPD/CONC  TOKENS DAY/MONTH/TOTAL  MODELS     GROUPS  EXPIRES
key_c5757f180851e5e2  rpd1-fail  sk-wwJP1z  enabled  0/1/0         0/0/0                                      never
key_be6a2d2661defcfb  conc1      sk-es5G1a  enabled  0/0/1         0/0/0                                      never
key_8bbcf94d3f568fc8  rpm2       sk-zjCxbj  enabled  2/0/0         0/0/0                                      never
key_88d8351cf3d4b967  dev-a      sk-LTCexE  enabled  0/0/0         0/0/0                   p/model-a          never
```
