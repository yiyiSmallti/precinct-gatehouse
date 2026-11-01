# G4 令牌

结果：**通过** · 运行于 2026-11-01 16:34:59 UTC · linux/amd64

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
ID                    NAME       EXPIRES               STATE    SCOPE
tok_82158c7075bd415d  local      never                 active   {"data_class":"confidential"}
tok_362c6b289463a9a6  only-a     2026-11-01T17:34:58Z  active   {"models":["p/model-a"],"data_class":"internal"}
tok_8edd52af5bbd8fde  group-g    2026-11-01T17:34:58Z  active   {"groups":["g"],"data_class":"internal"}
tok_1436d1d95e5a7fab  short      2026-11-01T16:34:59Z  expired  {"data_class":"internal"}
tok_876c525e8939df7e  to-revoke  2026-11-01T17:34:59Z  revoked  {"data_class":"internal"}
tok_ff87f24d49d72bd1  budget     2026-11-01T17:34:59Z  active   {"max_tokens":5,"data_class":"internal"}
```
