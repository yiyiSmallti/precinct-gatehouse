# G14 文档块、Codex 输入项与二进制工具输出（issue #11 补充需求）

结果：**通过** · 运行于 2026-11-22 22:34:14 PST · darwin/arm64

假上游记录网关转发的 Chat Completions 请求。覆盖 `/v1/messages` 的 `document` 块与 `tool_result` 图片、`/v1/responses` 的 `input_file`、`custom` 工具与 Codex 输入项、assistant 文本与工具调用合并，以及所有协议共用的二进制工具输出拦截。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| `/v1/messages` document：docx 解出中文正文、PDF 解出文本、无法解析的 PDF 换成提示 | ✅ | status=200 text="[文档：报告.docx]\n季度报告正文：营收增长百分之十二。\n第二段 结束\n\n[文档：report.pdf]\n\nGatehouse quarterly PDF revenue\n[网关提示：收到未解析的二进制文档（scan.pdf，application/pdf，30729 字节），请先提取纯文本或转为图片后再发送]\n总结这几个文件\n" |
| `/v1/messages` 不向上游透传文档原始 base64 | ✅ | docx=false pdf=false bad=false |
| `/v1/messages` tool_result 图片拆到该批 tool 之后的 user 消息，tool_result 里的 PDF 转为文本 | ✅ | roles=user,assistant,tool,tool,user,user tool1="截图完成" tool2="[文档：tool.pdf]\n\nGatehouse quarterly PDF revenue" image_ok=true |
| `/v1/responses` 含 compaction/web_search_call/item_reference/custom_tool_call/local_shell_call 不再 400，跳过项写入 `X-Gatehouse-Dropped-Items` | ✅ | status=200 dropped_items="web_search_call, item_reference" body={"created_at":1795415654,"id":"resp_b36c3b05d7bd5cc9","model":"gpt-5","object":"response","output":[{"content":[{"annotations":[],"text":"[ds/deepseek |
| assistant 文本与随后的工具调用合并为一条 assistant 消息；custom/local_shell 调用映射为 tool_calls | ✅ | roles=user,user,assistant,tool,tool,tool,user content=我先读文件再改补丁 calls=read,apply_patch,local_shell args=["{}" "{\"input\":\"*** Begin Patch\\n*** End Patch\"}" "{\"command\":[\"ls\"],\"type\":\"exec\"}"] |
| compaction 摘要保留为文本；input_file（消息与工具输出）解析为文本且无原始 base64；custom_tool_call_output 成为 tool 消息 | ✅ | compaction="[Summary of the earlier conversation]\n之前讨论了季度报告" user="[文档：报告.docx]\n季度报告正文：营收增长百分之十二。\n第二段 结束\n读一下" tool_read="[文档：tool.pdf]\n\nGatehouse quarterly PDF revenue" tool_patch="Done!" |
| `custom` 工具转为单参数 `input` 的 function，描述带上 grammar | ✅ | function=true dropped_tools="" |
| 非流式：对 custom 工具的调用还原为 `custom_tool_call`，input 为原始文本 | ✅ | status=200 item=map[call_id:call_1 id:fc_5705c116139417c3_0 input:改一下 README name:apply_patch status:completed type:custom_tool_call] |
| 流式：custom 工具发 `custom_tool_call_input.delta/done`，不发 `function_call_arguments.*` | ✅ | status=200 input="改一下 README" item_type=custom_tool_call |
| 乱码（U+FFFD 占比高）工具输出被替换为网关提示 | ✅ | status=200 len=446 head="[网关保护：该工具输出疑似原始二进制文件（如 .docx/.pdf，约 45760 字节），不可按纯文本阅读，请改用 p" |
| `PK\x03\x04` 开头且后接非可打印流的工具输出被替换 | ✅ | head="[网关保护：该工具输出疑似原始二进制文件（如 .docx/.pdf，约 11344 字节），不可按纯文本阅读，请改用 p" |
| 正常中文/代码/彩色日志（24 KiB）不受影响，响应头只计 2 条 | ✅ | normal_intact=true header="2" |
