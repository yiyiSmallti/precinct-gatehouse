# G11 存量密钥导入与 New-API 用量接口兼容

结果：**通过** · 运行于 2026-11-22 21:09:44 PST · darwin/arm64

验证通过 stdin 导入既有 sk- 明文密钥（不回显明文、拒绝重复导入）、导入密钥正常调用 chat 接口，以及 GET /api/usage/token/（含无斜杠路径）返回 New-API 兼容格式。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 1. apikey import 导入 48 字符旧密钥成功且输出不含明文 | ✅ | err=<nil> out="imported key_9fbd6f5ac468a108 (default-prod, prefix sk-AbCdEf)\n" |
| 2. 导入的密钥可正常完成 chat 请求 (200) | ✅ | status=200 body={"choices":[{"finish_reason":"stop","index":0,"message":{"content":"[p/model-a] hello","role":"assistant"}}],"created":1795410584,"id":"c1","model":"model-a","object":"chat.completion","usage":{"completion_tokens":4,"prompt_tokens":6,"prompt_tokens_details":{"cached_tokens":3},"total_tokens":10}} |
| 3. 重复导入相同明文密钥被拒绝 | ✅ | err=exit status 1 out="gatehouse: key already exists\n" |
| 4. GET /api/usage/token/ 与无斜杠路径均返回 200 及 unlimited_quota=true | ✅ | slash=200({"code":true,"data":{"expires_at":0,"model_limits":{},"model_limits_enabled":false,"name":"default-prod","object":"token_usage","total_available":500000000,"total_granted":500000000,"total_used":0,"unlimited_quota":true,"user_quota":{"total_available":500000000,"total_used":0}},"message":"ok"}) noslash=200({"code":true,"data":{"expires_at":0,"model_limits":{},"model_limits_enabled":false,"name":"default-prod","object":"token_usage","total_available":500000000,"total_granted":500000000,"total_used":0,"unlimited_quota":true,"user_quota":{"total_available":500000000,"total_used":0}},"message":"ok"}) |
| 5. 错误密钥调用 /api/usage/token/ 返回 401 且 code=false | ✅ | status=401 raw={"code":false,"data":null,"message":"invalid token"} |
| 6. --rpd 5 导入密钥返回 unlimited_quota=false 与 total_granted=5 | ✅ | err=<nil> status=200 raw={"code":true,"data":{"expires_at":0,"model_limits":{},"model_limits_enabled":false,"name":"rpd-key","object":"token_usage","total_available":4,"total_granted":5,"total_used":1,"unlimited_quota":false,"user_quota":{"total_available":4,"total_used":1}},"message":"ok"} |
