# AstrLink Guard 作为 Gatehouse NER 模型的独立评测

> 本报告写于插件原型阶段（原目录 `bench/ner/`）。原型已整理为 `plugins/astrlink-detector/`，第 7 节列出的核心改动已在 #33 实现；阈值现在由网关的 `redaction.sources.<名字>.min_confidence` 设置（见插件 README）。下文保留评测时的原貌，路径已改为现在的位置。

> 2026-11-25，模型 `QuantumNous/astrlink-guard` 0.1.3（int8/fp32 文件与 0.1.2 相同），本机 Apple M4（4 性能核 + 6 能效核，16 GB），ONNX Runtime 1.27.0。样本全部为合成数据。

## 结论

**可以用，但只能作为"脱敏模式下的补充检测器"，不能直接接入现有网关核心。** 质量足够：在自建样本集上，int8 五类宏 F1 为 0.937（严格边界）/ 0.953（重叠匹配），在 34 万 token 的真实 Go 代码和文档上**人名误报为 0**。卡点是性能：本机单个 512 token 窗口约 45–55 ms，13 万 token 的提示全量扫描要 15–22 s，而宿主对单次 `Detect` 的硬超时是 5 s（`internal/plugin/host.go:24`）。冒烟里一条约 16 万字符的消息已被 fail-closed 拦成 `503`。因此**增量检测缓存是上线前提**，超时退化、按来源限制动作也要一起做（见第 7 节）。

| 决策项 | 建议 | 依据 |
|---|---|---|
| 精度 | **int8** | fp32 在本样本集上与 int8 持平（宏 F1 0.935 vs 0.937），单窗口慢约 44%，常驻内存多约 50 MB |
| 默认阈值 | **按类别**：`private_person` 0.9、`private_date` 0.9、`account` 0.7，其余 0.8 | 人名 0.9 时反例上的人名误报 5→2，召回只降 0.8 个点；日期 0.9 时精确率 0.80→0.86，召回不变；账号 0.7 时召回 0.84→0.89，精确率仍为 0.97。合成后宏 F1 约 0.948 |
| 扫描范围 | **全文**（只扫增量消息），不跳过 fenced 代码块 | 跳过代码块误报一个没少，但放在代码块里的 60 个实体全部漏掉，覆盖率 95.5%→85.1% |
| 类别映射 | 人名/地址/账号/日期用新类别 `person/address/account/date`；密钥和卡号用 `NER_SECRET`/`NER_CARD`，与规则的 `SECRET`/`BANK_CARD` 分开；`url`、`ip_address` 丢弃 | 见第 6 节 |
| NER 来源的动作 | 一律 **`mask`**，永不 `block` | 真实代码里约每 10 万 token 有 2 处 `common_secret` 误报（`gh_admin`、`hasPwd`、`.yaml`）；如果沿用 `secret: block`，大多数长编程请求都会被整条拦截 |

## 1. 模型与文件核对

- 本机 Magpie 缓存中的 int8 文件（`model_int8.onnx`、`model_int8.onnx_data`、`tokenizer.json`）与 HF `SHA256SUMS` **全部一致**；fp32 下载后（`fp32/model.onnx` 219,740 B、`fp32/model.onnx_data` 147,443,712 B）也校验通过。模型文件与 ORT 库都放在 worktree 外，没有进仓库。
- 与背景信息不一致的地方：
  - 计算图有**三个**输入：`input_ids`、`attention_mask`、`token_type_ids`（全 0），输出 `logits [batch, seq, 21]`。
  - HF 仓库根目录没有 `labels.json`/`label_mapping.json`/`astrlink-model.json`，它们在 `int8/`、`fp32/` 子目录里；标签顺序以 `config.json` 的 `id2label` 为准（`0=O, 1/2=email, 3/4=phone, 5/6=account, 7/8=payment_card, 9/10=ip_address, 11/12=url, 13/14=common_secret, 15/16=private_address, 17/18=private_date, 19/20=private_person`，奇数 B、偶数 I）。
  - 自 0.1.2 起，`private_date` **只标与具体个人相关的日期**，日志时间、git 日期、发布日期都不标。
  - 模型卡说官方解码包含 `bio-offset-consistency-v1` 和 `existing-ip-and-dynamic-url-v1` 两步，但没有公开细节。本原型的解码是自己实现的近似（第 5 节）。
