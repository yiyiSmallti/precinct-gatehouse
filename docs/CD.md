# Precinct Gatehouse 持续交付（CD）使用指南

本文档说明 `precinct-gatehouse` 如何从 GitHub 仓库自动构建 `linux/amd64` 静态二进制、同步到国内私有镜像仓库（阿里云 ACR / 腾讯云 CCR 个人版），并在内网服务器 `gpu1`（`shzedu-node10`）上安全、可回滚地发布上线。

---

## 1. 整体架构

```text
打版本 Tag（如 v0.1.0）或 GitHub 网页手动触发 Run workflow
  │
  ▼
[GitHub Actions: .github/workflows/release.yml]
  ├─ 1. 质量门禁：gofmt / go vet / 全量 go test -count=1 ./...
  ├─ 2. 编译静态 linux/amd64 二进制（注入 -X main.version=<ver>）
  ├─ 3. 生成 SHA256SUMS + RELEASE_META + .tar.gz
  ├─ 4. 发布 GitHub Release（Tag 触发或勾选 create_github_release）
  └─ 5. 打包纯制品镜像（deploy/Dockerfile.artifact）推送到国内 ACR / CCR 私有仓库
          │
          ▼（gpu1 直连国内镜像，无需修改 /etc/resolv.conf，不依赖 dockerd DNS）
[gpu1 服务器: /AIOS/sglang-kunlun-v0522-1017/deploy-dsv41-1017/gateway/gatehouse/]
  ├─ pull-from-registry.py : 纯 Python 3.7 拉取 OCI 镜像层，内置 223.5.5.5 兜底 DNS
  ├─ apply-release.sh      : gatehouse config check 预检 -> 原子替换 bin/gatehouse 软链
  └─ 重启 dsv41-gatehouse.service -> /healthz 与 /admin/ 健康检查 -> 失败自动秒级回滚
```

> **核心设计原则**
> - Gatehouse 仍以 `systemd` 原生进程运行，不使用 Docker 容器托管网关（只把国内 ACR/CCR 当作免费的私有“制品中转柜”）。
> - 发布过程**只替换** `bin/gatehouse` 软链指向的二进制文件，**绝不覆盖**线上 `config.yaml` 与 `data/` 目录。

---

## 2. 开通国内免费私有镜像（约 3 分钟）

阿里云容器镜像服务（ACR）个人版或腾讯云容器镜像服务（CCR）个人版均**完全免费**且支持私有仓库（二选一即可）：

### 方案 A：阿里云 ACR 个人版（推荐）
1. 登录阿里云控制台，搜索并进入 **容器镜像服务 ACR**。
2. 开通**个人版实例**（免费），如提示设置固定登录密码，请牢记该密码（后续作为 `CN_REGISTRY_PASSWORD`）。
3. 选择地域（如华东 1 杭州），创建一个**命名空间**（如 `precinct`）。
4. 在该命名空间下创建仓库 `gatehouse`，仓库类型选 **私有**。
5. 记下公网地址，例如：
   `registry.cn-hangzhou.aliyuncs.com/precinct/gatehouse`

### 方案 B：腾讯云 CCR 个人版
1. 登录腾讯云控制台，搜索进入 **容器镜像服务 TCR -> 个人版**（免费）。
2. 初始化访问凭证（设置固定登录密码）。
3. 创建**命名空间**与**私有**镜像仓库 `gatehouse`。
4. 记下公网地址，例如：
   `ccr.ccs.tencentyun.com/<你的命名空间>/gatehouse`

---

## 3. 一次性填入凭据

开通后，只需要分别把凭据填入 **GitHub 仓库 Secrets** 和 **`gpu1` 的 `mirror.env`**（共填一次，后续发版全自动复用）：

### 3.1 配置 GitHub Actions Secrets
进入仓库页面：`Settings -> Secrets and variables -> Actions -> New repository secret`，新增以下 3 个 Secret：

| Secret 名称 | 示例值 | 说明 |
| --- | --- | --- |
| `CN_REGISTRY_IMAGE` | `registry.cn-hangzhou.aliyuncs.com/precinct/gatehouse` | 镜像地址（**不要带 `:tag`**） |
| `CN_REGISTRY_USERNAME` | 你的阿里云 / 腾讯云镜像登录用户名 | 通常为账号名或个人版分配用户名 |
| `CN_REGISTRY_PASSWORD` | 你设置的固定镜像访问密码 | 不要与云账号主密码混淆 |

也可以在已登录 `gh` 的本机直接用命令写入：

```bash
gh secret set CN_REGISTRY_IMAGE --repo yiyiSmallti/precinct-gatehouse
gh secret set CN_REGISTRY_USERNAME --repo yiyiSmallti/precinct-gatehouse
gh secret set CN_REGISTRY_PASSWORD --repo yiyiSmallti/precinct-gatehouse
```

### 3.2 配置 `gpu1` 上的 `mirror.env`
登录 `gpu1`，复制模板并填入相同的 3 项：

```bash
cd /AIOS/sglang-kunlun-v0522-1017/deploy-dsv41-1017/gateway/gatehouse
cp mirror.env.example mirror.env
chmod 600 mirror.env
vi mirror.env
```

填完后，可以在本机 Mac 执行一次检查与演练（不会重启线上网关）：

```bash
./deploy/scripts/deploy-gpu1.sh check-mirror
./deploy/scripts/deploy-gpu1.sh dry-run-mirror
```

---

## 4. 日常发版流程

