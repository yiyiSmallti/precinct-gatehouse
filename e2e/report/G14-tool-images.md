# G13 工具图片与大 base64（issue #11）

结果：**通过** · 运行于 2026-11-22 23:34:50 PST · darwin/arm64

本地目标开启 `mask` 脱敏。`/v1/responses` 一轮内 4 个并行工具调用分别以内容数组、单个图片对象、JSON 编码字符串、内嵌 data URI 文本返回图片，并带 Codex Desktop 的 `additional_tools` 输入项；另以 Chat Completions 发送夹着 ~3 MB base64 图片的文本。断言假上游收到的消息结构、脱敏结果与耗时。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| Responses 含 `additional_tools` 输入项与 7 路工具输出返回 200 | ✅ | status=200 body={"created_at":1795419290,"id":"resp_a838e3e7307e6be1","model":"gpt-5","object":"response","output":[{"content":[{"annotations":[],"text":"[ds/deepseek-chat] 继续","type":"output_text"}],"id":"msg_a838e3 |
| 消息顺序：7 条 tool 紧跟 assistant.tool_calls，图片在其后的一条 user 消息里 | ✅ | roles=user,assistant,tool,tool,tool,tool,tool,tool,tool,user,user |
| tool 消息只含纯文本，无 base64 | ✅ | a="截图 A，联系 {{PHONE_tm74d4hf}}" b="[image returned by tool; attached in the next user message]" c="截图 C" d="日志 D [image] 结束" e="{\"resource\":{\"text\":\"important tool result\",\"uri\":\"file:///report.txt\"},\"type\":\"resource\"}" f="[image returned by tool; attached in the next user message]" g="{\"ref\":\"shot-7\",\"type\":\"image\"}" |
| tool 文本中的手机号仍被脱敏 | ✅ | a="截图 A，联系 {{PHONE_tm74d4hf}}" |
| 6 张图片原样进入 user 消息的 image_url 块（含 detail、JSON 字符串里的 `image`+`url`、MCP `data`+`mimeType`）；无法转换的图片对象和未识别的内容块保留为 JSON 文本 | ✅ | images=6 big_intact=true detail_b="high" |
| `additional_tools` 中的函数工具并入上游 tools | ✅ | tools=screenshot,lookup |
| 工具续轮按用户原话分类路由，不被图片承载消息替代 | ✅ | status=200 rule=look model=ds/vision classifier_saw=["看图：这张截图里有什么"] |
| Chat 文本内 3 MB base64 在 mask 模式下不拖慢请求（< 1s，修复前本机 1.29s、线上 90s） | ✅ | status=200 elapsed=55ms |
| base64 前后（含紧跟同行、换行包裹图片之后）的手机号、身份证仍被脱敏，图片数据原样保留 | ✅ | phone_masked=true trailing_phone_masked=true id_masked=true image_intact=true wrapped_intact=true |
| ~2 MB 代码/日志历史在 mask 模式下脱敏耗时可控（< 400ms，修复前本机约 0.7s），其中的手机号、身份证全部打码 | ✅ | status=200 body=2575KB elapsed=84ms phone_left=false id_left=false phone_placeholders=6 |

耗时（含 HTTP 往返与假上游）：Responses 多路工具图片 26ms；Chat 内嵌 3 MB base64 55ms；~2 MB 历史 84ms。
