# G13 工具图片与大 base64（issue #11）

结果：**通过** · 运行于 2026-11-22 22:08:37 PST · darwin/arm64

本地目标开启 `mask` 脱敏。`/v1/responses` 一轮内 4 个并行工具调用分别以内容数组、单个图片对象、JSON 编码字符串、内嵌 data URI 文本返回图片，并带 Codex Desktop 的 `additional_tools` 输入项；另以 Chat Completions 发送夹着 ~3 MB base64 图片的文本。断言假上游收到的消息结构、脱敏结果与耗时。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| Responses 含 `additional_tools` 输入项与 5 路工具图片返回 200 | ✅ | status=200 body={"created_at":1795414117,"id":"resp_8b7ab5e9fb9c4c9a","model":"gpt-5","object":"response","output":[{"content":[{"annotations":[],"text":"[ds/deepseek-chat] 继续","type":"output_text"}],"id":"msg_8b7ab5 |
| 消息顺序：5 条 tool 紧跟 assistant.tool_calls，图片在其后的一条 user 消息里 | ✅ | roles=user,assistant,tool,tool,tool,tool,tool,user,user |
| tool 消息只含纯文本，无 base64 | ✅ | a="截图 A，联系 {{PHONE_od2e7xfe}}" b="[image returned by tool; attached in the next user message]" c="截图 C" d="日志 D [image] 结束" e="[image returned by tool; attached in the next user message]" |
| tool 文本中的手机号仍被脱敏 | ✅ | a="截图 A，联系 {{PHONE_od2e7xfe}}" |
| 5 张图片原样进入 user 消息的 image_url 块（含 detail、JSON 字符串里的 `image`+`url` 别名） | ✅ | images=5 big_intact=true detail_b="high" |
| `additional_tools` 中的函数工具并入上游 tools | ✅ | tools=screenshot,lookup |
| 工具续轮按用户原话分类路由，不被图片承载消息替代 | ✅ | status=200 rule=look model=ds/vision classifier_saw=["看图：这张截图里有什么"] |
| Chat 文本内 3 MB base64 在 mask 模式下不拖慢请求（< 1s，修复前本机 1.29s、线上 90s） | ✅ | status=200 elapsed=56ms |
| base64 前后（含紧跟同行、换行包裹图片之后）的手机号、身份证仍被脱敏，图片数据原样保留 | ✅ | phone_masked=true trailing_phone_masked=true id_masked=true image_intact=true wrapped_intact=true |

耗时（含 HTTP 往返与假上游）：Responses 4 路工具图片 27ms；Chat 内嵌 3 MB base64 56ms。
