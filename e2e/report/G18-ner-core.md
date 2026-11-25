# G18 NER 接入前的核心能力

结果：**通过** · 运行于 2026-11-25 01:47:14 PST · darwin/arm64

检测器插件 `ner`（e2e/fakedetector）模拟模型检测器：张三/李四/王五 记 PERSON、置信度 0.99，赵六 记 PERSON、置信度 0.6。`categories` 把 person 与 secret 都设为 block。网关 A 给 `ner` 配 `min_confidence: {default: 0.8}`、`max_action: mask`、`timeout: 500ms`、`on_error: rules_only`；网关 B 只配 `timeout: 500ms`（默认 fail_closed、无动作上限）作对照，再热重载为 `max_action: audit` 做影子运行。插件调用次数取自插件写的调用记录（只记长度）。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| max_action: mask：person 设为 block 时 NER 结果只脱敏，不拦截 | ✅ | status=200，外部收到：[{"content":"请通知{{PERSON_amqryjol}}和赵六明天参会","role":"user"}] |
| min_confidence 0.8：置信度 0.6 的赵六不处理 | ✅ | 外部收到：[{"content":"请通知{{PERSON_amqryjol}}和赵六明天参会","role":"user"}] |
| 回填人名 | ✅ | 客户端收到：[cloud/strong] 请通知张三和赵六明天参会 |
| 规则命中不受 NER 上限影响：secret 仍拦截 | ✅ | status=400 code=redaction_blocked |
| 第二轮只扫新增的 2 条消息（首条用缓存） | ✅ | status=200，本轮插件调用 2 次，外部收到：[{"content":"请通知{{PERSON_amqryjol}}和赵六明天参会","role":"user"},{"content":"[cloud/strong] 请通知{{PERSON_amqryjol}}和赵六明天参会","role":"assistant"},{"content":"再提醒{{PERSON_zfz4chnk}}","role":"user"}] |
| 第三轮 5 条消息仍只调用 2 次 | ✅ | status=200，本轮插件调用 2 次 |
| /healthz 报告缓存命中 | ✅ | detect_cache=map[entries:6 hits:4 misses:6] |
| 热重载生效 | ✅ |  |
| 阈值降到 0.5 后赵六被脱敏，且没有重新调用插件 | ✅ | status=200，插件调用 0 次，外部收到：[{"content":"请通知{{PERSON_amqryjol}}和{{PERSON_5jojo6qg}}明天参会","role":"user"}] |
| 审计样本记录来源与置信度 | ✅ | 0.99 的样本：map[action:mask confidence:0.99 kind:PERSON length:2 source:ner]；重载后 0.6 的样本：map[action:mask confidence:0.6 kind:PERSON length:2 source:ner] |
| 审计不含原文 | ✅ | 审计 3350 字节 |
| 插件超时 → 继续发送，规则照常脱敏手机号 | ✅ | status=200，外部收到：[{"content":"背景资料 #hang-detector","role":"system"},{"content":"张三的电话 {{PHONE_veymelad}}","role":"user"},{"content":"收到","role":"assistant"},{"content":"李四的电话 {{PHONE_undnksav}}","role":"user"}] |
| 整请求只等一次超时，后续文本跳过插件 | ✅ | 耗时 503ms，插件调用 1 次 |
| 退化后人名按规则处理（规则不识别人名，原样发出） | ✅ | 这是 rules_only 的代价，记在审计告警里 |
| 审计记录退化告警 | ✅ |  |
| 插件进程崩溃 → 仍发送，手机号已脱敏 | ✅ | status=200 code= |
| 重启窗口内请求不返回 503 | ✅ | status=200，外部收到：[{"content":"李四 {{PHONE_undnksav}}","role":"user"}] |
| 插件重启后恢复 NER 脱敏 | ✅ | running=true status=200，外部收到：[{"content":"{{PERSON_zfz4chnk}}明天到","role":"user"}] |
| 对照：无 max_action 时 person: block 拦截 NER 结果 | ✅ | status=400 code=redaction_blocked |
| 对照：默认 fail_closed 下插件超时 → 503 | ✅ | status=503 code=redaction_unavailable，外部收到 0 次 |
| 热重载为影子运行（max_action: audit） | ✅ |  |
| 影子运行：NER 结果只计数、原样发出，规则照常脱敏 | ✅ | status=200，X-Gatehouse-Redaction: mask; person=1; phone=1，外部收到：[{"content":"张三的电话 {{PHONE_oy5hacky}}","role":"user"}] |
| 影子运行的审计样本：source=ner、action=audit | ✅ | [{"action":"audit","confidence":0.99,"kind":"PERSON","length":2,"source":"ner"},{"action":"mask","kind":"PHONE","length":11,"source":"rules"}] |
| 故障转移到第二个候选时不再等检测器超时 | ✅ | status=200 模型=cloud/strong，第一个候选收到 1 次，插件调用 1 次，耗时 507ms |
| 最终审计记录仍含退化告警（失败发生在第一个候选） | ✅ |  |
| 意图路由：分类器脱敏时插件超时，正式请求不再等第二次 | ✅ | status=200，分类器调用 1 次，插件调用 1 次，耗时 506ms |
| 审计记录含分类器阶段的退化告警 | ✅ |  |
| 插件返回 NaN 置信度：按未打分处理，照常脱敏且审计照常写入 | ✅ | status=200，审计新增 1 条，样本：map[action:mask kind:PERSON length:2 source:ner] |
| 检测器插件不能命名为 rules / dictionary（否则其 max_action 会作用到内置规则） | ✅ | - plugins.rules: name is reserved for the built-in detector |
