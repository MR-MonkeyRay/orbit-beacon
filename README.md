# OrbitBeacon

自托管的多节点监控项目：Server 提供管理 API、内嵌 Web 控制台和 SQLite 存储，Agent 采集主机指标并通过 HTTPS 上报。当前适合开发体验和接入验证，**还不是具备实时看板、历史图表与告警的完整监控平台**。

## 当前能做什么

| 能力 | 当前状态 |
| --- | --- |
| 管理控制台 | 英文界面；登录/退出、节点清单与元数据、期望配置、用户/分组管理、修改自己的密码、审计记录 |
| Agent 接入 | 管理 API 创建短期接入凭证，Linux CLI 完成注册；支持单节点和分组批量接入 |
| 指标采集与上报 | 真实 CPU、负载、内存、Swap、运行时间、磁盘和网络数据；支持鉴权上报、重试和期望配置应用 |
| 备份与恢复 | Server CLI 已实现 SQLite 一致性备份和恢复；操作前必须停服，没有 Web 入口 |
| 部署材料 | Server 的 Dockerfile、Compose、systemd unit；需自行配置 HTTPS |
| 暂未提供 | Web 接入向导、指标展示、在线/离线判定、历史查询与流量汇总、探测任务、告警通知、公开状态页、Agent 自动/远程升级 |

注意：

- 注册后的节点当前显示 `unknown`，不是在线/离线结论；管理 API 的节点响应也不包含最新指标或 `last_seen`。
- 保存期望配置不等于 Agent 已应用。Agent 在上报交互中接收、校验并持久化配置；控制台暂不展示应用确认状态。
- 指标缺失以 `null` 和状态原因表示，不用零值伪装正常。断网时继续采集最新快照，但没有离线历史队列。Server 最新报告只保存在内存中，重启丢失，当前没有 HTTP 查询入口。
- 首次接入按 Linux 路径操作。非 Linux 的安全 token 文件接入未实现，Windows 运行身份校验也未实现；不要把跨平台采集代码理解为完整跨平台部署支持。
- CLI 备份产物只是单个 SQLite 数据库文件，不包含 `secrets.key`、配置或 TLS 文件，需另行保管。恢复不是自动修复工具，遇到无法安全处理的 SQLite 辅助文件会拒绝继续。

## 快速开始：本机 HTTPS 体验

需要 **Go 1.27.3**、Linux 和 OpenSSL。以下在仓库根目录、以普通用户执行，适用于新建的本机体验环境；不需要 Node.js 或前端构建。

### 1. 编译并创建首个管理员

```sh
umask 077
mkdir -p bin data
chmod 700 data
GOTOOLCHAIN=go1.27.3 go build -o bin/orbitbeacon-server ./cmd/orbitbeacon-server
GOTOOLCHAIN=go1.27.3 go build -o bin/orbitbeacon-agent ./cmd/orbitbeacon-agent
bin/orbitbeacon-server bootstrap --data-dir ./data --username admin
```

按提示输入密码（8–1024 个 UTF-8 字节，终端不回显）。没有默认密码或 HTTP 初始化入口；已有管理员时，`bootstrap` 不会覆盖它。自动化可从标准输入传入一行密码，不要放进命令参数。

### 2. 配置并启动 HTTPS

生成仅用于本机体验的短期自签名证书：

```sh
openssl req -x509 -newkey rsa:3072 -nodes -days 7 \
  -keyout data/server-key.pem -out data/server-cert.pem \
  -subj '/CN=localhost' \
  -addext 'subjectAltName=DNS:localhost' \
  -addext 'basicConstraints=critical,CA:TRUE'
cat > server.json <<'EOF'
{
  "listen_addr": "127.0.0.1:8443",
  "data_dir": "./data",
  "public_url": "https://localhost:8443",
  "tls_cert_file": "./data/server-cert.pem",
  "tls_key_file": "./data/server-key.pem",
  "max_nodes": 500
}
EOF
bin/orbitbeacon-server serve --config server.json
```

保持该进程运行，在另一个终端执行：

```sh
bin/orbitbeacon-server health --url https://localhost:8443 \
  --ca-file ./data/server-cert.pem
```

浏览器将证书加入受信任列表后访问 **https://localhost:8443**，用刚创建的管理员登录。此证书包含私钥的同一信任主体，仅用于本机演示，不要用于生产或分发私钥。浏览器会话 Cookie 始终为 `Secure`，不能改用普通 HTTP 完成登录。

