# Gatehouse 首期执行 Spec

版本 0.1 · 对应 PRD 0.1

## 1. 技术栈

- Go 1.26+（依赖的 gRPC/OTel 当前版本要求），标准库 `net/http` + 轻量路由；SQLite（`modernc.org/sqlite`，无 cgo）做计量与 Vault 落盘；YAML 配置。
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

### 3.5 请求约定与 dsh 接入（2026-11-01 与 Alcove 线约定）

- 入口只说 OpenAI Chat Completions。dsh 自带的 DeepSeek 适配器讲 Anthropic Messages，不直接指向 Gatehouse；Alcove 在 profile 里用 `@deepseek-ai/dsh-llm-pi-ai` 配 `api: openai-completions` 路由，`baseURL` 指 `http://127.0.0.1:7788/v1`，`apiKey` 为网关令牌。`/v1/messages` 不做（PRD 3 首期外）。
- `model`：`auto`（`default_group`）、路由组名，或 `<provider>/<model>`。
- 可选请求头：`X-Gatehouse-Session`（会话：同回合路由固定与 Vault 范围；缺失时回退到请求体 `user` 字段，再缺失则 Vault 按令牌、回合按对话哈希）、`X-Gatehouse-Source`（路由规则 `source`）、`X-Gatehouse-Data-Class`（`public|internal|confidential`，默认 `internal`，不得高于令牌上限）。
- 响应头（F1）：`X-Gatehouse-Request-Id`、`X-Gatehouse-Group`、`X-Gatehouse-Rule`、`X-Gatehouse-Effort`、`X-Gatehouse-Model`、`X-Gatehouse-Redaction`（`<模式>; <类别>=<计数>…`）。
- 请求体顶层字段 `dsh_session_log` 一律在入口删除（带完整明文会话日志，会绕过脱敏）；`redaction.strip_fields` 可追加。
- 错误码：401 `missing_token|invalid_token|token_expired|token_revoked`；403 `model_not_allowed|data_class_not_allowed|token_budget_exceeded|department_budget_exceeded|no_allowed_target|policy_denied`；400 `redaction_blocked|invalid_request`；503 `redaction_unavailable|policy_unavailable|policy_invalid|hook_unavailable|hook_failed|local_targets_unavailable`；500 `hook_invalid`；502 `upstream_unavailable|upstream_redirect|upstream_stream_interrupted`。
- `useEffort` 下发为 `reasoning_effort`（`xhigh`、`max` 降为 `high`）。

### 3.6 插件接口 v1

`plugin/proto/gatehousev1/` 五个 proto 文件，同一包 `gatehouse.plugin.v1`。握手：`GATEHOUSE_PLUGIN` 魔数 + 协议版本 1，只允许 gRPC。Go SDK 是同仓库 `plugin` 包：`plugin.Serve(plugin.Set{...})` 一个可执行文件可服务一种或多种扩展点（`ServeDetector` 保留）。`plugins[]` 声明 `name`、`kind`（`detector|auth_policy|audit_sink|provider|hooks`）、`path`、`args`、`enabled`。

| 扩展点 | RPC | 宿主行为 |
| --- | --- | --- |
| Detector（`detector.proto`） | `Detect(text, lang, hints) → findings{kind, start, end, confidence}`，偏移是 Unicode 码点、end 不含 | `redaction.detectors` 列出必需检测器：禁用、启动失败或进程退出时，需要脱敏（mask/warn/block）的请求返回 503，audit 模式只记告警 |
| AuthPolicy（`policy.proto`） | `Resolve(token, request_meta{model, route_group, client_ip, request_id, source}) → GatewayContext` | 只在 org 模式用，且必须恰好一个。字段与 Cloister `gateway-context.schema.json` 一一对应（`expires_at` 改为 `expires_at_unix`，预算用 `has_*` 区分"0"与"不限"）。gRPC `UNAUTHENTICATED` → 401 `invalid_token`，`PERMISSION_DENIED` → 403 `policy_denied`，其余错误、超时（`org.policy_timeout`，默认 3 s）、进程不在 → 503 `policy_unavailable`。上下文按（令牌、请求模型、路由组）缓存，至多 `org.policy_cache`（默认 5 s）且不超过 `expires_at`——这也是策略收回一个令牌（例如成员退出组织）后它还能用多久，所以默认取几秒；插件进程不在时缓存也不用（fail-closed） |
| AuditSink（`audit.proto`） | `Write(records_json[]) → accepted` | 每条审计记录先写本地 JSONL 文件（内置文件实现，始终开启），再入队异步批量发给每个 AuditSink 插件（每批至多 200 条）；失败指数退避重试，队列上限 10 000 条，溢出丢最旧并告警（本地文件仍有）。记录 `request_id` 唯一，插件据此去重 |
| Provider（`provider.proto`） | `Chat(model, body) → stream {status, content_type, data}` | `providers[].plugin: <名字>` 代替 `base_url`。首条消息带状态码与类型；JSON 一次给完，SSE 分多条。插件不在或 gRPC 失败按可重试上游错误处理（触发故障转移），错误信息只记状态码 |
| Hooks（`hooks.proto`） | `OnRequest(ctx, body) → body'`；`OnResponse(ctx, body, status)` | OnRequest 在策略之后、路由之前按配置顺序执行，可改写请求但不能改 `model`、`stream`、`tools`（改了返回 500 `hook_invalid`）；钩子插件不在或报错返回 503（钩子可能承担策略，不静默跳过）。OnResponse 只读，在缓冲响应回填之后异步调用。钩子在脱敏之前运行、看得到明文，只装可信的钩子 |

