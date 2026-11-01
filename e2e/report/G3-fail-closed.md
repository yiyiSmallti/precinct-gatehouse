# G3 fail-closed

结果：**通过** · 运行于 2026-11-01 16:23:20 UTC · linux/amd64

检测器插件 `ner`（e2e/fakedetector，go-plugin gRPC 子进程，识别人名 PERSON）被声明为必需（`redaction.detectors: [ner]`）。分三种情况：插件正常、配置里禁用、运行中进程被杀。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 插件正常：人名经插件脱敏 | ✅ | status=200，外部收到 1 次，X-Gatehouse-Redaction: mask; person=1 |
| 插件正常：回填人名 | ✅ | 客户端收到：[cloud/strong] 请提醒张三明天开会 |
| 运行中插件进程被杀 → 503 | ✅ | status=503 code=redaction_unavailable，外部新增请求 0 |
| 禁用检测器插件 → 503 | ✅ | status=503 code=redaction_unavailable message=map[code:redaction_unavailable message:required detector unavailable: ner; request not sent (fail-closed) type:api_error] |
| 外部假服务未收到任何请求 | ✅ | 外部收到 0 次 |
| /healthz 标出降级 | ✅ | {"detectors_unavailable":["ner"],"mode":"personal","status":"degraded"} |
| 本地目标 audit 模式不受影响 | ✅ | status=200（audit 只记不改，不要求检测器） |