- **int8 数值随 ORT 版本漂移**：同样的输入，Go+ORT 1.27 与 Python+ORT 1.26 的 logits 最大差 0.24，3,185 个 token 中 argmax 0 处不同；与 Python+ORT 1.19 比，最大差 1.25，有 3 处不同。部署时要固定 ORT 版本，换版本后重跑评测。

## 2. 样本集（`eval/data/`，由 `gen/gen_samples.py` 以固定种子生成）

| 文件 | 内容 | 数量 |
|---|---|---|
| `positives.jsonl` | 人名、地址、账号、密钥（无厂商前缀、无 `key=value` 字段名）、个人日期 × 中文/英文/中英混排，每格 34 条；其中 60 条的实体放在 fenced 代码块里（JSON/YAML/CSV/SQL 测试数据、命令行） | 510 |
| `negatives.jsonl` | 编程助手请求的典型内容：Go/TS/Python/Java/Rust/Kotlin/Swift/C 代码、import 路径、注释、`go test`/pytest/npm/kubectl 输出、日志、panic 栈、git 提交信息、技术术语，以及易混产品名（Jenkins、Hugo、Kafka、Cassandra、鸿蒙、飞书）和易混中文词（张量、李代数、高斯、王者荣耀、江湖）；含哈希、UUID、请求 ID、占位符配置 | 中 83 + 英 83，全部互不相同 |
| `requests.jsonl` | 模拟真实请求结构：system 提示 + user/assistant 多轮 + 工具输出（源文件、测试输出），每条约 650 token；12 条干净，12 条在最后一轮带 1–3 个实体 | 24 |

每条样本记录实体的类别和 Unicode 码点区间 `[start, end)`。人名由姓和名随机组合，地址的道路名是虚构的，账号、密钥、日期均为随机生成，**没有使用真实个人信息**。

**局限**：模板化的合成样本句式重复，和模型训练用的合成数据风格接近，F1 很可能偏乐观。所以另外用仓库真实代码测了误报密度（3.4 节）。

## 3. 准确率（int8，词内投票解码，全文扫描）

评分口径：**严格** = 类别相同且边界完全一致；**重叠** = 类别相同且区间有重叠（对脱敏来说，重叠就意味着原值不会明文发出）。"被任一类别覆盖" = 有任意类别的 span 与实体重叠（只要动作是 mask，类别判错也能遮住）。完整表格和全部误报/漏报例子见 `results/eval-int8-wordvote.md`。

### 3.1 阈值对比（五类合计）

| 阈值 | 人名 F1 | 地址 F1 | 账号 P / R | 密钥 F1 | 日期 P / R | 宏 F1 严格 / 重叠 | 实体被任一类别覆盖 | 干净单元≥1 处误报（不含 url/ip） |
|---|---|---|---|---|---|---|---|---|
| 0.7 | 0.950 | 1.000 | 0.969 / 0.886 | 0.976 | 0.777 / 0.904 | 0.937 / 0.955 | 96.8% | 2.8%（5/178） |
| **0.8** | 0.957 | 1.000 | 0.989 / 0.838 | 0.971 | 0.803 / 0.904 | **0.937 / 0.953** | 95.5% | 2.2%（4/178） |
| 0.9 | 0.963 | 1.000 | 1.000 / 0.771 | 0.976 | 0.862 / 0.904 | 0.938 / 0.950 | 93.0% | 1.1%（2/178） |
| 0.95 | 0.970 | 1.000 | 1.000 / 0.724 | 0.971 | 0.922 / 0.904 | 0.939 / 0.950 | 91.9% | 0%（0/178） |

干净单元 = 166 条反例 + 12 条干净多轮请求。如果把 url/ip 也计入，阈值 0.8 时为 5.6%（10/178），多出来的几乎都是日志里的 `127.0.0.1`。模型卡自报的是 20/187（10.7%），但它的请求更长、来源更真实，两者不能直接比较。

### 3.2 按语言（阈值 0.8）

