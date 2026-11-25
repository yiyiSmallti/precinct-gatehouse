# Gatehouse

<p align="center">
  <img src="docs/assets/icon.png" alt="Gatehouse 图标" width="112">
</p>

<p align="center">
  <strong>Precinct 系列 · 自研模型网关</strong><br>
  路由组 · 意图路由 · 隐私脱敏 · 计量限流 · 短时效令牌
</p>

<p align="center">
  <code>Go 1.26+</code> · <code>无 CGO 单二进制</code> · <code>OpenAI / Anthropic / Responses 兼容</code> · <code>MIT</code>
</p>

---

**导航**：[定位与能力](#定位与核心能力) · [快速开始](#快速开始个人模式) · [团队网关](#作为团队网关替代-new-api) · [内置智能路由](#内置智能分层路由providers-tiered) · [org 模式](#org-模式) · [相关文档](#文档)

---

## 定位与核心能力

**门房（Gatehouse）** 是模型调用的唯一出口。两种用法，同一份代码：

| 使用模式 | 适用场景 | 说明 |
| --- | --- | --- |
| **`personal`（单用户本机网关）** | 个人开发者本机运行 | 为任意 OpenAI / Anthropic / Responses 兼容客户端提供路由、脱敏、多 Key 限额与用量统计 |
| **`org`（组织多租户服务）** | 组织内部统一模型出口 | 配合策略与令牌插件，实施部门预算、数据分级硬约束、脱敏与集中审计 |

### 核心能力

- **路由组与意图路由**：支持 `order`（故障转移）与 `usage`（用量分摊）路由组；结合小模型意图分类与规则匹配指定模型和思考强度（`reasoning_effort`），同一回合决策固定。
- **隐私脱敏层**：位于路由之后、出站之前的独立阶段。内置规则检测器（手机号、身份证、银行卡、统一社会信用代码、密钥与令牌形态等）+ 词典 + 可选本地 NER；采用带键哈希占位符（跨回合一致）、响应与工具参数自动回填，支持 `fail-closed`（参考 magpie 的 Privacy masking 设计）。
- **令牌与计量**：支持短时效、限范围的网关令牌，以及持久化 `sk-` API Key；提供按令牌/密钥归因的用量统计、限流与 OTLP 指标导出。
- **插件扩展面**：提供鉴权与策略（`auth_policy`）、审计接收（`audit_sink`）、脱敏检测器（`detector`）、提供方适配（`provider`）与请求/响应钩子（`hooks`）五类 gRPC 扩展点。Gatehouse 自身不绑定任何组织平台，Cloister 适配实现 `gatehouse-cloister` 位于 `precinct-dovetail`。

> **项目边界**：Gatehouse 与 Precinct 系列其他项目仅通过插件与线协议关联，不共享代码库。系列总览与架构设计请参阅 `precinct-charter`。

---

## 文档

| 文档 | 说明 |
| --- | --- |
| [首期 PRD](docs/PRD.md) | 产品目标、用户场景、功能与非功能需求、验收标准 |
| [首期执行 Spec](docs/SPEC.md) | 技术栈、模块划分、流水线与插件接口设计、已知差异 |
| [持续交付指南（CD）](docs/CD.md) | GitHub Actions 自动构建、镜像中转同步、原子发布与回滚 |
| [端到端验收报告](e2e/report/) | `go test -v ./e2e/` 生成的各场景验收报告 |

---

## 快速开始（个人模式）

### 1. 编译与启动

```sh
go build -o gatehouse ./cmd/gatehouse   # Go 1.26+，无 CGO，单二进制
./gatehouse serve                       # 默认监听 127.0.0.1:7788，控制台 127.0.0.1:7789
```

首次运行若不存在配置文件，网关会自动生成可直接启动的默认配置（`~/.gatehouse/config.yaml`，含控制台地址与脱敏默认值）并继续运行，无需手工编辑。启动日志会打印一次性管理员初始化链接：

```text
Open the console to create the admin account (one-time setup code XXXXXXXXXX): http://127.0.0.1:7789/admin/#/setup?code=XXXXXXXXXX
```

### 2. 初始化与接入

1. 打开上述链接创建管理员账号，按控制台概览页三步引导完成：**添加模型渠道 → 建立路由组 → 签发访问密钥**。
   > 设置码仅在尚未设置管理员密码时有效，且与登录共用错误锁定计数。
2. 也可通过 CLI 完成初始化：
   - `gatehouse key set ds`：将上游密钥存入系统钥匙串
   - `gatehouse admin set-password`：设置控制台管理员密码
   - `gatehouse config check`：校验配置文件合法性
3. 本机默认令牌保存在 `~/.gatehouse/local.token`。任意 OpenAI 兼容客户端将 Base URL 设为 `http://127.0.0.1:7788/v1`，API Key 填写 `local.token` 内容，`model` 填 `auto`（默认路由组）、路由组名或 `<provider>/<model>`。`dsh` 经 `dsh-llm-pi-ai` 的 `openai-completions` 路由接入（详见 [SPEC 3.5](docs/SPEC.md#35-请求约定与-dsh-接入)）。

### 3. 常用 CLI 与验收

- **常用命令**：`gatehouse token issue|revoke|list`、`gatehouse redact try`、`gatehouse report`
- **决策可观测性**：响应头附带 `X-Gatehouse-Model`、`X-Gatehouse-Rule`、`X-Gatehouse-Redaction` 等字段；审计日志记录在 `~/.gatehouse/audit/`（仅含脱敏类别计数，不含明文）。
- **端到端验收**：运行 `go test -v ./e2e/`，验收报告输出至 [`e2e/report/`](e2e/report/)。

### 4. 内网自建模型部署提示

- **非回环地址必须显式标注本地目标**：指向内网 vLLM / SGLang（如内网 IP 或内网域名）的 provider 必须配置 `target: local`；否则网关默认按 `external` 处理，触发出站脱敏并整条缓冲响应，导致客户端失去流式输出。
- **大上下文与长生成超时**：provider 默认 `timeout: 300s`（覆盖请求体读取到响应完成的全程），长上下文推理建议配置 `timeout: 3600s`；如有数十 MB 含图请求，可设置顶层 `max_body_mb: 0`（默认 `32` MiB）。
- **本地目标关闭脱敏扫描**：对纯内网自建模型，建议设置 `redaction.mode_by_target.local: off`，避免对大请求体做正则扫描。

---

## 作为团队网关（替代 New-API）

除个人本机网关外，Gatehouse 内置面向小团队与内部集群的多 Key 管理、用量限额、多协议适配与 Web 控制面板。

> **注意**：多 `sk-` API Key 功能仅在 `personal` 模式可用；`org` 模式不支持 `sk-` Key（会返回 `401`）。

### 多 API Key 与用量限额

通过 CLI 或 Web 控制台签发持久化 `sk-` 密钥（SQLite 仅存储 SHA-256 哈希与前缀）：

```sh
gatehouse apikey create --name alice --models ds/deepseek-chat --rpm 30 --rpd 1000 --concurrency 4 --json
gatehouse apikey create --name bob --tokens-daily 1000000 --tokens-monthly 20000000 --json
gatehouse apikey list                              # TOKENS 列显示 今日 / 本月 / 累计 用量与配额
gatehouse apikey update <id> --rpm 60 --rpd 2000   # 在线更新，无需重发密钥
gatehouse apikey update <id> --tokens-total 0      # 0 表示不限
gatehouse apikey disable <id>
gatehouse apikey enable <id>
gatehouse apikey reset <id> --json                 # 轮换生成新密钥，旧密钥立即失效
gatehouse apikey delete <id>
```

**限额检查顺序**：**并发 → RPM → 每日次数（RPD）→ Token 配额**

- 上游 5xx 或网关自身失败（`status >= 500`）不占用每日额度；RPM 滑窗在准入时计数。
- 限额状态驻留内存；进程重启后每日已用次数会从 `request_log` 自动恢复，RPM 滑窗与实时并发计数重置。
- 默认保留最近 `90` 天 `request_log`（可通过顶层 `request_log_days` 配置，`0` 表示永久保留）。
- **按 Token 量限额**：支持为每把 Key 分别设置每日、每月、累计 Token 配额（输入 + 输出，含流式），`0` 为不限。用量在请求完成后记账，越过配额的当次请求正常完成，**下一次**请求返回 `429 token_quota_exceeded`（每日/每月配额附带 `Retry-After`）。Token 账本独立存储在 `key_token_usage` 表中，不随 `request_log` 清理；控制台「清零用量」仅清零账本，不换密钥。

### 入口协议与客户端配置

| 客户端 / 协议 | 网关端点 | 客户端配置要点 |
| --- | --- | --- |
| OpenAI Chat Completions | `POST /v1/chat/completions` | `base_url` 设为 `http://<host>:<port>/v1`，Bearer 填 `sk-...` |
| Claude Code（Anthropic Messages） | `POST /v1/messages`、`POST /v1/messages/count_tokens` | `ANTHROPIC_BASE_URL=http://<host>:<port>`、`ANTHROPIC_API_KEY=<sk-...>` |
| Codex（OpenAI Responses） | `POST /v1/responses` | 配置 `wire_api = "responses"`，`base_url` 指向 `http://<host>:<port>/v1` |

- `model_aliases` 可将外部客户端请求的名称按顺序映射为网关路由组或 `<provider>/<model>`（支持精确匹配或 `path.Match` 通配），响应中的 `model` 字段保留客户端原始请求名。
- provider 支持 `system_prompt`（向首条 system 消息前拼接，行为对齐 New-API）与 `extra_body`（顶层合并到出站 JSON）。

### Web 控制面板（admin）

默认配置已开启控制台（`admin.listen: 127.0.0.1:7789`），首次访问可使用启动日志中的设置码创建管理员，或通过 CLI 设置密码：

```sh
echo 'your-strong-password' | gatehouse admin set-password --user admin
```

随 `gatehouse serve` 启动后访问 `http://<admin_listen>/admin/`：

- **概览与门禁流**：实时查看「密钥鉴权 → 策略与限流 → 隐私脱敏 → 上游调度」四级关卡分流（`1h` / `24h`）、今日 KPI、24 小时柱状节奏图与实时并发/RPM。
- **访问密钥**：团队 `sk-` 密钥增删改/启停/轮换（一次性展示明文）、RPM/RPD/并发及 Token 配额（今日/本月/累计）用量条与「清零用量」。
- **统计分析**：支持 `24h` / `7d` / `14d` / `30d` / `90d` 时间范围与密钥/模型筛选（筛选条件写入 URL 可分享）；提供 Token 用量趋势堆叠图、模型与密钥排行、请求量与异常、耗时与首字延迟、状态/耗时分布、星期×小时热力图及明细表，支持导出 CSV（数据源 `GET /admin/api/stats`）。
- **请求日志**：多条件检索与详情抽屉，展示路由后端与路由原因。
- **在线配置编辑与热重载**：支持在面板直接编辑 **模型渠道**（含连通探测与 `db:<name>` 密钥直写）、**路由组与模型别名**、**智能路由**、**隐私脱敏规则**（含右侧实时演练）、**常规参数**、**服务与端口**及底部折叠的 **直接编辑 YAML**。所有修改通过 `base_hash` 乐观锁校验后原子生效。
- **深浅色主题与响应式**：奶油色/陶土色配色体系与深色侧栏，支持浅/深色切换，适配桌面与窄屏抽屉导航。

<details>
<summary>热重载语义与运维说明（点击展开）</summary>

1. **无损在途请求**：所有请求在进入门禁流水线时绑定当前配置快照；热重载通过原子切换快照立即对新请求生效，正在执行的流式或长耗时请求不会被打断。
2. **字段生效范围**：
   - **支持热重载即时生效**：`providers`、`routing_groups`、`default_group`、`model_aliases`、`redaction`、`max_body_mb`、`request_log_days`、`tokens.default_ttl`。
   - **需重启进程生效**：`listen`、`mode`、`data_dir`、`admin.*`、`meter.*`、`audit.*`、`org.*`、`plugins.*` 等启动基础设施字段（修改后接口与控制台会返回 `restart_required` 提示）。
3. **外部监听与信号**：`gatehouse serve` 自动轮询 `config.yaml` 变更（约 1.5s 内校验加载），也支持发送 `SIGHUP` 触发立即重载；若新配置不合法，网关保留上一有效快照运行并在控制台暴露 `last_error`。
4. **配置写回与自动备份**：Web 面板保存或回滚会以结构化 YAML 原子写回 `config.yaml`（权限 `0600`，**手写 YAML 注释将被移除**）；改写前自动备份至 `<data_dir>/config-history/`（最多保留 30 份），支持逐行 Diff 查看与一键回滚。
5. **上游密钥安全存储**：可通过控制台或 `PUT /admin/api/providers/<name>/key` 将上游密钥写入 SQLite 密钥库并引用 `db:<name>`，避免明文密钥落入 `config.yaml` 或备份历史。
6. **安全建议**：`admin.listen` 建议仅绑定 `127.0.0.1` 或内网地址，再通过 SSH 端口转发或带 IP 白名单/HTTPS 的反向代理暴露，切勿直接对公网裸露管理端口。

</details>

### 反向代理链路参考配置示例

若将 Gatehouse 串入 `Nginx -> Gatehouse -> smart-router -> SGLang` 等内网链路，可参考如下最小配置：

```yaml
listen: 127.0.0.1:7788
mode: personal
max_body_mb: 0                    # 不限制大上下文/多图请求体大小
request_log_days: 90

admin:
  listen: 127.0.0.1:7789          # 仅监听本机，配合 SSH 隧道或反向代理白名单访问
  session_ttl: 12h

providers:
  - name: ds
    base_url: http://127.0.0.1:8080/v1
    target: local                 # 必须标为 local，保留流式且不触发外部缓冲
    timeout: 3600s                # 长上下文生成超时
    models: [deepseek-chat]
    system_prompt: |
      每次在 <think>...</think> 块中进行内部推理思考时，请使用简体中文撰写。

default_group: main
routing_groups:
  main:
    type: order
    members: [ds/deepseek-chat]

model_aliases:
  - match: "claude-*"
    use: main
  - match: "gpt-*"
    use: main

redaction:
  mode_by_target:
    local: off                    # 纯内网自建模型关闭脱敏扫描
    external: mask
  # vault_persist: true           # 可选：占位符对照表加密（AES-256-GCM）落盘到 gatehouse.db，重启 / 热升级后仍可回填；默认关闭
  # sources:                      # 可选：按检测器插件调整（模型类 NER 检测器建议如下）
  #   ner:
  #     min_confidence: { default: 0.8, person: 0.9, date: 0.9, account: 0.7 }
  #     max_action: mask            # NER 结果最多脱敏、不拦截；先用 audit 影子运行观察误报
  #     timeout: 3s
  #     on_error: rules_only        # 插件超时 / 出错 / 重启中时只用规则继续，不返回 503
```

---

## 内置智能分层路由（`providers[].tiered`）

将原本独立的 `smart-router` 能力内嵌进单个 provider，支持 `shadow` 与 `active` 两种模式无缝切换（热重载自动保留同一 `Router` 的前缀估算校准缓存与实时健康探测器）：

- **`mode: shadow`**：请求仍透传至 `providers[].base_url`（如现网独立路由服务），Gatehouse 后台运行相同前缀估算与调度决策，读取响应头 `X-Router-Backend` 统计 Shadow 预测一致率与偏差样本，不改变任何线上路由行为；此模式下拒绝触发 `/starters/warm`（返回 HTTP `409 shadow_mode`）。
- **`mode: active`**：Gatehouse 直接通过分级后端列表调度（可省去独立路由进程，`base_url` 可留空），支持短/长请求自动升降级、同会话粘性滞回（`promote_at` / `demote_below`）、Heavy Miss 空闲调度与 400 超长上下文自动故障转移、`max_tokens` 安全钳制、Tools 确定性规范化排序、中文思考锚点注入，以及高命中 Starter 前缀自动观察与空闲 KV Cache 预热。

控制台路径 `#/tiered` 提供模式切换、节点健康与并发总览、Shadow 一致率与偏差样本重置、近期路由决策流及 Starter 钉选/预热/删除操作；`#/logs` 请求日志同步展示「路由后端」与「路由原因」。

---

## `org` 模式

`mode: org` 时令牌交给 `auth_policy` 插件解析（租户、用户、部门、数据分级、允许的路由组、预算），插件不可用时对所有请求返回 `503`。插件进程崩溃后由 supervisor 按 1 s 起、上限 30 s 的指数退避自动重启，窗口内仍返回 `503`；各插件状态与重启次数见 `/healthz` 的 `plugins`。最小配置示例：

```yaml
mode: org
plugins:
  - name: cloister
    kind: auth_policy
    path: /opt/gatehouse/plugins/gatehouse-cloister
  - name: cloister-audit          # 可选：审计记录同时送往组织平台
    kind: audit_sink
    path: /opt/gatehouse/plugins/gatehouse-cloister
```

插件使用同仓库 `plugin` 包开发：`plugin.Serve(plugin.Set{AuthPolicy: impl})`。五类扩展点与宿主行为详见 [SPEC 3.6](docs/SPEC.md#36-插件接口-v1)。

---

## 状态

| 阶段 | 已实现能力 |
| --- | --- |
| **首期（MVP）** | `G1` 路由组与意图路由 · `G2` 脱敏与回填 · `G3` fail-closed · `G4` 短时效令牌 · `G5` `org` 模式与五类插件扩展点 · `G6` 计量与 OTLP 导出 |
| **扩展能力** | `G7` 多 `sk-` API Key 与次数限额 · `G8` Anthropic / Responses 协议适配 · `G9` Web 控制面板 · `G10` 在线配置热重载 · `G11` New-API 密钥/用量兼容迁移 · `G12` 内置智能分层路由 · `G16` Vault 加密落盘 · `G17` 插件崩溃自动重启 |

参考实现：magpie（路由组、意图路由、Privacy masking） · 许可：MIT
