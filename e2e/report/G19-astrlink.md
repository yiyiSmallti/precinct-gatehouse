# G19 AstrLink NER 插件（真实模型）

结果：**通过** · 运行于 2026-11-25 02:14:15 PST · darwin/arm64

`plugins/astrlink-detector` 加载 AstrLink Guard int8 模型（ONNX Runtime），作为检测器插件接入真实 `gatehouse serve`。阈值、动作上限与超时退化都在网关 `redaction.sources.astrlink` 设置：`min_confidence {default 0.8, person 0.9, date 0.9, account 0.7}`、`max_action: mask`、`timeout: 30s`、`on_error: rules_only`；类别 `secret: block` 只对规则生效。本测试需要本机模型与运行库（`ASTRLINK_DIR`、`ORT_LIB`），CI 只编译插件。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 插件启动并加载模型 | ✅ | 从启动网关到插件就绪 1.182s |
| 中文人名、地址与英文人名被脱敏 | ✅ | status=200，耗时 20ms，X-Gatehouse-Redaction: mask; address=1; person=2，外部收到：[{"content":"帮我给{{PERSON_vdmiq5kp}}写封邮件，他住在{{ADDRESS_dhroioi7}}。另外问下 {{PERSON_xhzpowiw}} 那个 PR 的进度：\n```go\nfunc (s *AccountService) Disable(ctx context.Context, id int64) error { return nil }\n```","role":"user"}] |
| 代码块里的标识符原样保留 | ✅ |  |
| 响应回填原文 | ✅ | 客户端收到：[cloud/strong] 帮我给王建国写封邮件，他住在杭州市西湖区文澜东路88号翠微花园3栋2单元1201室。另外问下 Emily Carter 那个 PR 的进度：<br>```go<br>func (s *AccountService) Disable(ctx context.Context, id int64) error { return nil }<br>``` |
| 干净的编程请求原样发出 | ✅ | status=200，耗时 6ms，X-Gatehouse-Redaction: mask |
| 模型判出的密钥只脱敏、不拦截 | ✅ | status=200，X-Gatehouse-Redaction: mask; ner_secret=1 |
| 规则命中的密钥仍拦截 | ✅ | status=400 code=redaction_blocked |
| 长对话第二轮复用第一轮的检测结果 | ✅ | 文档约 77950 字符；第一轮 3.854s，第二轮 10ms，缓存命中 +1 |
| 审计样本带来源与置信度，不含原文 | ✅ | [{"action":"mask","confidence":0.9952463,"kind":"PERSON","length":3,"source":"astrlink"},{"action":"mask","confidence":0.9931157,"kind":"ADDRESS","length":27,"source":"astrlink"},{"action":"mask","confidence":0.99528074,"kind":"PERSON","length":12,"source":"astrlink"},{"action":"mask","confidence":0.99399364,"kind":"NER_SECRET","length":22,"source":"astrlink"},{"action":"block","confidence":0.9963013,"kind":"NER_SECRET","length":40,"source":"astrlink"},{"action":"mask","confidence":0.90768826,"kind":"NER_SECRET","length":7,"source":"astrlink"}] |

插件进程 RSS：260864 KiB