| 语言 | 人名 | 地址 | 账号 | 密钥 | 日期（严格 / 重叠） | 宏 F1 严格 / 重叠 |
|---|---|---|---|---|---|---|
| 中文 | 0.922 | 1.000 | 0.958 | 0.971 | 0.757 / 0.919 | 0.922 / 0.954 |
| 英文 | 0.989 | 1.000 | 0.923 | 0.971 | 0.838 / 0.919 | 0.944 / 0.960 |
| 中英混排 | 0.963 | 1.000 | **0.828** | 0.971 | 0.959 / 0.959 | 0.944 / 0.944 |

中文日期的严格 F1 偏低有两个原因。一是标注口径：模型把时段一起框进去（`【10月7日下午三点】`、`【1986年10月11号上午】`），而样本只标到日期；按模型卡的定义，时段本来就属于 `private_date`，这部分不算模型的错。二是真误报：模型把"下个月""今天下午"这类相对时间也标成日期。按重叠口径，中文日期 F1 为 0.92。混排里的账号最弱：用户名（`harbor_t5756`、`lucastiger90`）常被判成 `common_secret`。

### 3.3 扫描范围：全文 vs 跳过 fenced 代码块（阈值 0.8）

| 范围 | 宏 F1 严格 | 实体被覆盖 | 代码块内实体检出 | 干净单元≥1 处误报（不含 url/ip） |
|---|---|---|---|---|
| **全文** | 0.937 | 95.5% | 96.7%（58/60） | 2.2% |
| 跳过代码块 | 0.878 | 85.1% | 0%（0/60） | 2.8% |

反例里的误报都出现在正文（产品名、术语），跳过代码块减少不了；反而因为切段后上下文变短，多出 1 处误报。另一方面，实际请求里的工具输出（读文件、命令结果）本来就不包在 fenced 代码块里，跳过代码块对真实请求的提速也有限。**不建议默认跳过**，插件保留了 `-skip-fenced` 开关。

### 3.4 真实代码上的误报密度

用本仓库 `internal/`、`cmd/`、`plugin/` 的非测试 Go 文件和 `docs/*.md`（73 个文件，341,722 token）做干净语料，阈值 0.7，得到 41 个 span（fp32 为 40 个）：

| 类别 | 数量 | 内容 |
|---|---|---|
| `ip_address` | 19 | 文档和代码里的 `127.0.0.1`、DNS 服务器地址 |
| `url` | 11 | `http://127.0.0.1:7788/v1`、`https://api.github.com` 等 |
| `common_secret` | 8（≥0.8 有 6 个） | `cookieName = "gh_admin"`、`HasPassword: hasPwd`、`".yaml"`、`Ed25519`、插件握手魔数 `4c1f0b5e-precinct-gatehouse`、规则表里的 `"ghu_"` 前缀 |
| `account` / `phone` / `email` | 各 1 | `json:"username,omitempty"` 里的 `omitempty`；注释里的示例手机号；SPEC 里的示例邮箱 |
| `private_person` | **0** | |

去掉 url/ip 后，阈值 0.7 时误报密度约为每 1 万 token 0.32 个，也就是一个 13 万 token 的编程请求**平均约 4 处**，其中大部分是 `common_secret`。所以 NER 来源的密钥只能 `mask`：被误遮的代码常量会在响应里回填，代价是模型看到的是占位符；如果走 `block`，整个请求都会失败。

### 3.5 典型误报与漏报（阈值 0.8）

误报：
- 产品名和中文词被当成人名：`Compare 【Hugo】, Gatsby and Astro`（0.918）；`【李】代数和李群`（0.841）；`【王】者【荣耀】`（0.876/0.853）；`【江湖】上说 Rust…`（0.915）。人名阈值提到 0.9 后只剩 `Hugo` 和 `江湖` 两处。
- 相对时间被当成个人日期：`她【今天下午】在`、`我【下个月】搬到…`、`how old will I be 【next year】`、`interned with us 【last summer】`（0.83–0.95）。
- 用户名被判成密钥：`用户 【harbor_t5756】`（common_secret 0.89）、`WeChat: 【li8ciqvhcqr】`（0.80）。
- 日志里的 `addr=【127.0.0.1】:8080`（ip_address 0.99）。

