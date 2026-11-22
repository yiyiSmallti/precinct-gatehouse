# G4 令牌

结果：**通过** · 运行于 2026-11-22 06:13:04 PST · darwin/arm64

`gatehouse token issue` 签发限范围的短时效令牌，网关按令牌校验签名、有效期、吊销与范围。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 本机令牌零配置可用 | ✅ | 首次 serve 自动签发 local.token |
| 限定模型 A：请求 A → 200 | ✅ | status=200 |
| 限定模型 A：请求 B → 403 | ✅ | status=403 code=model_not_allowed，上游未收到 |
| 限定模型 A：请求未授权的路由组 → 403 | ✅ | status=403 code=model_not_allowed |
| 限定路由组：请求组 → 200 | ✅ |  |
| 限定路由组：直连模型 → 403 | ✅ |  |
| 过期 → 401 | ✅ | status=401 code=token_expired |
| 吊销前可用 | ✅ |  |
| 吊销后 → 401 | ✅ | status=401 code=token_revoked |
| 无令牌 → 401 | ✅ | status=401 code=missing_token |
| 篡改签名 → 401 | ✅ | status=401 code=invalid_token |
| token 上限用尽 → 403 | ✅ | 第一次 200，第二次 403 token_budget_exceeded |
| 数据分级越权 → 403 | ✅ | 令牌上限 internal，请求 confidential：status=403 |
| 拒绝均有审计记录 | ✅ | 审计中 401/403 共 9 条 |

令牌列表：

```
ID                    NAME       EXPIRES                    STATE    SCOPE
tok_e1465b52ad14581e  local      never                      active   {"data_class":"confidential"}
tok_77fb9d64ba19a650  only-a     2026-11-22T07:13:03-08:00  active   {"models":["p/model-a"],"data_class":"internal"}
tok_8f455c5ba840c644  group-g    2026-11-22T07:13:03-08:00  active   {"groups":["g"],"data_class":"internal"}
tok_8093b4af970cc86c  short      2026-11-22T06:13:04-08:00  expired  {"data_class":"internal"}
tok_7a95a55df004c9f7  to-revoke  2026-11-22T07:13:04-08:00  revoked  {"data_class":"internal"}
tok_fa9011c27bb6ce35  budget     2026-11-22T07:13:04-08:00  active   {"max_tokens":5,"data_class":"internal"}
```
