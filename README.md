# OrbitBeacon

自托管的多节点监控系统：Server 提供管理 API、内嵌 Web 界面和 SQLite 存储，Linux Agent 以非 root 身份采集并通过 HTTPS 上报。

管理控制台为英文，公开节点页为中文。控制台面向桌面和平板（最小宽度 744px），公开页支持手机。

[部署与备份](deploy/README.md) · [Agent 接入](docs/agent.md) · [架构与数据边界](docs/architecture.md) · [HTTP API](api/openapi.yaml) · [Web 开发](web/README.md)

## 能做什么

| 功能 | 使用入口与边界 |
| --- | --- |
| 节点监控 | Nodes：查看 CPU、内存、磁盘、网络、负载、指标历史和服务探测；Agent settings 只读显示 Agent 本地运行参数 |
| 流量统计 | 节点 Details → Traffic billing：设置账期和配额；仅统计，不限速，也不等同于云厂商账单 |
| 节点接入 | 创建节点后在 Enroll agent 签发一次性凭证；支持分组批量接入 |
| 公开页面 | 节点 Public page：独立填写公开名称、地区和服务商，默认不公开 |
| 告警 | Alerts：规则、事件及 Webhook / Telegram / 邮件通道；未绑定通道仍产生站内事件与通知 |
| 管理员 | Settings → Administrators：新增、更名、删除等权管理员，至少保留一名；Account 修改本人密码并结束本人全部会话 |
| 导出与备份 | Settings → Data operations：加密导出、本地或对象存储备份计划、任务记录；恢复须停服后通过 CLI 执行 |
| 站内通知 | Notifications：告警与备份结果，未读状态按管理员独立保存；备份结果不外发 |

需要了解的限制：

- 在线表示 Server 在最近 180 秒内收到报告，不代表业务健康。失联保留最后样本并标旧，数字和 uptime 不自行增长；完整样本只在内存中，Server 重启后须等待新报告。指标历史为尽力写入。
- 公开页和节点管理页自动按 WebSocket → SSE → HTTP 回退。浏览器断网只标旧，不自行判定节点离线；节点管理页实时更新只读数据，不覆盖未保存表单。
- 告警每 30 秒评估，外发通知可能延迟、重复或丢失，不替代独立的高可靠告警系统。
- 所有控制台账户均为管理员；没有他人密码重置或找回流程。Agent 没有远程升级、终端或任意命令执行功能；当前部署支持 Linux。

## 快速开始：本机 HTTPS

需要 Linux、Go（版本见 [go.mod](go.mod)）和 OpenSSL，不需要 Node.js。以下在仓库根目录以普通用户执行，使用新的数据目录。数据目录及祖先须归 root 或当前用户所有，且不可被组或其他用户写入；不要直接使用权限不符合要求的仓库目录。

### 1. 编译并创建管理员

```sh
umask 077
mkdir -p build "$HOME/orbitbeacon-demo"
chmod 700 "$HOME/orbitbeacon-demo"
go build -o build/orbitbeacon-server ./cmd/orbitbeacon-server
go build -o build/orbitbeacon-agent ./cmd/orbitbeacon-agent
build/orbitbeacon-server bootstrap --data-dir "$HOME/orbitbeacon-demo" --username admin
```

按提示输入密码（8–1024 个 UTF-8 字节，终端不回显）。没有默认密码或网页初始化入口；已有管理员不会被覆盖。自动化可从标准输入传入一行密码，不要放进命令参数。

### 2. 启动 HTTPS

```sh
DEMO_DIR="$HOME/orbitbeacon-demo"
openssl req -x509 -newkey rsa:3072 -nodes -days 7 \
  -keyout "$DEMO_DIR/server-key.pem" -out "$DEMO_DIR/server-cert.pem" \
  -subj '/CN=localhost' \
  -addext 'subjectAltName=DNS:localhost' \
  -addext 'basicConstraints=critical,CA:TRUE'
cat > "$DEMO_DIR/server.json" <<EOF
{
  "listen_addr": "127.0.0.1:8443",
  "data_dir": "$DEMO_DIR",
  "public_url": "https://localhost:8443",
  "tls_cert_file": "$DEMO_DIR/server-cert.pem",
  "tls_key_file": "$DEMO_DIR/server-key.pem"
}
EOF
build/orbitbeacon-server serve --config "$DEMO_DIR/server.json"
```

另开终端检查：

```sh
build/orbitbeacon-server health --url https://localhost:8443 \
  --ca-file "$HOME/orbitbeacon-demo/server-cert.pem"
```