漏报：
- "X老师"连称：`尊敬的【任振华】老师` 被框成"任振华老师"，分数 0.60–0.73，低于阈值。
- 拼音人名被切成子词后各给各的标签：`【Zheng Fang】` → `Z`=account、`heng`/`ang`=person，单个分数都在 0.5 左右。词内投票解码修好了部分英文名，`Zheng Fang`、`Min Cao` 仍然漏。
- 分组数字的会员卡号：`会员卡号【4648 4937 5009】` 在 account 和 payment_card 之间摇摆，分数 0.50–0.80。
- 无上下文的随机密钥：`I pasted 【yn4ES0zrKpd4qBKH7mw2VbGp】 into the header` 完全没检出（fp32 同样）；口令型密码 `Pepper67@aw`、`Copper2775%UH` 的分数 0.64–0.79。这与模型卡"最容易漏密码和无字段名的密钥"一致。

### 3.6 int8 vs fp32（全文，词内投票）

| | int8 | fp32 |
|---|---|---|
| 宏 F1 严格，阈值 0.8 / 0.9 | 0.937 / 0.938 | 0.935 / 0.938 |
| 干净单元≥1 处误报（不含 url/ip），0.8 / 0.9 | 2.2% / 1.1% | 2.2% / 0.6% |
| 真实代码 34 万 token 的 span 数（≥0.7） | 41 | 40 |
| 单窗口 p50 / p95（4 线程） | 44.7 / 71.1 ms | 64.5 / 88.3 ms |
| 13.1 万 token 全量扫描 | 22.9 s | 29.2 s |
| 冷启动（文件已缓存）/ 加载后 RSS | 58 ms / 84 MB | 82 ms / 135 MB |

fp32 唯一的优势是阈值 0.9 时少 1 处误报，不值得多付这些延迟和内存。**选 int8。**

## 4. 性能（int8，本机 M4，测量时后台负载 load average 约 6–7）

| 线程数 | 单窗口 p50 | p95 | batch 8 每窗口 | 2k token | 8k token | 32k token | 131k token |
|---|---|---|---|---|---|---|---|
| ORT 默认（10 核） | 54.5 ms | 71.7 ms | 63.4 ms | 0.35 s | 1.31 s | 5.86 s | 21.8 s |
| 1 | 78.5 ms | 91.1 ms | 90.6 ms | 0.68 s | 2.03 s | 6.90 s | 31.8 s |
| 2 | 54.6 ms | 130.7 ms | 83.0 ms | 0.30 s | 1.86 s | 7.34 s | 29.8 s |
| **4** | **44.7 ms** | 71.1 ms | 59.6 ms | 0.30 s | 1.50 s | 8.13 s | 22.9 s |

（"2k token"等列是端到端 `Detect` 时间，含分词、滑窗、解码。Python+ORT 1.26 同一窗口 p50 为 53 ms，说明 Go 绑定没有额外开销。）

- 分词很快：13.1 万 token 只要 70–120 ms，耗时几乎全在模型推理。
- 冷启动：第一次（文件不在页缓存）运行时初始化加模型加载共 197 ms，之后约 58 ms；网关带插件启动（含 go-plugin 握手）共 1.6 s。
- 内存：加载后 RSS 约 84 MB；ORT 的内存池会随 batch 和长文本增长，插件（batch 4）跑完长消息后 RSS 约 265 MB，perf 测试（batch 8）峰值 488 MB。
- **估算**：窗口数 ≈ ⌈(N − 510) / 382⌉ + 1，13 万 token 约 341 个窗口，按 45–65 ms 每窗口计为 **15–22 s**；只扫每轮新增的 2k–8k token 约 **0.3–1.5 s**。宿主的 5 s 超时下，单次 `Detect` 在本机最多能处理约 2.5 万 token。gpu1 是 Linux x86 服务器，没有实测；如果 CPU 支持 AVX-512 VNNI，int8 可能更快，需要在 gpu1 上跑 `run.sh perf` 确认。

## 5. Go 插件原型（现为 `plugins/astrlink-detector/`，独立 Go module，不影响网关主程序无 CGO 构建）

```
bench/ner/
  astrlink/tokenizer.go   XLM-R Unigram 分词器（读 tokenizer.json）
  astrlink/model.go       ONNX Runtime 会话（onnxruntime_go v1.31.0，cgo + dlopen）
  astrlink/detect.go      512 窗口 / 128 重叠滑窗、BIO 解码、边界修剪、跳过代码块（可选）
  cmd/astrlink-detector   Detector 插件（plugin.ServeDetector）
  cmd/nereval             评测、真实文件扫描、性能、冷启动、logits 导出
  cmd/tokdump             分词结果导出（用于对拍）
  cmd/nersmoke            真实 gatehouse serve 冒烟
  gen/                    样本生成、分词对拍、ORT 数值对拍
```

