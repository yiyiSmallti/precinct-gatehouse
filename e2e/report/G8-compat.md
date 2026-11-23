# G8 协议适配（Anthropic Messages & OpenAI Responses）

结果：**通过** · 运行于 2026-11-22 21:10:14 PST · darwin/arm64

验证 `/v1/messages`、`/v1/messages/count_tokens`、`/v1/responses` 的非流式/流式、thinking、tool_calls、别名路由、`system_prompt`/`extra_body` 注入及限额与 `request_log`。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| /v1/models 支持 sk- 且列出精确别名 | ✅ | status=200 |
| Anthropic 非流式 + x-api-key + thinking + 保留原 model | ✅ | status=200 model=claude-sonnet-4-5 think=true text=true |
| 上游收到 system_prompt 拼接与 extra_body | ✅ | sys="【网关规范】中文思考\n\n你是资深工程师。" extra=map[thinking:true] |
| Anthropic tools → tool_use（input 为对象） | ✅ | stop=tool_use id=call_1 input_is_obj=true |
| Anthropic 第二轮 tool_result → 上游 role:tool 且 tool_call_id 匹配 | ✅ | status=200 found_tool_msg=true |
| Anthropic 流式事件顺序与 thinking/text/tool_use delta | ✅ | events=[message_start content_block_start content_block_delta content_block_delta content_block_stop content_block_start content_block_delta content_block_delta content_block_stop content_block_start content_block_delta content_block_delta content_block_stop message_delta message_stop] json="{\"echo\":\"流式 #think\"}" |
| Anthropic 坏 key → 401 且为 Anthropic 错误格式 | ✅ | status=401 body={"error":{"message":"invalid api key","type":"authentication_error"},"type":"error"} |
| Anthropic count_tokens 返回数字且不调上游 | ✅ | status=200 input_tokens=21 |
| Responses 非流式 + instructions→system + 别名 gpt-* + 丢弃非函数工具头 + cached_tokens | ✅ | status=200 model=gpt-5 dropped="web_search" cached=14 |
| Responses function_call / function_call_output 转换 | ✅ | status=200 asst=true tool=true |
| Responses 流式事件顺序（reasoning + message + function_call + sequence_number） | ✅ | count=22 seqOK=true reason=true msg=true fn=true |
| Responses previous_response_id → 400 | ✅ | status=400 code=previous_response_not_supported |
| compat 协议受 RPM 限额约束（第 2 次 429 rate_limit_error） | ✅ | s1=200 s2=429 err=map[message:rate limit (1 rpm) exceeded for key; retry after 60s type:rate_limit_error] |
| request_log 记录了 messages/responses 协议及 cached_tokens | ✅ | messages=5 responses=3 cached_sum=136 |

Anthropic 流式事件序列：`message_start → content_block_start → content_block_delta → content_block_delta → content_block_stop → content_block_start → content_block_delta → content_block_delta → content_block_stop → content_block_start → content_block_delta → content_block_delta → content_block_stop → message_delta → message_stop`

Responses 流式事件序列：`response.created → response.in_progress → response.output_item.added → response.reasoning_summary_part.added → response.reasoning_summary_text.delta → response.reasoning_summary_text.delta → response.reasoning_summary_text.done → response.reasoning_summary_part.done → response.output_item.done → response.output_item.added → response.content_part.added → response.output_text.delta → response.output_text.delta → response.output_text.done → response.content_part.done → response.output_item.done → response.output_item.added → response.function_call_arguments.delta → response.function_call_arguments.delta → response.function_call_arguments.done → response.output_item.done → response.completed`
