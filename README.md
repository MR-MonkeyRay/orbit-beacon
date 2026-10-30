# OrbitBeacon

自托管的多节点监控系统：Go 服务器提供 API、内嵌网页与 SQLite 存储；Linux Agent 以非 root 身份采集并通过 HTTP / HTTPS 上报。控制台与状态页均支持简体中文（zh-CN）和英文（en）。

## 功能

| 功能 | 界面入口 |
| --- | --- |
| 节点指标、历史与服务探测 | 节点 / Nodes；Agent 设置 / Agent settings 查看本地运行参数 |
| 流量计费 | 节点详情 → 详情 → 流量计费 / Details → Traffic billing |
| Agent 注册 | 节点详情 → 注册 Agent / Enroll Agent；分组支持批量注册 |
| 匿名展示 | 设置 → 状态页 / Settings → Status page；节点详情 → 状态页展示 / Status page listing |
| 告警规则、事件与通知通道 | 告警 / Alerts |
| 管理员与个人密码 | 设置 → 管理员、账户 / Settings → Administrators, Account |
| 加密导出与备份计划 | 设置 → 数据操作 / Settings → Data operations |
| 告警与备份结果 | 站内通知 / Notifications |

## 重要限制

- 控制台面向桌面和平板，最小宽度 744px；状态页支持手机。
- 节点状态、指标历史与通知可靠性边界见[状态与历史](docs/architecture.md#状态与历史)、[告警与通知](docs/architecture.md#告警与通知)。
- 当前部署支持 Linux；Agent 不提供远程升级、终端或任意命令执行。

## 快速开始

以下在仓库根目录执行；需要 Linux 和 [Go](go.mod)，无需 Node.js。正式部署、HTTPS 与目录权限见[部署指南](deploy/README.md)。

### 1. 编译

```sh
umask 077
mkdir -p build "$HOME/orbitbeacon-demo"
chmod 700 "$HOME/orbitbeacon-demo"
go build -o build/orbitbeacon-server ./cmd/orbitbeacon-server
go build -o build/orbitbeacon-agent ./cmd/orbitbeacon-agent
```

### 2. 启动

```sh
build/orbitbeacon-server serve --data-dir "$HOME/orbitbeacon-demo"
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

在“节点 / Nodes”添加节点，进入“注册 Agent / Enroll Agent”，按 [Agent 指南](docs/agent.md#注册票据)签发注册票据并安装；回到节点详情确认最近上报与样本。注册成功不等于已上报。

### 6. 公开节点

在“状态页展示 / Status page listing”填写公开信息，勾选“公开此节点 / Publish this node”。展示资格、公开范围及全站开关规则见[安全](docs/architecture.md#安全)、[实时传输](docs/architecture.md#实时传输)。

## 文档

- [部署与配置、备份与恢复](deploy/README.md)
- [Agent 安装、配置与排障](docs/agent.md)
- [数据流、状态、保留与安全边界](docs/architecture.md)
- [Web 开发](web/README.md)
- [术语、日志与文案约定](docs/conventions.md)
- [HTTP API](api/openapi.yaml)

## 开发与发布

入口在 [`cmd/`](cmd/)，实现在 [`internal/`](internal/)；界面修改与产物同步见 [Web 开发](web/README.md)。

```sh
go mod verify
go test -count=1 ./...
go test -race -count=1 ./internal/server/... ./internal/agent/...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
```

[CI](.github/workflows/ci.yml) 检查 Go、Web、漏洞及安装脚本；`main` push 全部通过后调用[发布流程](.github/workflows/release.yml)。Release 标题直接使用 [`VERSION`](VERSION)（如 `0.1.0`），Git 标签仍为 `v0.1.0`。直接上传 `orbitbeacon-<server|agent>-linux-<amd64|arm64>` 四个独立二进制及统一的 `SHA256SUMS`，文件名不重复版本号、不再打压缩包；镜像仍支持两种架构。手动下载后先核对校验和，再用 `chmod +x` 赋予执行权限。重跑同版本发布会在二进制上传成功后移除该版本原有的分架构压缩包。使用前确认目标版本已发布。

## 许可证

[GNU GPL v3](LICENSE)。
