# G10 配置热重载与在线配置管理

结果：**通过** · 运行于 2026-11-22 06:32:35 PST · darwin/arm64

验证分节保存、原始 YAML 保存、校验与冲突检测、外部文件监听自动重载、版本历史与回滚、Provider Key 数据库存储、测试连通性、在途请求无损以及门禁流统计。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 1. sections/providers 新增 p2 热生效，version+1 且产生备份 | ✅ | put=200 ver=1->2 p2=200 backups=1 raw={"ok":true,"restart_required":[],"version":2,"warnings":[]}<br> |
| 2. sections/redaction 把 local 改为 mask 后 try 与真实请求立即脱敏 | ✅ | put=200 try="手机 {{PHONE_jhtiwr53}}" upstream="联系 {{PHONE_jhtiwr53}}" |
| 3. 非法配置返回 400 + problems 且旧配置继续服务 | ✅ | status=400 err=invalid_config probs=[providers.p1.base_url: must start with http:// or https://] p2=200 |
| 4. 过期 base_hash 返回 409 conflict | ✅ | status=409 body=map[current_hash:88285bb9ce4342ae90bff7b03a6766a94c63ab32384007b66adf0d1cc3d8db1f error:conflict] |
| 5. 外部改写 config.yaml 在 5s 内自动生效且 source=file | ✅ | reloaded=true alias_status=200 |
| 6. rollback 到首份备份后 p2 消失（p2/m2 → 404） | ✅ | rollback=200 p2=404 |
| 7. PUT provider key 生效且 GET config 不泄露明文 | ✅ | put=200 auth="Bearer upstream-secret-9988" has_key=true leaked=false |
| 8. 通过 YAML 修改 listen 成功且 restart_required 含 listen | ✅ | status=200 rr=[listen] |
| 9. 热重载期间在途 #slow 请求正常 200 完成 | ✅ | reload=200 slow=200 |
| 10. providers/test 返回 ok 与延迟 | ✅ | status=200 ok=true latency=0ms |
| 11. /admin/api/flow 各阶段计数准确且总和等于 total | ✅ | total=13 ok=7 401=1 403=1 429=1 red=1 up=1 other=1 |
