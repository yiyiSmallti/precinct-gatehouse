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

## 状态

设计阶段。参考实现：magpie（路由组、意图路由、脱敏）。许可：MIT。