审计记录（JSONL 与发给 AuditSink 的同一格式）：`time, request_id, token_id, token_name, tenant, user, department, on_behalf_of, instance_id, workspace_id, policy_version, source, requested_model, data_class, group, rule, rule_reason, effort, attempts[{model,status,error}], model, target, redaction{mode, counts, masked, warned, blocked, restored, samples[{kind,length,action}], alerts}, status, error, prompt_tokens, completion_tokens, latency_ms`。org 模式字段只在 org 模式出现。

### 3.6.1 org 模式

`mode: org` 时不签发、不接受本机 `ght1.` 令牌，Bearer 令牌整体交给 AuthPolicy 插件解析：

- 路由组必须在 `allowed_groups` 内；直连模型必须在 `models` 或某个允许组（含嵌套）的成员里；规则选出的候选再按同一集合过滤。org 模式下空 `allowed_groups` 表示什么组都不允许。
- `data_class` 是请求分级的下限：`X-Gatehouse-Data-Class` 只能调高，不能调低。`confidential` 只走本地目标，本地候选全部失败时返回 503 `local_targets_unavailable`，不降级到外部。
- `budget.tokens_remaining`、`department_tokens_remaining` 为 0 时 403；预算的扣减由策略端负责（Gatehouse 只报用量，经 AuditSink）。
- 占位符键 = HMAC(本机密钥, 租户 + 用户)；计量按 `org:<tenant>/<user>` 归因。
- `/v1/models` 按策略列出允许的组与模型；`/healthz` 带 `policy{plugin, available}`，插件不在时 `status: unavailable`。

### 3.7 实现与本 Spec 的差异

- 已在首期之后补充实现：
  - 持久化 `sk-` API Key 与基于内存滑窗/计数器的次数限额（并发、RPM、每日 RPD，重启后每日计数从 `request_log` 恢复）；`gatehouse apikey` CLI。
  - 入口协议适配：Anthropic Messages（`POST /v1/messages`、`POST /v1/messages/count_tokens`）与 OpenAI Responses（`POST /v1/responses`），含流式/非流式、thinking 与 tool_calls 互转。
  - 路由与请求增强：`max_body_mb` 配置（`0` 不限）、去掉 `ght1.` 令牌 `MaxTokens` 整请求串行锁、`model_aliases` 别名映射、provider 级 `system_prompt` 拼接与 `extra_body` 合并、`cached_tokens` 与 `ttft_ms` 统计。
  - 内嵌 Web 控制面板与管理 API（`admin.listen`，PBKDF2 密码哈希、Cookie 会话、`X-Gatehouse-Admin` CSRF 校验、密码修改）。
  - 配置热重载与全量在线配置管理：基于快照句柄的原子切换（`internal/live`，在途请求绑定进入时快照不受影响），支持渠道/路由与别名/脱敏/常规参数分节保存（带 `base_hash` 乐观锁冲突检测）、原始 YAML 编辑与语法校验、外部文件监听与 `SIGHUP` 自动重载、`<data_dir>/config-history/` 最多 30 份历史备份与逐行 Diff 回滚、`db:<name>` 上游密钥数据库存储、`providers/test` 连通性探测及 `/admin/api/flow` 门禁流统计；其中 `listen`、`mode`、`data_dir`、`admin.*`、`meter.*`、`audit.*`、`org.*`、`plugins.*` 等启动基础设施字段修改后返回 `restart_required` 提示需重启生效，且 Web 写回 YAML 会剥离手写注释。
  - **内置智能分层路由（`providers[].tiered`）**：
    - **配置 Schema**：`mode`（`shadow` | `active`）、`heavy_miss_threshold`、`health_interval_sec`、`zh_think_anchor`、`backends[]`（`id`, `url`, `tier`, `max_context_tokens`, `max_safe_input`, `promote_at`, `demote_below`, `speed_score`）、`starter_cache`（`enabled`, `canonicalize_tools`, `min_tokens`, `max_tokens`, `min_hits`, `max_entries`, `keepalive_sec`, `check_interval_sec`, `startup_delay_sec`, `entry_ttl_hours`, `persist_path`）。`shadow` 模式要求保留非空 `base_url`；`active` 模式可省略 `base_url`。
    - **运行模式与热重载**：`internal/tiered.Hub` 按 provider 名称维护常驻 `Router` 实例；`buildSnapshot`（含配置校验/预览）无副作用，`live.Manager.OnSwap` 仅在真实快照切换时调用 `Hub.Activate` 原地更新后端与 Starter 参数，完整保留内存前缀缓存、会话滞回表及后台探测协程；响应传播 `X-Router-*` 头并在 SQLite `request_log` 写入 `route_backend` / `route_reason`。
    - **管理接口**：`GET /admin/api/tiered`、`GET /admin/api/tiered/{provider}/starters`、`POST /admin/api/tiered/{provider}/starters/pin`、`DELETE /admin/api/tiered/{provider}/starters/{hash}`、`POST /admin/api/tiered/{provider}/starters/warm`（`shadow` 模式返回 `409 shadow_mode`）、`POST /admin/api/tiered/{provider}/shadow/reset`；验收覆盖 `e2e/g12_tiered_test.go`（`e2e/report/G12-tiered-router.md`）。
