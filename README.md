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

首期已实现：G1 路由、G2 脱敏、G3 fail-closed、G4 令牌、G5 org 模式与五类插件扩展点、G6 计量。参考实现：magpie（路由组、意图路由、脱敏）。许可：MIT。
