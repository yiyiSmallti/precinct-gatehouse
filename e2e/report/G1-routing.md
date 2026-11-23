# G1 个人模式路由

结果：**通过** · 运行于 2026-11-22 21:09:49 PST · darwin/arm64

两个假提供方：`local`（本地目标，托管分类器 small、备用 backup）与 `cloud`（外部目标，强模型 strong）。默认组 `order: [cloud/strong, local/backup]`，三条规则：闲聊意图 → local/small，估算 token > 3000 → cloud/strong，多步推理/长文意图 → cloud/strong。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 闲聊 → 小模型 | ✅ | status=200 model=local/small rule=chat effort=low |
| useEffort 下发为 reasoning_effort | ✅ | local/small 收到 reasoning_effort=low |
| 长文/推理意图 → 强模型 | ✅ | status=200 model=cloud/strong rule=reasoning effort=medium |
| 长输入（token 规则）→ 强模型 | ✅ | status=200 model=cloud/strong rule=long-input |
| 长输入：分类器最多调用一次 | ✅ | 分类器调用 2 → 3（闲聊意图规则排在 token 规则之前，所以会问一次分类器） |
| 同一回合决策固定 | ✅ | 第一次 cloud/strong，工具回合后 cloud/strong；分类器调用 4 → 4 |
| 强模型宕机 → order 组切到备用 | ✅ | status=200 model=local/backup rule=reasoning |
| 冷却期内跳过故障模型 | ✅ | model=local/backup，强模型在冷却期内收到请求 0 次 |
| usage 组按用量分摊 | ✅ | 依次命中 local/a → local/b → local/a → local/b |
| usage 组同一回合不换模型 | ✅ | local/a → local/a |
| 本地目标流式透传 | ✅ | Content-Type=text/event-stream；客户端未要 usage，网关读取后丢弃 usage 块 |
| 外部目标整条缓冲、对客户端仍是 SSE | ✅ | 上游请求 stream=<nil>，客户端收到 SSE |
| 上游重定向不跟随 | ✅ | status=502，重定向目标收到请求 0 次 |
| 流未完成即结束 → 记为失败 | ✅ | 客户端 status=200（头已发出），审计记录 status=502 |

## 决策记录（来自审计文件）

| 请求模型 | 组 | 规则 | 理由 | 最终模型 | 尝试 |
| --- | --- | --- | --- | --- | --- |
| auto | default | chat | intent "闲聊、问候、简单问答" | local/small | local/small:200 |
| auto | default | reasoning | intent "多步推理或写长文" | cloud/strong | cloud/strong:200 |
| auto | default | long-input | tokens≈4004>3000 | cloud/strong | cloud/strong:200 |
| auto | default | reasoning | intent "多步推理或写长文" | cloud/strong | cloud/strong:200 |
| auto | default | reasoning | turn-cached: intent "多步推理或写长文" | cloud/strong | cloud/strong:200 |
| auto | default | reasoning | intent "多步推理或写长文" | local/backup | cloud/strong:503, local/backup:200 |
| auto | default | reasoning | intent "多步推理或写长文" | local/backup | local/backup:200 |
| pool | pool | - | no rule matched; group usage order | local/a | local/a:200 |
| pool | pool | - | no rule matched; group usage order | local/b | local/b:200 |
| pool | pool | - | no rule matched; group usage order | local/a | local/a:200 |
| pool | pool | - | no rule matched; group usage order | local/b | local/b:200 |
| pool | pool | - | no rule matched; group usage order | local/a | local/a:200 |
| pool | pool | - | turn-cached: no rule matched; group usage order | local/a | local/a:200 |
| local/small | - | - | direct model | local/small | local/small:200 |
| cloud/strong | - | - | direct model | cloud/strong | cloud/strong:200 |
| local/small | - | - | direct model | local/small | local/small:307 |
| local/small | - | - | direct model | local/small | local/small:200 |

