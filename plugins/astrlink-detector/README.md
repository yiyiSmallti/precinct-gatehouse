# astrlink-detector

Gatehouse 的 NER 检测器插件：用 ONNX Runtime 运行 [AstrLink Guard](https://huggingface.co/QuantumNous/astrlink-guard)（多语言 MiniLM 微调的 token 分类模型，int8 约 36 MiB），识别中英文人名、地址、账号、日期、电话、邮箱、银行卡和密钥。它是 go-plugin gRPC 子进程，网关通过 `plugins[]` 加载。

插件需要 cgo（`onnxruntime_go` 在运行时 `dlopen` ONNX Runtime），所以是独立的 Go module；网关主程序仍是无 CGO 构建。

## 需要的文件

| 文件 | 来源 | 说明 |
|---|---|---|
| `astrlink-detector` | 本目录构建 | 不链接 ONNX Runtime，运行时按 `-ort` 加载 |
| ONNX Runtime 动态库 1.27.x | [microsoft/onnxruntime Releases](https://github.com/microsoft/onnxruntime/releases) | 需 ≥1.26（`onnxruntime_go` v1.31.0 用 C API 26），版本固定：int8 数值随版本变化。Linux 官方包要求 glibc ≥2.28，不支持 musl |
| 模型目录 | HF `QuantumNous/astrlink-guard` 的 `int8/`，revision `92190a3eebcbc0ca53529e22b14fb4df70ec14cf` | `model_int8.onnx` 与 `model_int8.onnx_data` 必须在同一目录，另需 `tokenizer.json`、`config.json`（标签顺序从这里读） |

模型文件校验（sha256）：

```text
0a070d3113615e33ba3b84d6bd486f663a377c04c4b056e7381732179f51832a  model_int8.onnx
0ba34216ceead01e0ae2dfd8e46d52b67e7aee80b04f722d9b758613a91c0067  model_int8.onnx_data
0cf3b235ef125658015a6c8026a5fe6f28088bf825101a990ec18a10200feef4  tokenizer.json
2c20003590d642e273c03c4c7c82d9849fa75f64bf29a71ec0a0c9a20ba4a150  config.json
```

模型为 Apache-2.0；训练数据要求署名（Nemotron-PII，CC BY 4.0 等），分发时附上模型的 `LICENSE`、`licenses/` 与 ONNX Runtime 的 `LICENSE`、`ThirdPartyNotices.txt`。

## 构建

```bash
cd plugins/astrlink-detector
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o astrlink-detector .
```

需要目标平台的 C 工具链：linux/amd64 在 Linux 上原生构建（CI 的 `check` 任务就是这样编译的），或用 `CC="zig cc -target x86_64-linux-gnu.2.28"` 交叉编译；windows 用 mingw-w64 或在 Windows 上构建，`-ort` 要写绝对路径，以免加载到 System32 里 Windows ML 自带的旧版 `onnxruntime.dll`。

## 网关配置

```yaml
plugins:
  - name: astrlink
    kind: detector
    path: /opt/gatehouse/plugins/astrlink-detector
    args: ["-model", "/opt/gatehouse/models/astrlink-int8", "-ort", "/opt/gatehouse/lib/libonnxruntime.so.1.27.0"]
redaction:
  mode_by_target: { local: off, external: mask }
  detectors: [astrlink]
  categories: { secret: block }          # 只对规则命中的密钥生效：NER 结果受下面 max_action 限制
  sources:
    astrlink:
      min_confidence: { default: 0.8, person: 0.9, date: 0.9, account: 0.7 }
      max_action: audit                  # 先影子运行 1–2 周，看审计里 source=astrlink 的样本，再改为 mask
      timeout: 10s
      on_error: rules_only               # 超时、出错或插件重启中时只用规则继续
```

阈值来自 [评测](eval/REPORT.md)：人名常见误报是产品名，日期常见误报是相对时间，所以这两类用 0.9；账号召回最弱、精确率高，用 0.7。`max_action` 不要设成 `block`：在真实编程请求里，模型平均每 1 万 token 约有 0.3 处误报（大多是 `common_secret`）。

网关按（插件名, 文本）缓存检测结果，多轮对话的历史消息不会重扫；但首轮长请求仍要全量检测（本机 M4 约 1 秒 2 万字符，13 万 token 约 15–22 秒），`timeout` 按实际请求长度设。

## 类别映射

| 模型标签 | 插件输出 kind | `categories` / `min_confidence` 键 |
|---|---|---|
| `private_person` | `PERSON` | `person` |
| `private_address` | `ADDRESS` | `address` |
| `account` | `ACCOUNT` | `account` |
| `common_secret` | `NER_SECRET` | `ner_secret`（与规则的 `secret` 分开） |
| `private_date` | `DATE` | `date` |
| `phone` | `PHONE` | `phone` |
| `email` | `EMAIL` | `email` |
| `payment_card` | `NER_CARD` | `ner_card`（会员卡、校园卡常被判成银行卡，不复用 `BANK_CARD`） |
| `ip_address`、`url` | 丢弃 | 交给规则 |

用 `-kinds label=KIND,...` 可以改映射，`KIND` 写 `-` 表示丢弃。

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-model` | `$ASTRLINK_DIR` | 模型目录 |
| `-ort` | `$ORT_LIB` | ONNX Runtime 动态库 |
| `-min` | `0.5` | 下限：低于它的结果不上报。按类别的阈值在网关 `min_confidence` 设置，可热重载 |
| `-kinds` | 见上表 | 标签 → kind |
| `-threads` | `4` | 每次推理的 ORT 线程数（0 为全部核） |
| `-batch` | `4` | 每次推理的 512-token 窗口数 |
| `-concurrency` | `1` | 同时推理的 `Detect` 调用数，避免多个请求的线程池互相抢核 |
| `-wordvote` | `true` | 同一 ASCII 单词的子词统一类别 |
| `-skip-fenced` | `false` | 跳过 ``` 代码块（评测显示不减少误报、反而漏检代码块里的实体，不建议开） |

插件不能往 stdout 打印任何东西：go-plugin 握手占用 stdout。模型在握手前加载，加载失败时插件启动失败，网关按检测器不可用处理（`on_error`）。

## 验收与评测

- 端到端验收：`e2e/g19_astrlink_test.go`（需本机模型，CI 只编译）：

  ```bash
  ASTRLINK_DIR=/path/to/astrlink-guard/int8 ORT_LIB=/path/to/libonnxruntime.1.27.0.dylib go test ./e2e/ -run TestG19AstrLink -count=1
  ```

  报告在 `e2e/report/G19-astrlink.md`。
- 模型评测（样本集、分词器与 HF 对拍、阈值、误报密度、性能）：[eval/REPORT.md](eval/REPORT.md)，复现脚本 `eval/run.sh`。
