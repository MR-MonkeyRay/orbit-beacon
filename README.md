# OrbitBeacon

自托管的多节点监控系统：Go 服务端提供 API、内嵌控制台与公开状态页，默认开箱使用 SQLite（可选 [MySQL 主备高可用](docs/storage-and-backup.md#2-mysql-主备与故障切换)）；Linux Agent 以专用非 root 账户运行并通过 HTTP / HTTPS 上报主机指标、流量与服务探测。

控制台与状态页支持 **简体中文（zh-CN）** / **English（en）**，状态页支持构建期主题替换。

---

## 核心特性

| 模块 | 能力概览 | 入口 / 工具 |
| --- | --- | --- |
| **节点监控** | CPU、内存、Swap、Load、磁盘/Inode、磁盘 IO、PSI、温度、实时网速 | 控制台 `节点 / Nodes` |
| **服务探测** | Agent 侧探测（TCP/HTTP/DNS/ICMP）+ 服务端独立监测（`services`）与 24h 可用率 | 节点详情 / `服务监测` |
| **流量计费** | 按 UTC 月与 `billing_day` 统计账期流量，支持 4 种计费模式与配额告警 | 节点详情 → `流量` |
| **告警通知** | 规则评估、静默窗口、站内通知与 8 种外发通道（Webhook/SMTP/Telegram/IM 等） | `告警` / `站内通知` |
| **远程运维** | 受本地策略约束的 `check_update` / `upgrade` / `restart` / `stop` 与自动回滚 | 节点详情 → `管理` / 批指令 |
| **公开状态页** | 白名单字段投影、可选分组展示、WS→SSE→HTTP 实时回退、可替换主题 | `/` 与 `设置 → 状态页` |
| **安全审计** | 管理员等权会话、可选 TOTP 两步验证、只读 `obt_` API 令牌、全量审计日志 | `设置 → 管理员 / API 令牌` |
| **备份高可用** | 三分类 `.tar.gz.age` 加密导出与计划备份、离线原子恢复、MySQL 异步主备 | `设置 → 数据` / `docs/` |
| **生态集成** | Prometheus `/metrics` + `/metrics/nodes`、只读 stdio MCP Server | `orbitbeacon-mcp` |

---

## 系统全景架构

```text
+-----------------------------------------------------------------------------------+
|                                   外部接入层                                      |
|  管理员浏览器 (Console)          匿名访客 (Status Page)         AI / MCP Client   |
+------------+------------------------------+-----------------------------+---------+
             | Cookie + CSRF                | 白名单投影                  | stdio JSON-RPC
             | WS -> SSE -> HTTP            | WS -> SSE -> HTTP           v
             v                              v                     +---------------+
+-----------------------------------------------------------------| orbitbeacon-  |
|                         HTTPS 反向代理 (Caddy / Nginx)          | mcp           |
+----------------------------------------+------------------------+-------+-------+
                                         | HTTP (127.0.0.1:8080)          | HTTPS GET
                                         |<-------------------------------+ (Bearer obt_)
                                         v
+-----------------------------------------------------------------------------------+
|                            orbitbeacon-server (Go 单二进制)                       |
|                                                                                   |
|  +--------------+   +---------------+   +----------------+   +-----------------+  |
|  | WebUI Embed  |   |  Ingest &     |   | Services       |   | Alert Evaluator |  |
|  | (HTML/JS/CSS)|   |  Live Hub     |   | (8 Workers)    |   | & Notify Outbox |  |
|  +--------------+   +-------+-------+   +-------+--------+   +--------+--------+  |
|                             |                   |                     |           |
|  +--------------------------v-------------------v---------------------v--------+  |
|  |                    Storage Engine (Domain Leases & Write Gate)              |  |
|  |   Main Domain (资产/配置/流量/告警/审计)   |   History Domain (指标与探测聚合)  |  |
|  +--------------------------+-----------------+-----------------------+--------+  |
+-----------------------------|-----------------------------------------|-----------+
                              |                                         |
            +-----------------+-----------------+                       |
            |                                   |                       v
            v                                   v               +---------------+
+-----------------------+           +-----------------------+   | 外部通知通道   |
| SQLite (默认单机)     |           | MySQL Primary/Standby |   | Webhook/SMTP/ |
| - orbitbeacon.db      |  或可选   | - 异步行日志复制      |   | TG/飞书/钉钉/ |
| - metrics.db          |           | - storage-control.db  |   | 企微/Bark/ntfy|
+-----------------------+           +-----------------------+   +---------------+
            ^                                                   +---------------+
            | 周期加密上传 (.tar.gz.age)                         | Prometheus    |
            +-------------------------------------------------->| :metrics_port |
                                                                +---------------+
            ^
            | HTTPS POST (/api/agent/v1/{enroll,report})
+-----------+-----------------------------------------------------------------------+
|  被控 Linux 主机: orbitbeacon-agent (非 root 专用账户)                            |
|  [主机采集: CPU/Mem/Disk/Net/IO/PSI/Temp] + [本地探测: TCP/HTTP/DNS/ICMP]         |
+-----------------------------------------------------------------------------------+
```

---

## 核心工作流一览

### 1. Agent 注册与监控数据流

```text
管理员签发一次性密钥 (30min 有效, 仅用 1 次)
   │
   ▼
Agent (enroll) ──[HTTPS + Key]──▶ Server 验证并签发 256-bit Node Credential
   │                                  (同事务写入 nodes + 审计; 密钥立即消费)
   ▼
Agent (run) 周期采集 ──[HTTPS POST /report]──▶ Server Ingest
                                                  ├──▶ 内存最新快照 ──▶ 实时推送 / /metrics/nodes
                                                  ├──▶ 业务库 Main (节流 30s 写 last_seen / 流量 / 探测 / 指令)
                                                  └──▶ 历史库 History (有界非阻塞队列 -> 1m/5m/1h 聚合)
```

### 2. 前端实时传输自动回退流

```text
浏览器 ──▶ [1. WebSocket] ──成功──▶ 首份快照 + 增量更新 / 10s 心跳
                 │ (失败或 8s 无快照)
                 ▼
           [2. SSE /stream] ──成功──▶ 快照与事件流 (每 5 分钟尝试恢复 WS)
                 │ (失败或 8s 无快照)
                 ▼
           [3. HTTP 轮询]  ─────────▶ 定时拉取快照 (页面隐藏暂停, 恢复可见重连)
```

---

## 5 分钟快速开始（本地体验）

以下命令在仓库根目录执行，仅需 Linux 与 [Go](go.mod)（服务端已内嵌前端构建产物，无需 Node.js）。生产部署请阅读 [服务器部署指南](deploy/README.md)。

```sh
# 1. 编译核心二进制（设置私有权限掩码）
umask 077
mkdir -p build "$HOME/orbitbeacon-demo"
chmod 700 "$HOME/orbitbeacon-demo"
go build -o build/orbitbeacon-server ./cmd/orbitbeacon-server
go build -o build/orbitbeacon-agent  ./cmd/orbitbeacon-agent
go build -o build/orbitbeacon-mcp    ./cmd/orbitbeacon-mcp

# 2. 启动服务端（默认监听 127.0.0.1:8080，SQLite 存储在 data-dir）
build/orbitbeacon-server serve \
  --config deploy/systemd/server.yaml \
  --data-dir "$HOME/orbitbeacon-demo"
```

```sh
# 3. 另开终端：健康检查
build/orbitbeacon-server health --url http://localhost:8080
```

4. **首次登录改密**：访问 <http://localhost:8080> → 点击“登录 / Sign in” → 使用初始管理员 `orbitbeacon` / `orbitbeacon` 登录 → 按强制提示修改密码并重新登录（自定义初始化凭据见[初始化配置](deploy/README.md#初始化凭据)）。
5. **注册 Agent**：进入 `节点 / Nodes` 新建节点并复制安装命令与一次性密钥（有效期 30 分钟），在目标 Linux 主机执行并按提示粘贴密钥；本地可使用 `orbitbeacon-agent doctor` 自检。
6. **发布到状态页**（可选）：在节点详情 `设置 → 状态页展示` 填写公开信息并勾选“公开此节点”。公开字段匿名可见、不会自动脱敏，请勿填写内网 IP、主机名或凭据。

> [!NOTE]
> HTTP 不加密密码、会话与节点凭据，仅限本机或受控网络调试；暴露到公网前务必配置 [HTTPS 反向代理](deploy/README.md#4-https-反向代理与静态-cdn)。

---

## 仓库目录结构

```text
orbit-beacon/
├── api/
│   └── openapi.yaml          # 服务端、Agent 与匿名 API 的 OpenAPI 3.1 契约 (权威接口定义)
├── build/
│   └── server.Dockerfile     # 服务端多架构容器构建文件
├── cmd/                      # 四个可执行程序入口
│   ├── orbitbeacon-server/   #   主服务端 (serve / restore / reset-totp / storage / health / version)
│   ├── orbitbeacon-agent/    #   Linux 采集端 (run / enroll / doctor / version)
│   ├── orbitbeacon-mcp/      #   只读 MCP stdio 服务端
│   └── orbitbeacon-load/     #   开发压测工具 (不进入发布产物)
├── deploy/                   # 部署清单与脚本
│   ├── install-agent.sh      #   Agent 自动化安装/升级/卸载脚本及测试
│   ├── proxy/                #   Caddyfile 与 nginx.conf 反向代理模板
│   ├── systemd/              #   systemd unit 与默认 YAML 配置模板
│   └── docker/               #   Docker Compose 示例配置
├── docs/                     # 分主题技术与运维文档
├── internal/                 # 核心业务实现 (包依赖受 internal/architecture 严格约束)
│   ├── agent/                #   采集(collect)、文件锁(filelock)、身份(identity)、探测(probe)、服务循环(service)
│   ├── config/               #   配置分层加载 (CLI > Env > YAML > Default) 与安装策略校验
│   ├── protocol/v1/          #   Agent-Server 通信协议、指标契约与 64 KiB 报文预算
│   └── server/               #   HTTP 路由、状态机、alerts/auth/backup/history/ingest/notify/services/storage 等
└── web/                      # 原生 JS/CSS 前端源码、esbuild 构建器与可替换状态页主题
    ├── src/{console,public,shared}/
    └── themes/{default,example}/
```

---

## 文档导航

按场景选择对应文档，避免通篇盲读：

| 你想了解…… | 推荐阅读 |
| --- | --- |
| **生产环境部署 Server、HTTPS 反代、CDN、Webhook 验签** | [服务器部署与配置指南](deploy/README.md) |
| **安装 / 运维 Linux Agent、本地配置、探测、远程升级与回滚** | [Linux Agent 安装与运维指南](docs/agent.md) |
| **系统内部架构、数据流、状态机、实时传输、安全边界与告警** | [系统架构与核心边界](docs/architecture.md) |
| **SQLite / MySQL 异步主备切换、人工恢复、数据清理与加密备份** | [存储架构、MySQL 高可用与备份恢复](docs/storage-and-backup.md) |
| **接入 AI 客户端的只读 MCP 工具** | [只读 MCP 客户端指南](docs/mcp.md) |
| **修改控制台界面或自定义公开状态页主题** | [Web 开发指南](web/README.md) · [状态页主题开发](web/themes/README.md) |
| **查阅完整 HTTP / WS / SSE 字段与错误码** | [OpenAPI 3.1 接口契约](api/openapi.yaml) |
| **术语对照、结构化日志规范与文案准则** | [术语、日志与约定](docs/conventions.md) |

---

## 开发、测试与发布

```sh
# Go 代码校验与全量测试 (含竞态检测)
go mod verify
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...

# 前端校验与构建 (需 Node.js 22+)
npm --prefix web ci
npm --prefix web run check
npm --prefix web test

# Agent 安装脚本测试
sh deploy/install-agent.test.sh
```

- **CI 门禁**：PR 与 `main` push 自动运行 `gofmt`、`go vet`、`staticcheck`、`govulncheck`、Go 单测与竞态测试、Web 一致性校验及安装脚本测试（见 [ci.yml](.github/workflows/ci.yml)）。
- **发布产物**：`main` 分支通过全部检查后触发 [release.yml](.github/workflows/release.yml)，发布 `server` / `agent` / `mcp` 的 `linux/amd64` 与 `linux/arm64` 独立二进制、`SHA256SUMS` 与多架构服务端镜像。
- **重要边界**：Agent 仅支持 Linux `amd64`/`arm64`；远程升级来源仅由本地只读的 `update_base_url` 决定，服务端不指定版本或 URL，亦不提供任意命令执行。

---

## 许可证

本项目遵循 [GNU General Public License v3.0](LICENSE) 开源。
