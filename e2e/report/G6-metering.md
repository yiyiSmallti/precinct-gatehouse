# G6 计量

结果：**通过** · 运行于 2026-11-22 21:09:57 PST · darwin/arm64

两把令牌在本地与外部模型上发请求（含一次外部宕机后的故障转移与一次分类器之外的直连），网关停止时冲刷 OTLP。用 `gatehouse report --format json` 的按令牌汇总与假 OTLP 收集器收到的累计值逐项核对。

| 步骤 | 结果 | 细节 |
| --- | --- | --- |
| 故障转移的请求也计量 | ✅ | model=local/small |
| 收集器收到导出 | ✅ | 收到 1 次 OTLP 推送 |
| 报表覆盖三把令牌 | ✅ | 令牌：map[tok_3fe74e05cfc327f4:alice tok_aa2906180b94044e:bob tok_fc9bb079b8bb6118:local] |
| 令牌 alice 汇总一致 | ✅ | requests 3/3 prompt 48/48 completion 87/87 cost 0.000792/0.000792 |
| 令牌 bob 汇总一致 | ✅ | requests 4/4 prompt 22/22 completion 22/22 cost 0.000033/0.000033 |
| 令牌 local 汇总一致 | ✅ | requests 1/1 prompt 7/7 completion 6/6 cost 0.000062/0.000062 |

## 按令牌核对

| 令牌 | 请求（CLI / OTLP） | prompt | completion | 成本 |
| --- | --- | --- | --- | --- |
| alice | 3 / 3 | 48 / 48 | 87 / 87 | 0.000792 / 0.000792 |
| bob | 4 / 4 | 22 / 22 | 22 / 22 | 0.000033 / 0.000033 |
| local | 1 / 1 | 7 / 7 | 6 / 6 | 0.000062 / 0.000062 |


CLI 报表（`gatehouse report --format md`）：

| day | token | model | requests | errors | prompt | completion | cost |
| --- | --- | --- | ---: | ---: | ---: | ---: | ---: |
| 2026-11-22 | alice | cloud/strong | 3 | 0 | 48 | 87 | 0.000792 |
| 2026-11-22 | bob | cloud/strong | 1 | 1 | 0 | 0 | 0.000000 |
| 2026-11-22 | bob | local/small | 3 | 0 | 22 | 22 | 0.000033 |
| 2026-11-22 | local | cloud/strong | 1 | 0 | 7 | 6 | 0.000062 |