- **分词器**：完整复刻 tokenizer.json 的流水线：特殊 token 切分（`<mask>` 的 lstrip）→ SentencePiece precompiled charsmap（darts 双数组 trie，按扩展字素簇查表、只取最短前缀，这是 HF 实现自带的怪异行为）→ `(?<![0-9\s])\s+|^\s+` 去空白（Go regexp 不支持后行断言，手写；`^` 按 Oniguruma Ruby 语法匹配行首）→ 单个空白独立成片 → Metaspace（每片都补 `▁`，对齐到首字符）→ Unigram Viterbi（同分取较短者，连续 unk 合并）。偏移按 HF `NormalizedString` 的对齐规则换算回原文码点。**对拍结果：877 段文本（全部样本 + 33 条边界用例：零宽字符、CRLF、BOM、emoji 组合序列、全角、`™ﬁ①`、`1\n  x` 等）共 36,432 个 token，id 与码点偏移与 HF tokenizers 0.22.2 完全一致。**
- **解码**：逐 token 取 softmax argmax；B 开新 span，I 续接同类 span，孤立的 I 也开新 span；同类且字符区间相接的 span 合并；span 分数取其中最低的 token 概率。**词内投票**（默认开启）：同一个 ASCII 单词的子词取平均概率，统一成同一个类别，让宏 F1 从 0.934 升到 0.937、账号召回从 0.819 升到 0.838，误报不变。最后修剪首尾空白和标点，去掉 IP 的 `:port`、URL 末尾的句读。重叠窗口里的 token 取它离窗口边缘最远的那个窗口的预测。
- **插件**：阈值和标签→kind 映射都在插件里完成，因为宿主会丢弃 `Confidence`。支持取消：宿主超时后，插件在批次之间检查 ctx 并停止，不会在后台继续占 CPU。用 `-concurrency`（默认 1）限制同时推理的调用数，避免多个请求的 ORT 线程池互相抢核。参数：`-model -ort -min -thresholds -kinds -skip-fenced -threads(4) -batch(4) -wordvote(true)`。注意不能往 stdout 打印任何东西，go-plugin 握手占用了 stdout。
- **冒烟**（`results/smoke.md`，真实 `gatehouse serve` 只启用这一个检测器，假上游会回显请求内容）：11 项全部通过。
  - 出站请求里，`王建国`、`Emily Carter` 和完整地址被替换成 `{{PERSON_…}}`/`{{ADDRESS_…}}`，代码块里的 `AccountService` 原样保留；响应回填为原文；`X-Gatehouse-Redaction: mask; address=1; person=2`。
  - 干净的编程请求原样发出，耗时 7 ms。
  - 模型判出的密钥以 `ner_secret=1` 遮盖，没有触发同一份配置里的 `secret: block`。
  - 约 16 万字符的消息在 5,013 ms 时返回 `503 redaction_unavailable`（`DeadlineExceeded`），没有发往上游。

## 6. 类别映射与默认动作建议

| AstrLink 标签 | 插件输出 kind（占位符） | `redaction.categories` 键 | NER 来源默认动作 | 建议阈值 | 说明 |
|---|---|---|---|---|---|
| `private_person` | `PERSON` | `person` | mask | 0.9 | 真实代码 0 误报；常见误报是产品名 |
| `private_address` | `ADDRESS` | `address` | mask | 0.8 | 本样本集 F1 1.00 |
| `account` | `ACCOUNT` | `account` | mask | 0.7 | 召回最弱，精确率高 |
| `common_secret` | `NER_SECRET` | `ner_secret` | **mask（永不 block）** | 0.8 | 和规则的 `SECRET` 分开，规则命中的厂商前缀密钥仍可 `block`；两者重叠时按现有逻辑取较强动作 |
| `private_date` | `DATE` | `date` | mask | 0.9 | 相对时间是主要误报 |
| `phone` | `PHONE` | `phone` | mask | 0.8 | 与规则共用类别，补充国际号码格式 |
| `email` | `EMAIL` | `email` | mask | 0.8 | 与规则共用类别 |
| `payment_card` | `NER_CARD` | `ner_card` | mask | 0.8 | 不复用 `BANK_CARD`：会员卡、校园卡常被误判成银行卡 |
| `ip_address` | 丢弃 | — | — | — | 交给规则 `IP`；模型在日志里大量标 `127.0.0.1` |
| `url` | 丢弃 | — | — | — | 交给规则 `DOMAIN`（内部域名）；公开 URL 在编程请求里不敏感 |

