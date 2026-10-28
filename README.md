# OrbitBeacon

自托管的多节点监控系统。Server 提供管理 API、内嵌 Web 控制台和 SQLite 存储；Agent 采集主机指标，通过 HTTPS 上报。

管理控制台为英文，公开节点首页为中文。Agent 当前按 Linux 部署；Windows 和 macOS 的采集代码不代表已支持部署。

文档：[部署与备份](deploy/README.md) · [Linux Agent 接入](docs/agent.md) · [架构与数据边界](docs/architecture.md) · [HTTP API](api/openapi.yaml) · [Web 开发](web/README.md)

## 功能

- 管理节点、分组、管理员和接入凭证；只读查看 Agent 本地运行设置，管理流量账期/配额及指标、历史与探测结果。
- 配置告警规则，查看告警事件，通过 Webhook、Telegram 或邮件发送通知。
- 为每个节点独立设置公开名称、地区和服务商，选择是否在公开首页展示；默认不公开。
- 在 Settings → Data operations 导出加密归档、管理本地/对象存储备份计划，支持离线按类别恢复；提供 systemd 和 Docker Compose 部署。Agent 自动升级尚未实现。

在线状态按最近上报时间判断，不等于 ping、业务健康或可用率。资源缺失或过期显示 `—`；公开资源快照在内存中，Server 重启后需等待新上报。历史指标是尽力记录，不应作为完整审计或计费数据。

遥测报告忽略并丢弃新增未知字段，已知指标和来源仍校验；注册、恢复、管理写入及本地配置严格拒绝未知字段。请求上限为 64 KiB，不改变协议版本校验。

配置、监控、事件可独立选择，默认仅配置；配置包包含匹配的 `secrets.key`，不含部署配置、TLS 证书或程序。每个 `.tar.gz.age` 包使用独立的 16 位随机密码，须另行保管；密码和任务保留至完成后 90 天，手动下载有效 24 小时。步骤与恢复边界见[部署文档](deploy/README.md)。

### 使用控制台

- **Nodes**：创建节点，在 **Enroll agent** 签发凭证。**Agent settings** 只读显示最近报告的本地设置；修改 Agent JSON 后 reload。**Details → Traffic billing** 单独保存 Server 账期与配额，不影响 Agent 采集周期。
- **Alerts**：在 **Channels** 配置并测试通知通道，在 **Rules** 添加规则并绑定通道；在 **Active** 和 **History** 查看事件。规则可覆盖全部节点、分组或单节点，支持 CPU、内存、Swap、磁盘、负载、探测失败和离线。未绑定通道时仍可产生事件和站内通知，但不会外发。
- **Settings → Administrators**：新增、更名和删除管理员；所有账户权限相同，至少保留一名管理员。存在其他管理员时可删除本人并立即退出。**Account** 仅支持验证旧密码后修改本人密码，成功后结束本人全部会话；不能重置他人密码。
- **Settings → Data operations**：发起后台导出、管理备份计划、查看任务详情；完成后自行下载，不会自动下载。备份结果只进站内通知，不外发。
- **Notifications** 铃铛：所有管理员查看备份结果和告警；未读数与已读状态按账户独立保存，不是当前触发告警数。

告警每 30 秒评估一次；外发通知可能延迟或丢失，不能替代独立的高可靠告警系统。未绑定外发通道仍会生成站内告警通知。更多数据保留和可靠性边界见[架构说明](docs/architecture.md)。

## 快速开始：本机 HTTPS 体验

需要 Linux、Go 1.27.3 和 OpenSSL，普通用户执行即可，不需要 Node.js。以下命令在仓库根目录运行，适用于新建的本机数据目录。数据目录及其祖先必须由 root 或当前用户所有，且不能对组/其他用户可写，所以示例把它放在 `$HOME/orbitbeacon-demo`，不要直接使用仓库内的 `./data`。

### 1. 编译并创建管理员

