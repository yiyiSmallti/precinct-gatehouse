# G9 Web 控制面板与管理 API

结果：**通过** · 运行于 2026-11-22 21:19:16 PST · darwin/arm64

验证管理员密码设置、登录鉴权与防暴力破解、CSRF 头校验、API Key 在线增改启停立即生效、请求日志、概览/用量/渠道/脱敏策略接口以及内嵌前端页面。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| CLI 设置管理员密码 | ✅ | admin set-password 成功 |
| 未登录访问 API → 401 | ✅ | status=401 |
| 错误密码 → 401 且同 IP 失败 5 次后 → 429 | ✅ | bad=401 rate_limited=429 |
| 正确密码登录拿到 cookie | ✅ | login=200 me=200 |
| 不带 X-Gatehouse-Admin 的 POST → 403 | ✅ | status=403 |
| Web 创建 key 调用成功 + PATCH rpm=2 无需重发即刻生效 | ✅ | r1=200 patch=200 r2=200 r3=429 |
| Web 禁用 key 后立即 → 401 | ✅ | patch=200 status=401 code=key_disabled |
| /admin/api/logs 返回请求记录 | ✅ | status=200 count=4 |
| /overview、/usage、/channels 返回 200 且结构正确 | ✅ | ov=200(hourly=24) use=200(rows=1) ch=200(provs=2) |
| GET /admin/ 返回含 js/app.js 的 HTML | ✅ | html=200 js=200(24003 bytes) |
| GET /admin/api/redaction 返回矩阵 local/internal=off, external/internal=mask | ✅ | status=200 local=off external=mask |
| POST /admin/api/redaction/try 脱敏手机号且校验 CSRF | ✅ | csrf=403 status=200 phone=1 text="请联系 {{PHONE_4u2mqcsv}}" |
