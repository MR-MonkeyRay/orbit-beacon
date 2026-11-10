# OrbitBeacon

自托管的多节点监控系统：Go 服务器提供 API、内嵌控制台与状态页，默认使用 SQLite，也可配置 [MySQL 主备](deploy/README.md#数据库)；Linux Agent 以非 root 身份采集并通过 HTTP / HTTPS 上报。控制台与状态页支持简体中文（zh-CN）和英文（en），状态页支持公告与可替换主题。

## 功能

| 功能 | 入口 / 方式 |
| --- | --- |
| 节点指标、历史、探测与可用率 | 节点 / Nodes；节点详情 → 监控 / Monitoring、服务探测 / Service probes |
| 流量计费与配额告警 | 节点详情 → 流量 / Traffic；告警 / Alerts（配额规则） |
| Agent 注册、运行设置与远程升级 | 节点详情 → 注册 Agent / Enroll Agent、Agent 设置 / Agent settings；设置 → 分组 / Groups |
| 状态页、公告、公开字段与主题 | 设置 → 状态页 / Status page；节点详情 → 状态页展示 / Status page listing；构建时选择 `web/themes/<name>` |
| 告警、静默与通知 | 告警 / Alerts；站内通知 / Notifications |
| 管理员、两步验证与只读 API 令牌 | 设置 → 管理员 / Administrators、账户 / Account |
| 加密导出、备份计划、恢复与数据清理 | 设置 → 数据操作 / Data operations；数据管理 / Data management |

## 重要限制

- 控制台支持桌面、平板与手机；手机使用抽屉导航，宽表格横向滚动。状态页支持所有设备。
- 节点状态、历史、可用率、公告与通知可靠性边界见[状态与历史](docs/architecture.md#状态与历史)、[实时传输](docs/architecture.md#实时传输)、[告警与通知](docs/architecture.md#告警与通知)。
- 公告与公开字段均为匿名可见文本，不自动脱敏；不要填写凭据、IP、主机名或秘密。
- 当前部署支持 Linux；Agent 支持本地许可控制的远程版本升级，不提供终端或任意命令执行。

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

在“节点 / Nodes”添加节点后，复制弹窗中的完整部署命令，在 Agent 主机执行；票据 30 分钟内仅可成功激活一次。已有节点从详情“注册 Agent / Enroll Agent”重新签发，分组入口为该组部署一个 Agent。安装与恢复见 [Agent 指南](docs/agent.md#注册票据)。回到节点详情确认最近上报与样本；注册成功不等于已上报。

### 6. 公开节点

在“状态页展示 / Status page listing”填写公开信息，勾选“公开此节点 / Publish this node”。展示资格、公开范围及全站开关规则见[安全](docs/architecture.md#安全)、[实时传输](docs/architecture.md#实时传输)。全站公告在“设置 → 状态页 / Settings → Status page”添加，只在开始时间后显示；设置结束时间后到点隐藏。

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

## 许可证

[GNU GPL v3](LICENSE)。
