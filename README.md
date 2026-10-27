# OrbitBeacon

自托管的多节点监控系统。Server 提供管理 API、内嵌 Web 控制台和 SQLite 存储；Agent 采集主机指标，通过 HTTPS 上报。

当前可用的是节点接入、鉴权上报、期望配置下发和备份恢复。控制台仍以管理功能为主，**没有指标图表、在线判定、告警和公开状态页**，适合自用或小团队做接入验证。

文档：[部署](deploy/README.md) · [Linux Agent 接入](docs/agent.md) · [HTTP API](api/openapi.yaml)

## 功能

| 模块 | 状态 |
| --- | --- |
| 控制台 | 登录/退出、节点清单与元数据、期望配置、用户/组、修改自己的密码、审计。界面为英文。 |
| 接入 | 管理 API 签发接入凭证，Linux Agent CLI 注册；支持单节点和分组批量接入。 |
| 上报 | CPU、负载、内存/Swap、运行时间、磁盘和网络；Bearer 鉴权、退避重试、应用期望配置。 |
| 运维 | 健康检查、SQLite 备份/恢复、systemd、Docker Compose。 |
| 未实现 | 浏览器接入向导、指标展示、在线/离线、历史与流量汇总、探测任务、告警、公开状态页、Agent 自动升级。 |

上线前需要知道：

- 注册成功的节点状态仍是 `unknown`，管理 API 也不返回 `last_seen` 或最新指标。它不是在线/离线判定。
- 保存期望配置只代表服务端已记录。Agent 会在后续上报中接收并保存配置，但控制台不展示应用确认。
- 无法采集的指标上报为 `null`，不会用 0 伪装。Server 把最新指标报告正文放在内存中，重启即丢，也还没有查询接口；断网期间不积压历史样本。
- 当前按 Linux 部署。Windows 运行身份校验未实现；其他平台的采集代码不等于已支持的部署方式。
- `backup` 只生成 SQLite 数据库文件，不包含 `secrets.key`、配置或 TLS 证书；`restore` 也不是损坏数据库的通用修复工具。

## 快速开始：本机 HTTPS 体验

需要 Linux、Go 1.27.3 和 OpenSSL，普通用户执行即可，不需要 Node.js。以下命令在仓库根目录运行，适用于新建的本机数据目录。数据目录及其祖先必须由 root 或当前用户所有，且不能对组/其他用户可写，所以示例把它放在 `$HOME/orbitbeacon-demo`，不要直接使用仓库内的 `./data`。

### 1. 编译并创建管理员

```sh
umask 077
mkdir -p bin "$HOME/orbitbeacon-demo"
chmod 700 "$HOME/orbitbeacon-demo"
GOTOOLCHAIN=go1.27.3 go build -o bin/orbitbeacon-server ./cmd/orbitbeacon-server
GOTOOLCHAIN=go1.27.3 go build -o bin/orbitbeacon-agent ./cmd/orbitbeacon-agent
bin/orbitbeacon-server bootstrap --data-dir "$HOME/orbitbeacon-demo" --username admin
```

按提示输入密码，长度 8–1024 个 UTF-8 字节，终端不回显。没有默认密码或 HTTP 初始化入口；已有管理员时不会覆盖。自动化场景可从标准输入传入一行密码，但不要放进命令参数。

### 2. 配置证书并启动

```sh
DEMO_DIR="$HOME/orbitbeacon-demo"
openssl req -x509 -newkey rsa:3072 -nodes -days 7 \
  -keyout "$DEMO_DIR/server-key.pem" -out "$DEMO_DIR/server-cert.pem" \
  -subj '/CN=localhost' \
  -addext 'subjectAltName=DNS:localhost' \
  -addext 'basicConstraints=critical,CA:TRUE'
cat > server.json <<EOF
{
  "listen_addr": "127.0.0.1:8443",
  "data_dir": "$DEMO_DIR",
  "public_url": "https://localhost:8443",
  "tls_cert_file": "$DEMO_DIR/server-cert.pem",
  "tls_key_file": "$DEMO_DIR/server-key.pem",
  "max_nodes": 500
}
EOF
bin/orbitbeacon-server serve --config server.json
```

保持服务运行，在另一个终端检查（如果新开了终端，先重新设置 `DEMO_DIR`）：

