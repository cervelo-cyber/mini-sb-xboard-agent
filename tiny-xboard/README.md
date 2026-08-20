# tiny-xboard

Tiny Xboard UniProxy API 服务器（模拟 Xboard 面板的节点接口），专为
[mini-sb-agent](https://github.com/ashvvvvv/mini-sb-agent) / sing-box 节点联调设计。

纯 Go 标准库实现，无任何第三方依赖，无 CGO。可在 64MB / 128MB 的 NAT 容器内长期运行，
不易被 OOM killer 杀掉。

**分发模型：开发机负责编译，GitHub 仓库以普通文件分发二进制，目标服务器只负责下载 + 校验 + 运行。**
不依赖 GitHub Releases，不在目标服务器上编译，不需要 Go/Git/GCC/make。

## 特性

- **三个 UniProxy 接口**：`/user`、`/config`、`/push`，协议/字段与 Xboard 面板一致
- **单实例多节点**：一台管理机托管多台 NAT 容器，各节点共用通讯密钥、靠 `node_id` 区分
- **JSON 文件持久化**：`nodes.json`（或单节点 `node.json`）/ `users.json` / `traffic.json`，无需数据库
- **原子写入**：`tmp → fsync → .bak → rename → fsync(dir)`，崩溃不丢数据；主文件损坏时自动回滚 `.bak`
- **版本化安装**：`/opt/tiny-xboard/bin/tiny-xboard-<commit>` + `current` 软链，升级失败自动回滚
- **轻量**：单二进制 ~6MB（strip），运行时峰值内存实测 < 6MiB
- **省内存**：`GOMEMLIMIT=32MiB` + `GOGC=70` + `GOMAXPROCS=1`
- **安全**：token 鉴权 + node_id/node_type 校验，文件权限 0600，目录 0700
- **优雅退出**：SIGTERM/SIGINT 时落盘流量再退出

## 接口

全部接口需带 query 参数 `token`、`node_id`、`node_type`。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/v1/server/UniProxy/user` | 返回用户列表 `{"users":[...]}` |
| GET | `/api/v1/server/UniProxy/config` | 返回节点配置（VLESS Reality / Hysteria2） |
| POST | `/api/v1/server/UniProxy/push` | 上报流量，返回 `{"success":true,"accepted":N}` |

错误码：token 错误返回 `401`，node_id/node_type 不匹配返回 `404`。

`/push` 同时支持两种报文：

```jsonc
// Xboard / sing-box 真实格式
{"traffic": [{"uid": 1, "up": 100, "down": 100, "created_at": 1712000000}]}

// 紧凑格式（curl 调试方便）
{"1": [100, 100]}
```

未知 uid 会被忽略（不计入 accepted），不影响整包提交。请求体上限 256KB，并发读取有信号量上限（2MB 在途），超限返回 413/503，防止大包撑爆内存。

## 安装

服务器只需要：`curl` 或 `wget`、`sha256sum` 或 `openssl`、Linux（amd64 / arm64）。
不需要 Go / Git / GCC / make / SQLite / MySQL / Redis / GitHub Release。

### Install

```sh
curl -fsSL https://raw.githubusercontent.com/ashvvvvv/mini-sb-agent/master/tiny-xboard/install.sh | sh
```

非交互安装（VLESS 节点）：

```sh
sh install.sh --token '你的token' --node-id 1 --node-type vless --node-port 443 --yes
```

Hysteria2 节点：

```sh
sh install.sh --token '你的token' --node-id 2 --node-type hy2 --node-port 44311 --yes
```

常用参数：

| 参数 | 说明 |
| --- | --- |
| `--ref REF` | Git ref（分支名或 commit），默认 `master`；二进制从该 ref 下的仓库文件下载 |
| `--listen ADDR` | API 监听地址，默认 `127.0.0.1:8080` |
| `--data-dir PATH` | 状态目录，默认 `/etc/tiny-xboard` |
| `--reality-*` | VLESS Reality 参数 |
| `--gomemlimit` / `--gogc` / `--gomaxprocs` | 内存与调度调优，默认 32MiB / 70 / 1 |
| `--force` | 强制重新下载并校验二进制（**不会**删除任何状态文件） |
| `--enable` | 启用开机自启（systemd enable / rc-update add） |
| `--no-start` | 只安装不启动 |
| `--add-node` | 追加节点到 nodes.json（多节点部署用） |
| 环境变量 `TINY_XBOARD_BASE_URL` | 覆盖二进制下载地址；`TINY_XBOARD_BINARY` 指定本地二进制 |

安装流程：下载 → SHA256 校验（对照仓库 `checksums/SHA256SUMS`）→ ELF/架构检查 →
`--version` 执行检查 → 安装版本化二进制 → 切换 `current` 软链 → 启动 → `/user` + `/config` 健康检查。
任何一步失败自动回滚到旧版本并恢复旧服务；状态目录 `/etc/tiny-xboard` 永不被触碰。

安装后测试：

```sh
curl "http://127.0.0.1:8080/api/v1/server/UniProxy/config?token=你的token&node_id=1&node_type=vless"
```

安装脚本自动识别 systemd / OpenRC 创建服务；容器内无 init 时直接前台运行。

### Upgrade

```sh
sh install.sh            # 重新下载最新 master 二进制并升级
sh install.sh --ref <commit>
```

升级只切换 `current` 软链，保留 `/etc/tiny-xboard/`（node.json / users.json / traffic.json / Reality key）和运行时参数。

### Build binaries

```sh
./build.sh            # 构建 amd64 + arm64，写入 checksums/SHA256SUMS
./build.sh amd64      # 只构建 amd64
./build.sh arm64      # 只构建 arm64
./build.sh all        # 同默认
```

产出：

```
bin/tiny-xboard-linux-amd64
bin/tiny-xboard-linux-arm64
checksums/SHA256SUMS
```

构建参数：`CGO_ENABLED=0`、`-trimpath`、`-buildvcs=false`、`-ldflags "-s -w"`，并注入
`version` / `commit`（`git rev-parse --short HEAD`）/ `buildTime`。可用环境变量 `VERSION` / `COMMIT` / `BUILD_TIME` 覆盖，无 Git 时自动回退到 `dev` / `unknown` / 当前时间，不会失败。

### Supported

- Linux amd64
- Linux arm64
- Alpine 3.21 / Debian 12 / Ubuntu 24.04（及其他 Linux，只要二进制可运行）
- systemd / OpenRC（容器内无 init 时直接前台运行）

### Runtime

- 64MB / 128MB 友好：`GOMEMLIMIT=32MiB` + `GOGC=70` + `GOMAXPROCS=1`
- 空载 ~2MiB，600 并发 push 后 ~6MiB

### Data

- 状态目录：`/etc/tiny-xboard/`（node.json / users.json / traffic.json，权限 0600/0700）
- 二进制目录：`/opt/tiny-xboard/bin/`（版本化二进制 + `current` 软链）
- 升级/回滚/卸载均不触碰 `/etc/tiny-xboard/`

### Uninstall

```sh
sh /opt/tiny-xboard/uninstall.sh     # 或仓库 uninstall.sh
```

默认删除 binary 与服务，保留 `/etc/tiny-xboard/` 状态数据。

### Purge

```sh
sh uninstall.sh --purge
```

同时删除状态数据（含 Reality key），需要二次确认。

## 数据文件（默认 /etc/tiny-xboard）

- `nodes.json`：多节点配置（`token` + `nodes[]`），存在时优先于 `node.json`
- `node.json`：单节点配置（`auth.token`、`node.id`、`node.type`、`server.port` 等），多节点模式下不生成
- `users.json`：用户列表，`enabled` 为 false 的用户在 `/user` 中不会返回
- `traffic.json`：累计流量 `{"users":{"<id>":{"upload":N,"download":N}}}`，每 `traffic_flush_interval` 秒落盘一次

文件均 0600、目录 0700。编辑文件后**无需重启**，接口即时生效（节点配置需重启生效）。
VLESS Reality 私钥/公钥在首次启动时自动生成并持久化，重新安装/升级/回滚不会改变。

## 多节点部署

一台管理机（VPS 或长期 NAT 容器）运行 tiny-xboard，多台 NAT 容器各自跑
mini-sb-agent 接入：**所有节点共用同一个 token（通讯密钥），仅 `node_id` 不同**。
这与 mini-sb-agent 的 `--panel-token` 用法一致。

1. 首次安装（管理机，默认节点 1）：
   ```sh
   sh install.sh --token '通讯密钥' --node-id 1 --node-type vless --node-port 443 --yes
   ```
2. 追加其余 NAT 容器节点（同一台管理机上重复执行）：
   ```sh
   sh install.sh --add-node --node-id 5 --node-type hy2 --node-port 44311 --yes
   sh install.sh --add-node --node-id 6 --node-type vless --node-port 2053 --node-name nat-extra --yes
   ```
   - 首次 `--add-node` 会把现有 `node.json` 迁移为 `nodes.json`（保留原 token，旧节点不受影响）
   - 添加节点后重启服务：`systemctl restart tiny-xboard`（或 `rc-service tiny-xboard restart`）
3. 各 NAT 容器上的 mini-sb-agent 用同一个 token 接入，分别指定自己的 `--node-id`：
   ```sh
   ./mini-sb-agent --panel-token '通讯密钥' --node-id 5 --node-type hy2 ...
   ```

## 容器部署（64MB podman/docker）

先在本机执行 `./build.sh` 生成 `bin/`，再构建镜像（Dockerfile 直接复制预编译二进制，不编译）：

```sh
docker build -t tiny-xboard .
docker run -d --name tiny-xboard --memory=64m \
  -p 127.0.0.1:8080:8080 \
  -v tiny-xboard-data:/etc/tiny-xboard \
  tiny-xboard
```

arm64 镜像：`docker build --build-arg TARGETARCH=arm64 -t tiny-xboard .`

- 容器内默认监听 `0.0.0.0:8080`（否则端口映射不通）；用 `--network host` 时改用 `127.0.0.1`
- 首次启动自动生成 `node.json` 与随机 token：`docker exec tiny-xboard cat /etc/tiny-xboard/node.json`
- 镜像运行层仅 alpine + ca-certificates，约 10MB

## 开发者发布流程

1. 修改源码
2. 运行测试：`go test ./...`
3. `./build.sh` → 生成 `bin/tiny-xboard-linux-{amd64,arm64}` + `checksums/SHA256SUMS`
4. `git diff` 检查源码与 binary
5. `git commit` + `git push`

构建脚本自带发布前检查：两个二进制存在且可执行、ELF/架构正确、`--version` 正常、SHA256SUMS 自检通过，任一失败返回非 0。

**注意：** 本项目将二进制作为普通文件提交到 Git 仓库（不依赖 GitHub Release，不使用 Git LFS）。
每次更新 binary 会增大仓库历史；如果未来成为问题，再单独评估替代方案。

## 常见问题

- **token 忘记？** 查看 `/etc/tiny-xboard/node.json` 的 `auth.token`（多节点时是 `nodes.json` 的 `token`），改动即时生效。
- **流量没落盘？** 默认 `traffic_flush_interval=300` 秒刷一次；SIGTERM/SIGINT 也会立即落盘。
- **接口返回 401？** token 不匹配；`/push` 需使用 POST 方法。
- **升级失败？** 安装器自动回滚到旧版本；旧版本文件保留在 `/opt/tiny-xboard/bin/`，可手动 `ln -sfn` 切换。
- **安装器无法下载？** 检查网络，或设置 `TINY_XBOARD_BASE_URL` 指向镜像源；不会在服务器上编译。
- **Reality key 会变吗？** 不会。安装/升级/回滚都不生成新 key；只有首次初始化且无 key 时才生成。