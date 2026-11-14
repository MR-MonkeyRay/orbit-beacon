# OrbitBeacon

自托管的多节点监控系统：Go 服务器提供 API、内嵌控制台与状态页，默认使用 SQLite，也可配置 [MySQL 主备](deploy/README.md#数据库)；Linux Agent 以非 root 身份采集并通过 HTTP / HTTPS 上报。控制台与状态页支持简体中文（zh-CN）和英文（en），状态页支持可替换主题。

## 功能

| 功能 | 入口 / 方式 |
| --- | --- |
| 节点指标、历史、探测与可用率 | 节点 / Nodes；节点详情 → 监控 / Monitoring、服务探测 / Service probes |
| 流量计费与配额告警 | 节点详情 → 流量 / Traffic；告警 / Alerts（配额规则） |
| Agent 注册、检查更新与升级 | 节点详情 → 管理 / Manage；节点分组操作菜单支持批量指令 |
| 状态页、公开字段与主题 | 设置 → 状态页 / Status page；节点详情 → 设置 → 状态页展示 / Settings → Status page listing；构建时选择 `web/themes/<name>` |
| 告警、静默与通知 | 告警 / Alerts；站内通知 / Notifications |
| 管理员、两步验证与只读 API 令牌 | 设置 → 管理员 / Administrators、账户 / Account、API 令牌 / API tokens |
| 加密导出、备份计划、恢复与数据清理 | 设置 → 数据 / Settings → Data（备份与导出、存储与清理） |

## 重要限制

- 控制台支持桌面、平板与手机；手机使用抽屉导航，宽表格横向滚动。状态页支持所有设备。
- 节点状态、历史、可用率与通知可靠性边界见[状态与历史](docs/architecture.md#状态与历史)、[实时传输](docs/architecture.md#实时传输)、[告警与通知](docs/architecture.md#告警与通知)。
- 公开字段为匿名可见文本，不自动脱敏；不要填写凭据、IP、主机名或秘密。
- 当前部署支持 Linux；Agent 按本地许可检查更新、升级、重启或关闭。更新来源仅由本地 `update_base_url` 决定，默认 `https://github.com/mr-monkeyray/orbit-beacon/releases/download`，自动识别 GitHub Release 或其他 HTTPS 源；服务器不指定版本或地址，不提供终端或任意命令执行。

## 快速开始

以下在仓库根目录执行；需要 Linux 和 [Go](go.mod)，无需 Node.js。正式部署、HTTPS 与目录权限见[部署指南](deploy/README.md)。

### 1. 编译

```sh
umask 077
mkdir -p build "$HOME/orbitbeacon-demo"
chmod 700 "$HOME/orbitbeacon-demo"
go build -o build/orbitbeacon-server ./cmd/orbitbeacon-server
go build -o build/orbitbeacon-agent ./cmd/orbitbeacon-agent
go build -o build/orbitbeacon-mcp ./cmd/orbitbeacon-mcp
```

### 2. 启动

```sh
build/orbitbeacon-server serve --config deploy/systemd/server.yaml --data-dir "$HOME/orbitbeacon-demo"
```

默认监听 `127.0.0.1:8080`。HTTP 不加密密码与凭据，仅用于本机或受控网络；公网访问前请配置外置 HTTPS 代理。

首次启动空用户库会创建 `orbitbeacon` / `orbitbeacon` 管理员，首次登录必须修改密码。自定义初始化凭据见[配置参考](deploy/README.md#配置参考)。

### 3. 健康检查

另开终端执行；检查含义见[日常检查](deploy/README.md#日常检查)。

```sh
build/orbitbeacon-server health --url http://localhost:8080
```

### 4. 登录

访问 <http://localhost:8080>，点击“登录 / Sign in”，按提示改密后重新登录。

### 5. 注册 Agent

在“节点 / Nodes”添加节点后，复制命令，在主机执行，按提示粘贴单独复制的密钥，再等待上报。命令默认不含密钥，可开启“命令中包含密钥 / Include key in command”；密钥有效 30 分钟、仅用一次。已有节点用“更多 → 注册 Agent / More → Enroll Agent”，分组入口每次部署一个 Agent。安装与恢复见 [Agent 指南](docs/agent.md#密钥)。Agent 采集配置保存在本地，使用 `orbitbeacon-agent doctor` 检查。

### 6. 公开节点

在节点详情“设置 → 状态页展示 / Settings → Status page listing”填写公开信息，勾选“公开此节点 / Publish this node”。展示资格、公开范围及全站开关规则见[安全](docs/architecture.md#安全)、[实时传输](docs/architecture.md#实时传输)。

## 文档

- [部署与配置、备份与恢复](deploy/README.md)
- [Agent 安装、配置与排障](docs/agent.md)
- [只读 MCP 客户端](docs/mcp.md)
- [数据流、状态、保留与安全边界](docs/architecture.md)
- [Web 开发](web/README.md)
- [公开首页主题开发](web/themes/README.md)
- [术语、日志与文案约定](docs/conventions.md)
- [接口契约（OpenAPI）](api/openapi.yaml)

## 开发与发布

入口在 [`cmd/`](cmd/)，实现在 [`internal/`](internal/)；界面修改与产物同步见 [Web 开发](web/README.md)。

接口字段、认证与错误码见[接口契约（OpenAPI）](api/openapi.yaml)。

```sh
go mod verify
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
```

[CI](.github/workflows/ci.yml) 在 PR 和 `main` push 时运行 Go 测试（含 race）、静态分析、Web 检查、漏洞扫描与安装脚本测试；远程 MySQL 回归仅在配置测试凭据后的 `main` push 时运行。

`main` 全部检查通过后调用[发布流程](.github/workflows/release.yml)，提供 server、agent、mcp 的 amd64 / arm64 独立二进制、`SHA256SUMS` 与双架构服务端镜像。手动下载后先核对校验和，再 `chmod +x`。

### Agent IP 地理位置

服务器记录 Agent 客户端 IP，并遵循 `trusted_proxies` 配置。默认启用 `geoip_lookup: true`：未手动设置地区的节点会将公网 IP 发送给 **ipinfo.io** 查询国家和地区。成功或无效响应后缓存 24 小时；IP 变化后重新查询。设置 `geoip_lookup: false` 或 `ORBITBEACON_GEOIP_LOOKUP=false` 可关闭外部查询，IP 记录仍保留。可选 `geoip_token` / `ORBITBEACON_GEOIP_TOKEN` 或 `geoip_token_file` / `ORBITBEACON_GEOIP_TOKEN_FILE` 提供 token，两者不可同时配置；文件须为同用户所有的 `0600` 普通文件。

公开设置为 `{enabled, fields}`。已发布节点始终显示自身名称、状态和 `last_seen`，其余键仅在选中时出现；已选中但无数据的指标为 `null`。地区优先使用手动值，否则使用自动位置。公开流量目前仅提供 `quota_bytes`，避免匿名请求逐节点查询账期用量。排序为 `display_order DESC, name, id`，集合不变时偏移分页顺序稳定。

配置备份包含显示权重和套餐字段。IP 与地理位置随 `last_seen`、`system` 归入监控类别：它们是运行观测，不是管理员配置。仅恢复配置时保留目标节点已有的运行观测，不从配置备份恢复地址。

## 许可证

[GNU GPL v3](LICENSE)。