```sh
umask 077
mkdir -p build "$HOME/orbitbeacon-demo"
chmod 700 "$HOME/orbitbeacon-demo"
GOTOOLCHAIN=go1.27.3 go build -o build/orbitbeacon-server ./cmd/orbitbeacon-server
GOTOOLCHAIN=go1.27.3 go build -o build/orbitbeacon-agent ./cmd/orbitbeacon-agent
build/orbitbeacon-server bootstrap --data-dir "$HOME/orbitbeacon-demo" --username admin
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
cat > "$DEMO_DIR/server.json" <<EOF
{
  "listen_addr": "127.0.0.1:8443",
  "data_dir": "$DEMO_DIR",
  "public_url": "https://localhost:8443",
  "tls_cert_file": "$DEMO_DIR/server-cert.pem",
  "tls_key_file": "$DEMO_DIR/server-key.pem",
  "max_nodes": 500
}
EOF
build/orbitbeacon-server serve --config "$DEMO_DIR/server.json"
```

保持服务运行，在另一个终端检查（如果新开了终端，先重新设置 `DEMO_DIR`）：

```sh
DEMO_DIR="$HOME/orbitbeacon-demo"
build/orbitbeacon-server health --url https://localhost:8443 \
  --ca-file "$DEMO_DIR/server-cert.pem"
```

浏览器信任 `$DEMO_DIR/server-cert.pem` 后访问 <https://localhost:8443> 查看公开首页，点“管理登录”使用刚创建的账号。这张证书仅用于本机演示，不要分发 `server-key.pem`。会话 Cookie 固定为 `Secure`，不能改用 HTTP 登录。

### 3. 接入 Agent

在控制台 Nodes 中创建节点，在节点详情的 Enroll agent 签发凭证；然后按 [Linux Agent 接入指南](docs/agent.md) 安装并运行。指南也说明私有 CA 和远程节点的配置。

### 4. 选择公开节点

管理员进入 Nodes → 节点详情 → Public page，填写独立的 Public name、Region、Provider，勾选 Publish this node 后保存。不会自动复制内部元数据；已注册且启用的节点才会展示。不要填入 IP、域名、主机名或凭证；这些文本是主动发布的内容，不会自动脱敏。CPU、内存、负载和运行时长也会公开。

取消公开、禁用、删除节点或撤销节点凭证后，下次请求将其排除；已发送的数据无法撤回。首页优先使用事件流，建立连接失败时回退到轮询。`/status` 不提供页面。

## 正式部署

见 [部署与备份说明](deploy/README.md)，包含 systemd、Docker Compose、管理员初始化和备份恢复。

部署前至少确认：

- 使用可信 HTTPS 证书，或让客户端显式信任你的私有 CA；不要用跳过 TLS 校验代替信任配置。
- `public_url` 是浏览器实际访问的 HTTPS 源站，推荐域名根路径。API 与页面同源，不支持子路径前缀。
- 数据目录放在本地文件系统，由运行用户单独所有并设为 `0700`；数据库、SQLite 辅助文件和 `secrets.key` 必须是同一用户的 `0600` 普通文件，路径不能经过符号链接。
- 主库使用 schema 19：空库直接建表，不执行历史迁移；旧库（含 schema 18）及旧 schema 归档均拒绝，不清空或转换。使用新的私有数据目录，保留旧目录供原版本使用。
- 导出和计划备份由运行中的 Server 执行；恢复或重新 `bootstrap` 前先停服。遇到 `data_dir_in_use` 不要删除 `server.lock`。
- 默认不信任转发头，代理后的登录限流按代理直连 IP 计算。仅在受控代理下配置 `trusted_proxies`，并确保代理正确设置 `X-Forwarded-For`；详见部署文档。

## 配置

Server 配置优先级：**CLI 参数 > `ORBITBEACON_` 环境变量 > JSON 文件 > 默认值**。未知配置键会直接报错。

