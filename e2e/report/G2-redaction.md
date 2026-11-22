# G2 脱敏

结果：**通过** · 运行于 2026-11-22 06:13:02 PST · darwin/arm64

外部目标 `cloud/strong` 模式为 mask，本地目标为 audit。请求含手机号、身份证、银行卡、客户词典词「星辰科技」、订单号干扰项（11 位，形似手机号）、Never-mask 地址。断言外部假服务收到的请求体、回填后的响应与工具参数、审计文件。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 出站不含手机号 | ✅ | 外部假服务收到的请求体中 手机号 出现 0 次 |
| 出站不含身份证 | ✅ | 外部假服务收到的请求体中 身份证 出现 0 次 |
| 出站不含银行卡 | ✅ | 外部假服务收到的请求体中 银行卡 出现 0 次 |
| 出站不含词典词 | ✅ | 外部假服务收到的请求体中 词典词 出现 0 次 |
| 出站带占位符 | ✅ | 占位符 {{BANK_CARD_lyvhw6um}} {{ID_CARD_3qfxj7pn}} {{PHONE_a3whklux}} {{TERM_li72spfc}} |
| 订单号未被误脱 | ✅ | 订单号后的 11 位数字原样发出 |
| dsh_session_log 被剥掉 | ✅ | 该字段带完整明文会话日志，出站前删除 |
| Never-mask 原样 | ✅ | team@example.com 原样发出 |
| 工具描述与参数 schema 描述也脱敏 | ✅ | function.description 与 parameters.properties.*.description 中的手机号已替换，属性名保留 |
| 响应文本回填为原值 | ✅ | 客户端收到：[cloud/strong] 请给星辰科技的联系人回电 13812345678，身份证 110101199003071233，卡号 6222021234567890128。订单号 13900001111 已发货。抄送 team@example.com。 |
| tool_calls 参数回填且仍是合法 JSON | ✅ | arguments={"echo":"请给星辰科技的联系人回电 13812345678，身份证 110101199003071233，卡号 6222021234567890128。订单号 13900001111 已发货。抄送 team@example.com。"} |
| 响应头说明脱敏 | ✅ | X-Gatehouse-Redaction: mask; bank_card=1; id_card=1; phone=3; term=1 |
| 跨回合同一值同一占位符 | ✅ | {{PHONE_a3whklux}} 在第二回合出现 2 次 |
| 历史工具参数按 JSON 解码后脱敏 | ✅ | 出站 arguments={"company":"{{TERM_li72spfc}}","phone":"{{PHONE_a3whklux}}"} |
| 工具参数里带键名的密码仍按 secret 处理 | ✅ | status=400 code=redaction_blocked |
| 本地目标 audit：原样发出、只计数 | ✅ | X-Gatehouse-Redaction: audit; bank_card=1; id_card=1; phone=1; term=1 |
| 密钥类别 block → 400 且不出站 | ✅ | status=400 code=redaction_blocked message=map[code:redaction_blocked message:request blocked by redaction policy: secret(40) type:invalid_request_error] |
| confidential 不出外部（拒绝而不降级） | ✅ | status=403 code=no_allowed_target |
| 审计只含类别计数，无原文 | ✅ | 审计文件 7 条，泄漏值 []，含占位符=false |

首条审计记录的 redaction 字段：

```json
{"counts":{"bank_card":1,"id_card":1,"phone":3,"term":1},"masked":6,"mode":"mask","restored":8,"samples":[{"action":"mask","kind":"TERM","length":4},{"action":"mask","kind":"PHONE","length":11},{"action":"mask","kind":"ID_CARD","length":18},{"action":"mask","kind":"BANK_CARD","length":19},{"action":"mask","kind":"PHONE","length":11},{"action":"mask","kind":"PHONE","length":11}]}
```