### 3. 接入第一个 Agent

按 [Linux Agent 接入指南](docs/agent.md) 完成：API 登录 → 创建待接入节点 → 签发 15 分钟凭证 → 安全保存 token → 配置管理员所有的安装策略 → 以非 root 身份注册和运行。指南可直接衔接上面的本机 HTTPS 环境，也说明了远程节点的区别。

## 正式部署

见 [部署与备份说明](deploy/README.md)，包括 Server Docker/Compose、systemd、首次管理员初始化和停服备份恢复。

部署前必须明确以下边界：

- 使用可信 HTTPS 证书，或在客户端显式信任你的私有 CA；不要用跳过 TLS 校验代替信任配置。
- `public_url` 设置为实际浏览器访问的 HTTPS 地址，并保持 API 与页面同源。推荐部署在域名根路径，不要加子路径前缀。
- 可由反向代理终结 TLS，Server 监听回环 HTTP；也可同时配置 `tls_cert_file` 和 `tls_key_file` 直接提供 HTTPS。容器内监听地址需为 `0.0.0.0:8080`。
- 数据必须放在可写的本地文件系统，目录由运行用户所有且权限为 `0700`；数据库、SQLite 辅助文件和 `secrets.key` 应为同一用户所有的普通 `0600` 文件。路径不能经过符号链接；祖先目录也必须具有安全的所有者和权限。
- 同一数据目录仅允许一个 Server 管理命令持有。备份、恢复、重新 bootstrap 前先停服；遇到 `data_dir_in_use` 不要删除 `server.lock`，应停止占用进程。
- 当前不消费转发头。反向代理后登录限流按代理的直连 IP 共享，需在入口额外配置按客户端限流。

## 配置与接口

Server 配置优先级：**显式 CLI 参数 > `ORBITBEACON_` 环境变量 > JSON 文件 > 默认值**。未知配置项会报错。

| JSON 配置项 | 默认值 / 说明 |
| --- | --- |
| `listen_addr` | `127.0.0.1:8080` |
| `data_dir` | `./data`，持久化本地目录 |
| `public_url` | `https://localhost`，必须改为实际 HTTPS 地址 |
| `max_nodes` | `500`，包括待接入和禁用节点的容量上限，不是性能承诺 |
| `tls_cert_file` / `tls_key_file` | 默认空；启用内置 TLS 时必须同时设置 |
| `trusted_proxy_cidrs` | 默认空；当前不会据此启用转发头处理 |

例如 `--max-nodes 100`、`ORBITBEACON_MAX_NODES=100` 对应 JSON 的 `max_nodes`。Agent 的服务器地址、CA、目录和运行身份由受保护的安装策略固定，不能用普通配置或环境变量覆盖成不同值，详见 [接入指南](docs/agent.md)。

接口契约见 [OpenAPI](api/openapi.yaml)：

- 管理端：`/api/v1/`，使用会话 Cookie。写请求校验配置的 HTTPS Origin 和会话 CSRF token；登录需要 Origin，但不需要已有会话。
- Agent：`/api/agent/v1/`，使用接入 token 或节点 Bearer 凭证，不能与浏览器会话互换。
- 健康检查：`GET /healthz/live` 检查进程存活，`GET /healthz/ready` 检查服务是否接收流量及 SQLite 是否可用；均无需认证。
- API 错误包含 `code`、`message`、`request_id` 和可选 `fields`。排障时记录 `request_id`，不要发送 Cookie、密码或 token。

## 开发

主要入口是 [`cmd/`](cmd/)，Server 和 Agent 实现位于 [`internal/`](internal/)。Web 静态资源直接嵌入 Server 二进制。

常用检查命令（需自行执行）：

```sh
GOTOOLCHAIN=go1.27.3 go mod verify
GOTOOLCHAIN=go1.27.3 go test -count=1 ./...
GOTOOLCHAIN=go1.27.3 go test -race -count=1 ./internal/server/... ./internal/agent/...
GOTOOLCHAIN=go1.27.3 go vet ./...
```

完整质量门禁见 [CI 配置](.github/workflows/quality.yml)，还包括格式检查和固定版本的漏洞扫描。架构边界检查包含在 `go test -count=1 ./...` 中，不能依赖测试缓存。

## 许可证

本项目使用 **GNU GPL v3**，以仓库中的 [LICENSE](LICENSE) 为准。