配置示例：

```yaml
plugins:
  - name: astrlink
    kind: detector
    path: /opt/gatehouse/plugins/astrlink-detector
    args: ["-model", "/opt/gatehouse/models/astrlink-int8", "-ort", "/opt/gatehouse/lib/libonnxruntime.so.1.27.0"]
redaction:
  mode_by_target: { local: audit, external: mask }
  detectors: [astrlink]
  categories: { person: mask, address: mask, account: mask, ner_secret: mask, date: mask, ner_card: mask, secret: block }
```

**NER 来源需要区别对待的原因**：规则命中的是格式确定的值（带厂商前缀的密钥、带校验位的证件号），误报很少，`block` 是安全的；模型给出的是概率判断，在长编程请求里几乎每条都会有误报。现有核心只按 kind 决定动作，`mode_by_target: block/warn` 还会覆盖类别动作（`internal/redact/redact.go:138`）。插件目前用 `NER_` 前缀的 kind 绕开 `secret: block`，但对 `block` 模式的目标仍然无效，必须在核心里按来源加动作上限（第 7 节第 3 条）。

## 7. 网关核心需要配合的改动（等 Vault 分支合并后做）

1. **增量检测缓存（上线前提）**：按（检测器名 + 版本 + 阈值配置 + 文本哈希）缓存每个字符串的检测结果，多轮请求只扫新增消息。没有它，每一轮都要重扫全部历史，13 万 token 的会话每轮多花 15–22 s。SPEC 3.2 已经写了这一条，但还没实现。
2. **超时与退化**：`DetectTimeout` 现在是 5 s 常量。改成按检测器配置（如 `timeout: 3s`），并新增 `on_error: fail_closed | rules_only`。NER 超时或出错时退化为只用规则和词典，并在审计里记告警，不要返回 503；对规则检测器仍保持 fail-closed。另外建议设一个整请求的检测预算：宿主现在是对每个字符串单独计时，一个请求里如果有很多中等长度的工具输出，总耗时会叠加。
3. **按来源设置动作上限**：`redact.Finding.Source` 已经记录了检测器名，可以加 `redaction.sources.<name>.max_action: mask`，让来自 NER 的发现在 `warn/block` 模式或类别配置为 `block` 时也最多 `mask`。重叠聚类时按"规则 > NER"取动作。
4. **`min_confidence` 下沉到核心**：宿主现在丢弃了 `Finding.Confidence`（`internal/plugin/host.go:176`）。保留置信度，支持 `redaction.sources.<name>.min_confidence: {default: 0.8, person: 0.9, date: 0.9, account: 0.7}`，这样阈值可以热重载，也能写进审计样本，插件参数只保留模型路径。
5. **传角色提示**：`Detect` 的 `hints` 现在是 nil。建议传 `role`（system/user/assistant/tool）和字段类型（工具 schema、tool 结果）。这样可以按角色配置扫描范围，例如跳过客户端固定的 system 提示和工具定义：每轮都在、内容固定，缓存能覆盖，但首轮仍要付全量成本。
6. **影子运行**：先在 `audit` 模式下对真实流量启用 NER，只记录各类别计数和长度（审计记录本来就不含原文），观察 1–2 周的误报分布后再切到 `mask`。

## 8. 分发与交叉编译

需要分发的文件：

