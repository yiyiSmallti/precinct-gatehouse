# Gatehouse

Precinct 系列 · 自研模型网关：路由组、意图路由、脱敏、计量、短时效令牌。

## 定位

门房：模型调用的唯一出口。对内对外都只说 OpenAI 兼容 HTTP。

两种用法，同一份代码：

- 单用户本机网关：个人在自己机器上运行，给任意 OpenAI 兼容客户端做路由与脱敏（用法类似 magpie）。
- 组织多租户服务：配合策略与令牌插件，成为组织内所有模型调用的策略、路由、脱敏与计量出口。

## 核心能力

- 路由组（order / usage）与意图路由：小模型分类，规则命中指定模型与思考强度，同一回合决策固定。
- 脱敏层：路由之后、出站之前的独立阶段。规则检测器（中国手机号、身份证、银行卡、统一社会信用代码、密钥与令牌形态等）+ 词典 + 可选本地 NER；带键哈希占位符，跨回合一致；响应与工具参数回填；fail-closed。主要参考 magpie 的 Privacy masking。
- 短时效、限范围的网关令牌；按令牌归因的计量与 OTLP 导出。

## 扩展面（插件接口）

鉴权与策略、审计、脱敏检测器、提供方适配、请求/响应钩子。Gatehouse 自身不认识任何组织平台；多租户与策略是插件接口上的能力，Cloister 的实现 `gatehouse-cloister` 在 [Dovetail](https://github.com/yiyiSmallti/precinct-dovetail)。

## 与系列其它项目的关系

只通过插件和线协议关联，不共享代码库。总览与架构文档见 [Charter](https://github.com/yiyiSmallti/precinct-charter)。

## 文档

- [首期 PRD](docs/PRD.md)
- [首期执行 Spec](docs/SPEC.md)

## 快速开始（个人模式）

```sh
go build -o gatehouse ./cmd/gatehouse   # Go 1.26+，无 cgo，单二进制
./gatehouse serve                       # 首次运行写出 ~/.gatehouse/config.yaml 样例并退出
./gatehouse key set ds                  # 把 DeepSeek key 存进系统钥匙串
./gatehouse config check
./gatehouse serve                       # 监听 127.0.0.1:7788，本机令牌写在 ~/.gatehouse/local.token
```

任意 OpenAI 兼容客户端把 base URL 设为 `http://127.0.0.1:7788/v1`，API key 填 `local.token` 的内容，`model` 填 `auto`（默认路由组）、路由组名或 `<provider>/<model>`。dsh 经 `dsh-llm-pi-ai` 的 `openai-completions` 路由接入（见 [SPEC 3.5](docs/SPEC.md)）。

其它命令：`gatehouse token issue|revoke|list`、`gatehouse redact try`、`gatehouse report`。每个响应带 `X-Gatehouse-Model`、`X-Gatehouse-Rule`、`X-Gatehouse-Redaction` 等头说明决策路径；审计记录在 `~/.gatehouse/audit/`，只含类别计数。

端到端验收：`go test -v ./e2e/`，报告写到 [`e2e/report/`](e2e/report/)。

### 内网自建模型部署提示

- **非回环地址必须显式标注本地目标**：指向内网 vLLM/SGLang（如 `172.16.x.x` 或域名）的 provider 必须配置 `target: local`；否则网关默认按 `external` 处理，触发出站脱敏并整条缓冲响应，导致客户端失去流式输出。
- **大上下文与长生成超时**：provider 默认 `timeout: 300s`（覆盖读取请求体到响应完成的全程），长上下文推理建议配置 `timeout: 3600s`；如有数十 MB 含图请求，可设置顶层 `max_body_mb: 0`（默认 `32` MiB）。
- **本地目标关闭脱敏扫描**：对纯内网自建模型，建议设置 `redaction.mode_by_target.local: off`，避免对大请求体做正则扫描。

## 作为团队网关（替代 New-API）

除个人本机网关外，Gatehouse 现已内置面向小团队/内部集群的多 Key 管理、次数限额、多协议适配与 Web 控制面板（仅 `personal` 模式可用；`org` 模式不支持 `sk-` API Key，会返回 401）。

### 多 API Key 与次数限额

通过 CLI 或 Web 控制台签发持久化 `sk-` 密钥（SQLite 只存 SHA-256 哈希与前缀）：

```sh
gatehouse apikey create --name alice --models ds/deepseek-chat --rpm 30 --rpd 1000 --concurrency 4 --json
gatehouse apikey list
gatehouse apikey update <id> --rpm 60 --rpd 2000   # 在线更新，无需重发密钥
gatehouse apikey disable <id>
gatehouse apikey enable <id>
gatehouse apikey reset <id> --json                 # 生成新密钥，旧密钥立即失效
gatehouse apikey delete <id>
```

限额检查顺序为 **并发 → RPM → 每日次数（RPD）**：

- 上游 5xx 或网关自身失败（`status >= 500`）不占用每日额度；RPM 滑窗在准入时计数。
- 限额状态驻留内存；进程重启后，每日已用次数会从 `request_log` 自动恢复，RPM 滑窗与实时并发计数重置。
- 默认保留最近 `90` 天 `request_log`（可通过顶层 `request_log_days` 配置，`0` 表示永久保留）。

### 入口协议与客户端配置

| 客户端 / 协议 | 网关端点 | 客户端配置要点 |
| --- | --- | --- |
| OpenAI Chat Completions | `POST /v1/chat/completions` | `base_url` 指 `http://<host>:<port>/v1`，Bearer 填 `sk-...` |
| Claude Code（Anthropic Messages） | `POST /v1/messages`、`POST /v1/messages/count_tokens` | `ANTHROPIC_BASE_URL=http://<host>:<port>`、`ANTHROPIC_API_KEY=<sk-...>` |
| Codex（OpenAI Responses） | `POST /v1/responses` | 配置 `wire_api = "responses"`，`base_url` 指向 `http://<host>:<port>/v1` |

`model_aliases` 可把外部客户端习惯请求的名称按顺序映射为网关路由组或 `<provider>/<model>`（支持精确名或 `path.Match` 通配），响应中的 `model` 字段仍保留客户端原始请求名。provider 支持 `system_prompt`（向首条 system 消息前拼接，行为对齐 New-API）与 `extra_body`（顶层合并到出站 JSON）。

### Web 控制面板（admin）

```sh
echo 'your-strong-password' | gatehouse admin set-password --user admin
```

配置 `admin.listen` 后随 `gatehouse serve` 一并启动，访问 `http://<admin_listen>/admin/`：

- **功能总览**：
  - **概览与门禁流**：实时查看入站请求经由「密钥鉴权 → 策略与限流 → 隐私脱敏 → 上游调度」四级关卡的分流情况（`1h` / `24h`）、今日关键 KPI、24 小时柱状节奏图与各密钥实时并发/RPM。
  - **访问密钥与用量**：团队 `sk-` 密钥增删改/启停/重置计数（一次性展示明文）、RPM/RPD/并发用量条、按日期/密钥/模型的用量汇总，以及多条件请求日志检索与详情抽屉。
  - **在线配置编辑与热重载**：支持在 Web 面板直接编辑 **模型渠道**（含连通探测与 `db:<name>` 上游密钥直写）、**路由组与模型别名**（顺序拖拽/权重/规则/别名映射）、**隐私脱敏规则**（各渠道模式、11 类敏感项动作、词表白名单、`fail_closed`、`vault_ttl` 与右侧实时演练）、**常规参数** 及 **原始 YAML 编辑器**。所有配置修改进入底部待应用栏，通过 `base_hash` 乐观锁校验后原子生效。
  - **深浅色主题与响应式**：默认提供建筑图纸风格浅/深色主题切换，适配 1024px 与窄屏抽屉导航。
- **热重载语义与运维说明**：
  - **无损在途请求**：所有请求在进入门禁流水线时绑定当前配置快照；热重载通过原子切换快照立即对新请求生效，正在执行的流式或长耗时请求不会被打断或受半截配置影响。
  - **无需重启 vs 需重启字段**：`providers`、`routing_groups`、`default_group`、`model_aliases`、`redaction`、`max_body_mb`、`request_log_days`、`tokens.default_ttl` 均**支持热重载即时生效**；`listen`、`mode`、`data_dir`、`admin.*`、`meter.*`、`audit.*`、`org.*`、`plugins.*` 等启动期基础设施字段变更后会在控制台与接口返回 `restart_required` 提示，需重启进程后生效（热重载期间也不主动拉起新插件进程）。
  - **外部文件监听与 `SIGHUP`**：`gatehouse serve` 自动轮询监听 `config.yaml` 的内容变更（约 1.5s 内自动校验并加载），同时支持向进程发送 `SIGHUP` 信号触发立即重载；若磁盘新配置不合法或构建失败，网关将继续保留上一有效快照运行并在控制台暴露 `last_error`。
  - **YAML 写回与 30 份自动备份**：通过 Web 控制面板保存或回滚配置会以结构化 YAML 原子写入 `config.yaml`（权限 `0600`），**原文件中的手写 YAML 注释将被移除**；每次改写前会自动将旧版本备份至 `<data_dir>/config-history/`（最多保留最近 30 份），可在控制台「系统与配置」查看逐行 Diff 并一键回滚。
  - **上游密钥安全存储（`db:` `key_ref`）**：可在渠道编辑抽屉或调用 `PUT /admin/api/providers/<name>/key` 将上游密钥直接写入 SQLite 密钥库并引用 `db:<name>`，避免明文密钥落入 `config.yaml` 或配置历史备份。
- **安全建议**：`admin.listen` 建议仅绑定 `127.0.0.1` 或内网地址，再通过 SSH 端口转发或带 IP 白名单/HTTPS 的 Nginx 暴露，不要直接对公网裸露管理端口。

### gpu1 链路参考配置示例（仅作示例，未部署）

若将 Gatehouse 串入 `Nginx :18000 → Gatehouse :18001 → smart-router :29999 → SGLang` 链路，示例配置如下：

```yaml
listen: 127.0.0.1:18001
mode: personal
max_body_mb: 0                    # 不限制大上下文/多图请求体大小
request_log_days: 90

admin:
  listen: 127.0.0.1:18002         # 仅监听本机，配合 SSH 隧道或 Nginx 白名单访问
  session_ttl: 12h

providers:
  - name: ds
    base_url: http://127.0.0.1:29999/v1
    target: local                 # 必须标为 local，保留流式且不触发外部缓冲
    timeout: 3600s                # 长上下文生成超时
    models: [deepseek-v4]
    system_prompt: |
      每次在 <think>...</think> 块中进行内部推理思考时，请使用简体中文撰写。

default_group: main
routing_groups:
  main:
    type: order
    members: [ds/deepseek-v4]

model_aliases:
  - match: "claude-*"
    use: main
  - match: "gpt-*"
    use: main

redaction:
  mode_by_target:
    local: off                    # 纯内网自建模型关闭脱敏扫描
    external: mask
```

## org 模式

`mode: org` 时令牌交给一个 `auth_policy` 插件解析（租户、用户、部门、数据分级、允许的路由组、预算），插件不可用就对所有请求返回 503。最小配置：

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

插件用同仓库 `plugin` 包写：`plugin.Serve(plugin.Set{AuthPolicy: impl})`。五类扩展点与宿主行为见 [SPEC 3.6](docs/SPEC.md)。

## 状态

首期已实现：G1 路由、G2 脱敏、G3 fail-closed、G4 令牌、G5 org 模式与五类插件扩展点、G6 计量；扩展已实现：G7 多 `sk-` API Key 与次数限额、G8 Anthropic/Responses 协议适配、G9 Web 控制面板。参考实现：magpie（路由组、意图路由、脱敏）。许可：MIT。
