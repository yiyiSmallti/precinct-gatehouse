# G13 首次设置与 Token 配额

结果：**通过** · 运行于 2026-11-22 21:09:49 PST · darwin/arm64

验证无管理员时用启动日志中的一次性设置码在网页创建账号；sk- Key 的每日/累计 Token 配额：越线后下一次 429 token_quota_exceeded 并带 Retry-After，重启后由用量账本继续生效，控制台“清零用量”和 CLI 调整配额立即解除；统计接口返回聚合数据。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 未设密码时启动日志打印一次性设置码 | ✅ | len=10 |
| GET /admin/api/setup → setup_required=true | ✅ | status=200 body=map[setup_required:true] |
| 错误设置码 → 401 | ✅ | status=401 |
| 密码少于 8 位 → 400 | ✅ | status=400 |
| 正确设置码（大小写不敏感）→ 200 并直接登录 | ✅ | setup=200 me=200 |
| 账号已存在后再次设置 → 409 | ✅ | status=409 |
| 每日配额：越线那次 200，下一次 429 token_quota_exceeded | ✅ | statuses=200,429 code=token_quota_exceeded |
| 每日配额 429 带 Retry-After（到本地午夜，≤ 86400 秒） | ✅ | retry_after=10212 |
| 配额用尽后请求不到达上游 | ✅ | upstream requests 1 → 1 |
| 控制台密钥列表显示今日 Token 用量 | ✅ | tokens_today=12 |
| 控制台“清零用量”后同一把 Key 立即恢复 200（不换密钥） | ✅ | reset=200 after=200 |
| 累计配额：流式请求的用量也计入，下一次 429 | ✅ | stream=200 next=429 code=token_quota_exceeded |
| 累计配额 429 不带 Retry-After（不会自动恢复） | ✅ | retry_after="" |
| 负数配额被拒绝 → 400 | ✅ | status=400 |
| 统计接口返回请求数、限流数与按 Key 排行 | ✅ | status=200 requests=6 rate_limited=3 by_key=2 |
| 按模型筛选时保留路由前被拒的请求（配额 429 仍计入该模型） | ✅ | status=200 rate_limited all=3 model=3 |
| 请求日志带缓存命中与输出速度所需字段（流式请求） | ✅ | cached=6 prompt=13 completion=7 ttft_ms=1002 latency_ms=1002 |
| 控制台保存常规参数后 OTLP headers 不丢失 | ✅ | status=200 headers_kept=true |
| 重启网关后累计配额仍生效 → 429 | ✅ | status=429 code=token_quota_exceeded |
| CLI 把累计配额改为 0（不限）后恢复 200 | ✅ | status=200 |
