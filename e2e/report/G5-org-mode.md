# G5 org 模式

结果：**通过** · 运行于 2026-11-22 05:58:04 PST · darwin/arm64

`mode: org`，策略插件 `cloister` 用 e2e/fakeplugin 的假实现（按令牌返回租户、用户、部门与分级）。默认组 `order: [cloud/strong, local/a, local/b]`，cloud 是外部目标。同时挂上 AuditSink 插件（写文件）、Provider 插件（`plug/x`）和 Hooks 插件（在最后一条用户消息后加 `[hooked:<部门>]`）。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| confidential → 只走本地组 | ✅ | status=200 model=local/a，外部模型收到 0 次请求 |
| 部门与分级写入审计 | ✅ | tenant=org_acme user=u_zhang department=研发部 data_class=confidential policy_version=7 |
| Hooks 插件改写请求（带部门上下文） | ✅ | 上游收到："季度预算明细 [hooked:研发部]" |
| 请求头不能把分级降到策略之下 | ✅ | status=200 model=local/a |
| confidential 指定外部组 → 拒绝 | ✅ | status=403 code=no_allowed_target |
| internal → 可用外部强模型 | ✅ | status=200 model=cloud/strong |
| 本地组全部宕机 → 拒绝不降级 | ✅ | status=503 code=local_targets_unavailable，外部模型收到 0 次请求 |
| 策略拒绝令牌 → 401 | ✅ | status=401 code=invalid_token |
| 策略越权 → 403 | ✅ | status=403 code=policy_denied |
| 不在 allowed_groups 的组 → 403 | ✅ | status=403 code=model_not_allowed |
| allowed_groups 内的直连模型可用 | ✅ | status=200 model=local/b |
| 允许组的规则目标（不在 members 里）照常生效 | ✅ | status=200 model=local/b |
| 策略预算为 0 → 403 | ✅ | status=403 code=token_budget_exceeded |
| Provider 插件（非流式） | ✅ | status=200 |
| Provider 插件（流式） | ✅ | status=200 |
| Hooks 改 model → 拒绝 | ✅ | status=500 code=hook_invalid |
| /v1/models 按策略列出 | ✅ | {"data":[{"created":0,"id":"auto","object":"model","owned_by":"gatehouse"},{"created":0,"id":"default","object":"model","owned_by":"gatehouse"},{"created":0,"id":"ruled","object":"model","owned_by":"gatehouse"},{"created":0,"id":"local/a","object":"model","owned_by":"gatehouse"},{"created":0,"id":"local/b","object":"model","owned_by":"gatehouse"},{"created":0,"id":"cloud/strong","object":"model","owned_by":"gatehouse"},{"created":0,"id":"plug/x","object":"model","owned_by":"gatehouse"}],"object":"list"} |
| AuditSink 插件收到记录（与本地文件一致） | ✅ | 插件收到 14 条，本地文件 14 条 |
| 策略收回令牌后，网关在 10s 内拒绝（默认缓存） | ✅ | 收回前 status=200；收回后 5.1 秒时 status=401 |
| 策略插件进程退出 → 503（含已缓存的令牌） | ✅ | 崩溃那次 status=503；之后 status=503 code=policy_unavailable |
| healthz 报告策略不可用 | ✅ | {"detectors_unavailable":null,"mode":"org","policy":{"available":false,"plugin":"cloister"},"status":"unavailable"} |
| 策略插件禁用 → 503 | ✅ | status=503 code=policy_unavailable |