```sh
DEMO_DIR="$HOME/orbitbeacon-demo"
bin/orbitbeacon-server health --url https://localhost:8443 \
  --ca-file "$DEMO_DIR/server-cert.pem"
```

浏览器信任 `$DEMO_DIR/server-cert.pem` 后访问 <https://localhost:8443>，用刚创建的账号登录。这张证书仅用于本机演示，不要分发 `server-key.pem`。会话 Cookie 固定为 `Secure`，不能改用 HTTP 登录。

### 3. 接入 Agent

按 [Linux Agent 接入指南](docs/agent.md) 完成：登录管理 API、创建节点、签发凭证、安装固定的安装策略，再以非 root 身份注册并运行。该指南也说明了远程节点与同机体验的区别。

## 正式部署

见 [部署与备份说明](deploy/README.md)，包含 systemd、Docker Compose、首次管理员初始化和停服备份恢复。

部署前至少确认：

- 使用可信 HTTPS 证书，或让客户端显式信任你的私有 CA；不要用跳过 TLS 校验代替信任配置。
- `public_url` 是浏览器实际访问的 HTTPS 源站，推荐域名根路径。API 与页面同源，不支持子路径前缀。
- 数据目录放在本地文件系统，由运行用户单独所有并设为 `0700`；数据库、SQLite 辅助文件和 `secrets.key` 必须是同一用户的 `0600` 普通文件，路径不能经过符号链接。
- 同一数据目录只能有一个 Server 命令持有。备份、恢复或重新 `bootstrap` 前先停服；遇到 `data_dir_in_use` 不要删除 `server.lock`。
- 服务默认只监听回环地址，由反向代理终结 TLS。当前不消费转发头，代理后的登录限流会按代理的直连 IP 计算，需在入口另做按客户端限流。

## 配置

Server 配置优先级：**CLI 参数 > `ORBITBEACON_` 环境变量 > JSON 文件 > 默认值**。未知配置键会直接报错。

| JSON 配置项 | 默认值 / 说明 |
| --- | --- |
| `listen_addr` | `127.0.0.1:8080` |
| `data_dir` | `./data`，本地持久化目录 |
| `public_url` | `https://localhost`；必须是实际 HTTPS 地址，用于浏览器 Origin 校验 |
| `max_nodes` | `500`；包括待接入和禁用节点，是容量上限，不是性能承诺 |
| `tls_cert_file` / `tls_key_file` | 默认空；直接提供 HTTPS 时必须成对设置 |
| `trusted_proxy_cidrs` | 默认空；仅为预留配置，当前不读取转发头 |

环境变量使用同名的 `ORBITBEACON_` 前缀，例如 `ORBITBEACON_MAX_NODES=100`；对应 CLI 参数为 `--max-nodes 100`。Agent 的服务器地址、CA、目录和运行身份由安装策略固定，不能用普通配置或环境变量覆盖，详见[接入指南](docs/agent.md)。

## API 与排障

接口契约见 [OpenAPI](api/openapi.yaml)。

- 管理端：`/api/v1/`，使用会话 Cookie。写请求校验 `Origin` 和会话 CSRF token；登录需要 `Origin`，但不需要已有会话。
- Agent 端：`/api/agent/v1/`，使用接入凭证或节点 Bearer 凭证，不能与浏览器会话互换。
- 健康检查：`GET /healthz/live` 检查进程，`GET /healthz/ready` 检查是否接收流量及 SQLite 是否可用；两者都不需要认证。
- 错误响应包含 `code`、`message`、`request_id` 和可选 `fields`。排障时记录 `request_id`，不要发送 Cookie、密码或 token。

## 开发

主要入口在 [`cmd/`](cmd/)，Server 和 Agent 实现在 [`internal/`](internal/)，Web 静态资源直接嵌入 Server 二进制。

```sh
GOTOOLCHAIN=go1.27.3 go mod verify
GOTOOLCHAIN=go1.27.3 go test -count=1 ./...
GOTOOLCHAIN=go1.27.3 go test -race -count=1 ./internal/server/... ./internal/agent/...
GOTOOLCHAIN=go1.27.3 go vet ./...
```

完整门禁见 [CI 配置](.github/workflows/quality.yml)，还包括格式检查和固定版本的漏洞扫描。架构边界检查包含在 `go test -count=1 ./...` 中，不能依赖测试缓存。

## 许可证

**GNU GPL v3**，以仓库中的 [LICENSE](LICENSE) 为准。