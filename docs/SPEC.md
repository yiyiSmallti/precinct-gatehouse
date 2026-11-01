# Gatehouse 首期执行 Spec

版本 0.1 · 对应 PRD 0.1

## 1. 技术栈

- Go 1.23+，标准库 `net/http` + 轻量路由；SQLite（`modernc.org/sqlite`，无 cgo）做计量与 Vault 落盘；YAML 配置。
- 插件：hashicorp/go-plugin 模式（子进程 + gRPC，握手魔数、协议版本协商）。是否直接依赖该库在实现期决定（MPL-2.0 可用），接口按其形态定义。
- OTLP：`go.opentelemetry.io/otel` 导出计量。
- 构建：单二进制，三平台交叉编译；Windows 版给 Alcove 个人用户。

## 2. 模块

```
cmd/gatehouse              CLI：serve / token issue|revoke / config check / redact try / report
internal/api               OpenAI 兼容入口：请求解析、流式/非流式、错误映射
internal/pipeline          每请求流水线：auth → policy → route → redact → provider → restore → meter → audit
internal/policy            租户上下文解析（personal 模式内置单用户；org 模式委托策略插件）、允许的路由组、数据分级硬约束、自带 API 判定
internal/route             路由组（order/usage）、意图路由（规则、分类器调用、useEffort）、同回合决策缓存（按 session 标签）
internal/redact            detect（规则包、词典、Never-mask、检测器插件、NER 可选）→ decide → mask（键控哈希占位）→ vault → restore（文本、tool_calls、thinking）
internal/redact/rules      中国手机号、身份证（校验）、银行卡（Luhn）、统一社会信用代码、车牌、邮箱、内网 IP/域名、密钥与令牌形态、上下文例外
internal/provider          OpenAI 兼容出站、超时与重试、健康状态供 order 组使用
internal/token             网关令牌：签发（HMAC 或 Ed25519）、校验、范围（groups/models/tokens/data_class/ttl）
internal/meter             按令牌归因、SQLite 汇总、OTLP 导出、预算与优先级队列的钩子位
internal/audit             审计记录（脱敏后）、本地文件/插件接收器
internal/keystore          个人：OS 钥匙串；org：加密文件，KMS 走插件
internal/plugin            五类扩展点接口与子进程宿主：Auth/Policy、AuditSink、Detector、Provider、Hooks
internal/config            YAML 结构、校验、热重载（规则与词典可热，监听地址不可）
e2e/                       假提供方、假策略插件、报告模板
```

## 3. 关键设计

### 3.1 流水线顺序与模式判定

`route` 先出最终目标（本地组 / 外部 / 自带 API），`redact` 据此查 `redaction.mode_by_target` 与数据分级交叉表，再决定 off/audit/mask/warn/block。`confidential` 且目标外部 → block（第二道闸）。外部目标一律整条缓冲，向提供方以非流式请求；本地目标且模式为 off/audit 时允许流式透传。

### 3.2 占位符与 Vault

- 占位符：`{{<KIND>_<8 位哈希>}}`，哈希 = `HMAC(user_key, normalized_value)` 截断；`user_key` 由租户与用户 ID 派生，personal 模式为本机随机密钥。
- Vault：内存 map（key: tenant+user+session），TTL 跟随会话标签，默认 24 h 无活动清除；可选 SQLite 加密落盘以支持重启后回填。
- 回填：按占位符正则扫描，容忍花括号内空白与大小写；覆盖 `choices[].message.content`、`tool_calls[].function.arguments`（JSON 字符串内替换后校验仍是合法 JSON）、thinking 字段。
- 增量：多轮请求对历史消息复用上次检测结果（按消息哈希缓存），只扫新增消息。

### 3.3 插件接口 v1（gRPC，go-plugin 形态）

```
AuthPolicy    Resolve(request_meta, token) → { tenant, user, department, data_class, allowed_groups, byo_api{enabled,max_class}, budget }
AuditSink     Write(record)                               record 只含脱敏后字段
Detector      Detect(text, lang, hints) → []Finding{kind, start, end, confidence}
Provider      Chat(request) → response/stream             非 OpenAI 兼容的提供方走这里
Hooks         OnRequest(ctx, req) → req'（不可改 model/stream/tools）；OnResponse(ctx, resp) 只读
```

插件由配置声明可执行文件路径与版本范围；握手失败不启动；策略类插件不可用时 org 模式拒绝服务（fail-closed），personal 模式不需要策略插件。

### 3.4 配置（personal 模式样例）

```yaml
listen: 127.0.0.1:7788
mode: personal
providers:
  - name: ds; base_url: https://api.deepseek.com; key_ref: keychain:ds
  - name: local; base_url: http://127.0.0.1:11434/v1
routing_groups:
  default:
    classifier: local/small
    rules:
      - intent: "闲聊、问候、简单问答"; use: local/small; useEffort: low
      - intent: "多步推理或长文"; use: ds/strong; useEffort: medium
      - tokens > 20000; use: ds/strong-long
    fallback: order: [ds/strong, local/small]
redaction:
  mode_by_target: { local: audit, external: mask }
  categories: { phone: mask, id_card: mask, bank_card: mask, secret: block, person: mask }
  dictionary: ~/.gatehouse/words.txt
  never_mask: ["team@example.com"]
  ner: { enabled: false }
  fail_closed: true
```

## 4. 里程碑与任务

- M1 骨架（2 周）：OpenAI 兼容入口、单提供方出站、personal 配置、令牌签发校验、计量 SQLite、CLI。验收 G4、G6 部分。
- M2 路由（1.5 周）：order/usage 组、意图规则、分类器调用与超时、同回合固定、健康与故障转移。验收 G1。
- M3 脱敏（3 周）：规则包与校验、词典、Never-mask、模式表、键控哈希、Vault、回填（含 tool_calls JSON 校验）、整条缓冲、审计记录、`redact try`。验收 G2、G3。
- M4 插件接口（2 周）：五类扩展点、子进程宿主、假策略插件、AuditSink 文件实现、org 模式与 fail-closed。验收 G5。
- M5 NER 可选模块（1 周，视验证周结论）：Detector 插件形态接入本地小模型，超时退化。
- 合计 8.5–9.5 周。

## 5. 测试策略

只做端到端：`e2e/` 用 Go 启动真实 `gatehouse serve` + 两个假提供方 HTTP 服务 + 假策略插件，跑 G1–G6，断言抓到的出站请求体、响应、审计文件与计量报表，产出 Markdown 报告到 `e2e/report/`。规则包的样本集放 `e2e/fixtures/redaction/`，报告列出每条样本的检测结果，作为脱敏召回/误报的可重复证据。不写单元测试。

## 6. 风险与对策

- 回填改坏 `tool_calls.arguments` 的 JSON：替换后做 JSON 校验，失败则把该回合标记并原样返回占位符，审计告警。
- 整条缓冲在长输出下的首字节延迟：用户侧非流式本就如此；本地目标仍可流式。
- 分类器小模型本身也是一次模型调用：缓存同会话决策，规则命中则不调分类器；8 s 超时走 fallback。
- 插件子进程在 Windows 个人模式的体验：personal 模式默认不加载任何插件，所有能力内置。