浏览器信任 `server-cert.pem` 后访问 <https://localhost:8443>，点“管理登录”。此证书仅用于本机演示，不要分发私钥。登录 Cookie 固定为 `Secure`，不能改用 HTTP 登录。

### 3. 接入并选择公开节点

1. 在 Nodes 创建节点，打开 Enroll agent 签发凭证，按 [Agent 指南](docs/agent.md) 安装。
2. 确认最近上报时间和最新样本；注册成功不等于已上报。
3. 如需公开，在 Public page 填写公开信息并勾选 Publish this node。仅已注册、启用且有有效凭据的公开节点展示。

公开文本不会自动脱敏，不要填入 IP、主机名或秘密。CPU、内存、负载和 uptime 也会公开。取消公开、禁用、删除或撤销凭据会从后续快照移除节点，但无法召回已发送或断网保留的数据。`/status` 不提供页面。

## 正式部署与配置

按[部署指南](deploy/README.md)选择 systemd 或 Docker Compose，配置可信 HTTPS 和备份。页面与 API 同源，不支持子路径前缀。

**当前主库 schema 为 19，不提供历史迁移或跨 schema 恢复。** 旧库（含 schema 18）会被拒绝；使用新的私有数据目录，保留旧目录供原版本使用。

Server 配置优先级：**CLI > `ORBITBEACON_` 环境变量 > JSON > 默认值**；未知 JSON 键报错。

| JSON 键 | 默认值 / 说明 |
| --- | --- |
| `listen_addr` | `127.0.0.1:8080` |
| `data_dir` | `./data`；本地持久化目录，所有权和权限要求见部署指南 |
| `public_url` | `https://localhost`；浏览器实际访问的 HTTPS 源站，用于 Origin 校验 |
| `max_nodes` | `500`；包括待接入和禁用节点，不是性能承诺 |
| `tls_cert_file` / `tls_key_file` | 空；Server 直接终结 TLS 时成对设置 |
| `trusted_proxies` | `[]`；仅信任指定 IP / CIDR 的转发客户端地址 |
| `web_update_interval_seconds` | `10`；整数 2–60 秒，控制网页推送合并与 HTTP 轮询，重启生效，不改变 Agent 采集周期 |
| `metrics_listen` | 空，关闭独立 `/metrics` 监听；开启时须限制访问 |
| `access_log` | `false` |
| `allow_insecure_webhooks` / `notification_allow_private_networks` | `false`；出站安全限制见部署指南 |

环境变量如 `ORBITBEACON_MAX_NODES=100`，CLI 如 `--max-nodes 100`。Agent 的五项运行参数在 Agent 主机上配置并 reload，不通过网页下发。

## API 与排障

完整字段、端点和错误码见 [OpenAPI](api/openapi.yaml)。

- 管理：`/api/v1/`，使用会话 Cookie，写请求校验 Origin 和 CSRF token。
- Agent：`/api/agent/v1/`，使用接入或节点凭证，不能与管理会话互换。
- 公开：`/api/public/v1/nodes`、`/stream`、`/ws`，匿名白名单数据。
- 健康检查：`/healthz/live` 检查进程；`/healthz/ready` 检查接流状态及 SQLite；均无需认证，不证明 Agent 已上报。

排障记录错误响应的 `code` 和 `request_id`，不要发送 Cookie、密码或 token。遇到 `data_dir_in_use` 先查占用进程，不要删除 `server.lock`。

## 开发与发布

入口为 [`cmd/`](cmd/)，实现为 [`internal/`](internal/)。Web 产物嵌入 Server，修改界面见 [Web 开发](web/README.md)。

```sh
go mod verify
go test -count=1 ./...
go test -race -count=1 ./internal/server/... ./internal/agent/...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
```

[CI](.github/workflows/ci.yml) 检查 Go、Web 产物与测试、漏洞和 Agent 安装脚本。`main` push 的 CI 全部通过后调用 [Release](.github/workflows/release.yml)，从 [`VERSION`](VERSION) 发布 Linux amd64/arm64 Server、Agent 包与镜像；发布配置不代表对应产物已可用。

Go 版本以 `go.mod` 为准，Web 构建 Node.js 版本以 `web/.nvmrc` 为准。JavaScript Actions 独立使用 Node.js 24 运行时并固定完整 commit SHA；修改前端 `node-version` 不会升级 Action 运行时。

## 许可证

[GNU GPL v3](LICENSE)。