| JSON 配置项 | 默认值 / 说明 |
| --- | --- |
| `listen_addr` | `127.0.0.1:8080` |
| `data_dir` | `./data`，本地持久化目录 |
| `public_url` | `https://localhost`；必须是实际 HTTPS 地址，用于浏览器 Origin 校验 |
| `max_nodes` | `500`；包括待接入和禁用节点，是容量上限，不是性能承诺 |
| `tls_cert_file` / `tls_key_file` | 默认空；直接提供 HTTPS 时必须成对设置 |
| `trusted_proxies` | 默认空数组；仅信任指定代理 IP 或 CIDR 的 `X-Forwarded-For` |
| `metrics_listen` | 默认空，即关闭独立指标监听；开启时应限制访问 |
| `access_log` | 默认 `false`，是否启用访问日志 |
| `allow_insecure_webhooks` / `notification_allow_private_networks` | 默认 `false`；通知目标的网络安全限制见部署文档 |

环境变量使用同名 `ORBITBEACON_` 前缀，对应 CLI 使用连字符，例如 `ORBITBEACON_MAX_NODES=100` / `--max-nodes 100`。Agent 五项运行设置由本地 JSON/env/CLI 合并；服务器地址、CA、状态目录和身份仍受安装策略保护。`systemctl reload orbitbeacon-agent` 发送 SIGHUP，结果以日志和下一报告为准，详见[接入指南](docs/agent.md)。

## API 与排障

接口契约见 [OpenAPI](api/openapi.yaml)。

- 管理端：`/api/v1/`，使用会话 Cookie；写请求校验 `Origin` 和会话 CSRF token。
- Agent 端：`/api/agent/v1/`，使用接入凭证或节点 Bearer 凭证，不能与浏览器会话互换。
- 公开端：`GET /api/public/v1/nodes?limit=50&offset=0` 和 `/api/public/v1/stream`，无需会话，仅返回公开白名单字段。公开设置由管理端保护。
- 健康检查：`GET /healthz/live` 检查进程，`GET /healthz/ready` 检查是否接收流量及 SQLite 是否可用；两者都不需要认证。
- 错误响应包含 `code`、`message`、`request_id` 和可选 `fields`。排障时记录 `request_id`，不要发送 Cookie、密码或 token。

## 开发

主要入口在 [`cmd/`](cmd/)，Server 和 Agent 实现在 [`internal/`](internal/)，Web 静态资源直接嵌入 Server 二进制。项目版本集中记录在根目录 [`VERSION`](VERSION)；发布流水线从该文件读取版本，不要在流水线中另行维护版本号。

```sh
GOTOOLCHAIN=go1.27.3 go mod verify
GOTOOLCHAIN=go1.27.3 go test -count=1 ./...
GOTOOLCHAIN=go1.27.3 go test -race -count=1 ./internal/server/... ./internal/agent/...
GOTOOLCHAIN=go1.27.3 go vet ./...
```

| Workflow | 触发与职责 |
| --- | --- |
| [CI](.github/workflows/ci.yml) | push / PR：并行执行 Go 格式、测试、race、跨平台 vet、Staticcheck，Web 产物一致性与测试，漏洞扫描，Agent 安装脚本检查与回归 |
| [Release](.github/workflows/release.yml) | 仅由 `main` push 的 CI 全部门禁通过后调用；发布 amd64/arm64 镜像和 Linux Server/Agent 包 |

Go 版本读取 `go.mod`，Node.js 版本读取 `web/.nvmrc`。发布镜像标签为版本号、提交 SHA 和 `latest`，Release tag 为 `v<VERSION>`；版本镜像、发布包及 `SHA256SUMS` 均已存在时跳过发布。PR、其他分支和 tag push 只检查、不发布。架构边界检查包含在 `go test -count=1 ./...` 中，不能依赖测试缓存。

## 许可证

**GNU GPL v3**，以仓库中的 [LICENSE](LICENSE) 为准。