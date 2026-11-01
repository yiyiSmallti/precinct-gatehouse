# 脱敏规则包样本集

结果：**通过** · 运行于 2026-11-01 16:34:57 UTC · linux/amd64

`fixtures/redaction/samples.tsv` 每行：期望类别（逗号分隔，`-` 表示不应检出）、文本。经 `gatehouse redact try --json` 检测，比较检出类别与期望。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 请拨打 13812345678 联系我 | ✅ | 期望 phone，检出 phone |
| 电话：+86 138-1234-5678 | ✅ | 期望 phone，检出 phone |
| 手机 138 1234 5678 随时找我 | ✅ | 期望 phone，检出 phone |
| 订单号 13900001111 已发货 | ✅ | 期望 -，检出 - |
| order_id: 13900001111 | ✅ | 期望 -，检出 - |
| 工单号：110101199003071233 | ✅ | 期望 -，检出 - |
| 身份证号 110101199003071233 | ✅ | 期望 id_card，检出 id_card |
| 证件 11010119900307002x 末位小写 x | ✅ | 期望 id_card，检出 id_card |
| 编号 110101199003071234 校验位不对 | ✅ | 期望 -，检出 - |
| 工资卡 6222021234567890128 | ✅ | 期望 bank_card，检出 bank_card |
| 卡号 6222 0212 3456 7890 128 | ✅ | 期望 bank_card，检出 bank_card |
| 流水 6222021234567890127 不过 Luhn | ✅ | 期望 -，检出 - |
| 统一社会信用代码 91350100M000100Y43 | ✅ | 期望 uscc，检出 uscc |
| 车牌 京A12345 停在楼下 | ✅ | 期望 plate，检出 plate |
| 粤B·D12345 新能源 | ✅ | 期望 plate，检出 plate |
| 发邮件到 zhang.san@corp-mail.com.cn | ✅ | 期望 email，检出 email |
| 抄送 team@example.com | ✅ | 期望 -，检出 - |
| 数据库在 10.12.3.4:5432 | ✅ | 期望 ip，检出 ip |
| 网关 192.168.1.1 | ✅ | 期望 ip，检出 ip |
| 公网 8.8.8.8 不算内网 | ✅ | 期望 -，检出 - |
| 访问 git.dev.internal 拉代码 | ✅ | 期望 domain，检出 domain |
| OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz123456 | ✅ | 期望 secret，检出 secret |
| Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789 | ✅ | 期望 secret，检出 secret |
| postgres://admin:S3cretPass@10db.example.com/app | ✅ | 期望 secret，检出 secret |
| password: hunter2hunter2 | ✅ | 期望 secret，检出 secret |
| AKIAIOSFODNN7EXAMPLE 是 AWS key | ✅ | 期望 secret，检出 secret |
| token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U | ✅ | 期望 secret，检出 secret |
| 星辰科技的合同下周签 | ✅ | 期望 term，检出 term |
| 版本号 1.2.3，端口 8080，时间 2026-11-01 16:00 | ✅ | 期望 -，检出 - |
| UUID 550e8400-e29b-41d4-a716-446655440000 | ✅ | 期望 -，检出 - |

合计：命中 21，漏检 0，误报 0。