| 文件 | 大小 | 说明 |
|---|---|---|
| `astrlink-detector`（`.exe`） | 约 20 MB（darwin/arm64） | 不链接 ORT，运行时 `dlopen` |
| ONNX Runtime 动态库 1.27.x | darwin arm64 `libonnxruntime.1.27.0.dylib` 37 MB；linux x64 `libonnxruntime.so.1.27.0`；windows `onnxruntime.dll` | 版本要 ≥1.26（C API 26），并且固定：int8 数值随版本变化 |
| `model_int8.onnx` + `model_int8.onnx_data` | 323 KB + 35.4 MB | 两个文件必须放在同一目录 |
| `tokenizer.json`、`config.json` | 1.4 MB、1.8 KB | 标签顺序来自 `config.json` |
| `LICENSE`、`licenses/`（模型）+ ORT 的 `LICENSE`/`ThirdPartyNotices.txt` | | 模型为 Apache-2.0，训练数据要求署名（Nemotron-PII CC BY 4.0 等） |

交叉编译：

- `onnxruntime_go` 运行时用 `dlopen`/`LoadLibrary` 加载 ORT，构建时不链接 ORT 库，但包装层是 cgo：`CGO_ENABLED=0` 会被构建约束整体排除；用 macOS 自带 clang 交叉编译到 linux/amd64 失败在汇编（`gcc_amd64.S: unknown token`），编 windows/amd64 失败在 `windows.h not found`。**需要目标平台的 C 工具链**：
  - **linux/amd64**：`CC="zig cc -target x86_64-linux-gnu.2.28" CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build ./cmd/astrlink-detector`，或在 Linux CI 上原生构建。官方 ORT Linux 包基于 manylinux_2_28，要求 glibc ≥2.28，不支持 musl/Alpine。
  - **windows/amd64**：mingw-w64（`CC=x86_64-w64-mingw32-gcc`）或 zig；更稳妥的是在 Windows CI 上原生构建。坑：System32 里有 Windows ML 自带的旧版 `onnxruntime.dll`，`-ort` 必须传**绝对路径**，否则可能加载到系统里的旧版本；go-plugin 在 Windows 上走 TCP 回环，`path` 要写到 `.exe`。
  - 本机没有 zig/mingw，Docker 守护进程也没运行，**这次没有实际产出 linux/windows 二进制**。
- 绑定版本要与 ORT 库匹配：`onnxruntime_go` v1.38.0 需要 C API 31（ORT ≥1.31），本机只有 1.27，所以用了 v1.31.0（API 26）。升级 ORT 时两者一起升。
- 可选方向：基于 purego 的 ORT 绑定可以做到 `CGO_ENABLED=0` 交叉编译，但没有找到成熟实现，需要自己写，未评估。

## 9. 复现

```bash
# 需要：Go 1.26+；ONNX Runtime ≥1.26 动态库；HF int8 模型目录；Python + tokenizers（+ onnxruntime、numpy，用于对拍）
export ORT_LIB=/path/to/libonnxruntime.1.27.0.dylib
export ASTRLINK_DIR=/path/to/astrlink-guard/int8      # model_int8.onnx(+_data)、tokenizer.json、config.json
export ASTRLINK_FP32_DIR=/path/to/astrlink-guard/fp32 # 可选
plugins/astrlink-detector/eval/run.sh   # 全部步骤；或 samples | tokenizer | parity | eval | scan | perf
```

各步骤的产物都在 `eval/results/`：`eval-int8-{plain,wordvote}.{md,json}`、`eval-fp32-wordvote.*`、`perf-*.json`。`smoke.md` 是原型阶段冒烟工具的结果，该工具已由 `e2e/g19_astrlink_test.go` 取代。真实代码扫描的结果只打印到终端，没有入库，因为输出里包含源码片段。

## 10. 剩余不确定性

- **样本是合成的**：F1 偏乐观，账号、地址、日期的真实分布可能更难。建议按第 7 节第 6 条，用真实流量做影子评估。
- **gpu1（Linux x86）的延迟没有实测**，本机测量时后台有负载。
- **解码是近似**：官方的 offset-consistency 和 IP/URL 清理规则没有公开，边界可能与 AstrLink（Magpie）的结果略有出入；span 分数的定义（最低 token 概率，词内投票时为平均类别概率）也可能与官方不同，所以阈值是对本原型校准的。
- 多轮请求样本只有 24 条，每条约 650 token，比线上平均 13 万 token 短得多；长请求的误报率是用真实代码的误报密度推算出来的。
- linux/amd64 和 windows/amd64 构建没有实际验证，只确认了需要 C 交叉工具链。