### 方式一：打版本 Tag 正式发布（推荐）
在 `main` 分支拉取最新代码后打 Tag 并推送：

```bash
git tag v0.1.0
git push origin v0.1.0
```

GitHub Actions 会自动：
1. 跑完 `gofmt`、`go vet` 和全量 `go test`；
2. 构建注入 `version=v0.1.0` 的 `linux/amd64` 二进制；
3. 创建 GitHub Release `v0.1.0`；
4. 推送国内镜像 `:v0.1.0`（若版本号不含 `-`，还会同步更新 `:latest`）。

> 预发布版本（如 `v0.1.0-rc1`）会标记为 GitHub Pre-release，且**不会**覆盖 `:latest`。

### 方式二：GitHub 网页手动触发构建
打开 GitHub 仓库的 **Actions -> release -> Run workflow**：
- `version`：可选填写版本号（留空则生成 `manual-<short_sha>`）；
- `create_github_release`：是否同时发布 GitHub Release（默认关闭）；
- `push_mirror`：是否推送到国内镜像（默认开启）；
- `update_latest`：是否顺便更新国内镜像 `:latest`（默认关闭）。

---

## 5. 在 `gpu1` 上线与回滚

在本机 Mac 仓库目录执行统一脚本 `./deploy/scripts/deploy-gpu1.sh`：

| 场景 | 命令 | 说明 |
| --- | --- | --- |
| 从国内镜像拉取并部署最新版本 | `./deploy/scripts/deploy-gpu1.sh pull` | 默认拉取 `:latest`（也可指定具体版本如 `pull v0.1.0`） |
| 只检查镜像是否有更新 | `./deploy/scripts/deploy-gpu1.sh check-mirror` | 对比远端 Digest 与 `gpu1` 当前 Digest |
| 演练拉取与配置预检 | `./deploy/scripts/deploy-gpu1.sh dry-run-mirror` | 下载校验并执行 `config check`，**不重启**服务 |
| 兜底：不走镜像，从 GitHub Release 直推 | `./deploy/scripts/deploy-gpu1.sh release v0.1.0` | 由本机 Mac 拉取并校验 Release 后经 SSH 推上 `gpu1` |
| 应急：本机交叉编译当前代码直推 | `./deploy/scripts/deploy-gpu1.sh local` | 用于紧急热修复验证 |
| 一键回滚到上一版本 | `./deploy/scripts/deploy-gpu1.sh rollback` | 或指定历史版本 `./deploy/scripts/deploy-gpu1.sh rollback v0.1.0` |
| 查看线上状态与已安装版本 | `./deploy/scripts/deploy-gpu1.sh status` / `list` | 查看软链、进程 PID、`/healthz` 与 `/admin/` 状态 |
| 同步最新部署脚本到 `gpu1` | `./deploy/scripts/deploy-gpu1.sh sync-scripts` | 更新服务器上的 `scripts/` 与 `mirror.env.example` |

也可以直接在 `gpu1` 本机执行：

```bash
/AIOS/sglang-kunlun-v0522-1017/deploy-dsv41-1017/gateway/gatehouse/scripts/pull-and-deploy.sh v0.1.0
```

---

## 6. 可选：开启定时自动拉取

默认情况下，`gpu1` **不会**自动拉取更新，需要你执行 `deploy-gpu1.sh pull` 才升级。  
如果希望 `gpu1` 每 2 分钟自动检查国内镜像 `:latest` 的 Digest（仅在发生变更时静默滚动升级，无变更不下载也不重启），可在 `gpu1` 执行：

```bash
systemctl enable --now dsv41-gatehouse-pull.timer
```

查看定时器与日志、或随时关闭：

```bash
# 查看状态与日志
systemctl status dsv41-gatehouse-pull.timer
journalctl -u dsv41-gatehouse-pull.service -n 50 --no-pager

# 恢复为手动触发
systemctl disable --now dsv41-gatehouse-pull.timer
```

---

## 7. 安全与回滚保障机制

1. **绝不覆盖线上配置与数据**：`apply-release.sh` 仅读写 `releases/`、`bin/gatehouse` 软链及 `.current_*`/`.previous_*` 状态标记；`config.yaml`、`data/`、`ROLLBACK.md`、`bin/gatehouse.bak.20261122_pre_tiered` 全程保持只读不动。
2. **绝不修改系统 DNS**：`gpu1` 的 `/etc/resolv.conf` 保持原样；`pull-from-registry.py` 仅在系统 DNS 解析失败时，于进程内部向 `223.5.5.5` / `119.29.29.29` 发起直连 UDP 解析。
3. **三道校验防线**：
   - 镜像层下载流式校验 OCI Blob SHA256；
   - 解压前校验 tar 路径安全性，解压后双校验 `SHA256SUMS` 与 `RELEASE_META`；
   - 切换软链前先用新二进制执行 `gatehouse version` 与 `gatehouse config check --config config.yaml`，任一失败直接中止，不触碰线上进程。
4. **自动秒级回滚**：重启 `dsv41-gatehouse.service` 后在 25 秒内持续校验 `systemd` 存活状态、`http://127.0.0.1:18011/healthz` 返回 `status: "ok"`、以及 `http://127.0.0.1:18002/admin/` 返回正常状态码；若未通过，打印最近 40 行日志并**自动切回上一个版本重新拉起服务**。
5. **并发互斥与基线保护**：使用 `flock` 防止同时触发两次发布；保留最近 5 个版本，且永远不清理当前版本、上一版本及 `baseline-*` 初始基线版本。