- 仍未实现：
  - Vault 加密落盘、多轮增量检测缓存、NER 模块（M5，看验证周）、插件进程崩溃后自动重启（当前需重启 Gatehouse）、插件包（`.tgz` + `gatehouse` 字段）的安装命令。
  - Gemini 原生协议互转；OpenAI Responses 的 `previous_response_id` 有状态续聊（返回 400 `previous_response_not_supported`）；Responses 中 `web_search`、`custom` 等非 `function` 内置工具会被丢弃并在 `X-Gatehouse-Dropped-Tools` 响应头中列出。
- org 模式暂不使用 `byo_api` 与策略下发的 `redaction` 段（脱敏仍按本机配置）；`budget.period` 只透传不解释；`sk-` API Key 仅限 personal 模式使用。
- 外部目标含图请求：类别 `image`，默认动作同其它类别（mask 模式下视为 warn，原样发出并告警），可设 `image: block`。
- 审计样本只记类别、长度、动作，不记值的前几个字符。
- 路由组的 `usage` 分摊按进程内累计 token，重启归零。

## 4. 里程碑与任务

- M1 骨架（2 周）：OpenAI 兼容入口、单提供方出站、personal 配置、令牌签发校验、计量 SQLite、CLI。验收 G4、G6 部分。
- M2 路由（1.5 周）：order/usage 组、意图规则、分类器调用与超时、同回合固定、健康与故障转移。验收 G1。
- M3 脱敏（3 周）：规则包与校验、词典、Never-mask、模式表、键控哈希、Vault、回填（含 tool_calls JSON 校验）、整条缓冲、审计记录、`redact try`。验收 G2、G3。
- M4 插件接口（2 周）：五类扩展点、子进程宿主、假策略插件、AuditSink 文件实现、org 模式与 fail-closed。验收 G5（已完成，报告 `e2e/report/G5-org-mode.md`）。
- M5 NER 可选模块（1 周，视验证周结论）：Detector 插件形态接入本地小模型，超时退化。
- 合计 8.5–9.5 周。

## 5. 测试策略

只做端到端：`e2e/` 用 Go 启动真实 `gatehouse serve` + 两个假提供方 HTTP 服务 + 假策略插件，跑 G1–G6，断言抓到的出站请求体、响应、审计文件与计量报表，产出 Markdown 报告到 `e2e/report/`。规则包的样本集放 `e2e/fixtures/redaction/`，报告列出每条样本的检测结果，作为脱敏召回/误报的可重复证据。不写单元测试。

## 6. 风险与对策

- 回填改坏 `tool_calls.arguments` 的 JSON：替换后做 JSON 校验，失败则把该回合标记并原样返回占位符，审计告警。
- 整条缓冲在长输出下的首字节延迟：用户侧非流式本就如此；本地目标仍可流式。
- 分类器小模型本身也是一次模型调用：缓存同会话决策，规则命中则不调分类器；8 s 超时走 fallback。
- 插件子进程在 Windows 个人模式的体验：personal 模式默认不加载任何插件，所有能力内置。
