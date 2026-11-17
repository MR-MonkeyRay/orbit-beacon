# 只读 MCP 客户端（`orbitbeacon-mcp`）

`orbitbeacon-mcp` 是独立的标准输入输出（stdio）MCP 进程，通过 HTTP GET 只读访问服务端开放的 `obt_` API 令牌接口，适合接入支持 MCP 的 AI 助手或自动化工具。

---

## 1. 工作架构

```text
+-------------------+   stdio JSON-RPC    +--------------------+   HTTPS GET           +--------------------+
| AI Client / IDE   | <=================> | orbitbeacon-mcp    | ----------------------> | orbitbeacon-server |
| (Claude/Cursor等) |  (stdout 仅传协议;  | (无状态, 15s 超时,  |  Authorization:         | (/api/v1/* 白名单) |
+-------------------+   stderr 输出诊断)  |  禁用重定向, ≤4MiB) |  Bearer obt_<token>     +--------------------+
                                          +--------------------+
```

- **版本支持**：支持 MCP 协议 `2025-06-18`、`2025-03-26` 与 `2024-11-05`。
- **安全边界**：`orbitbeacon-mcp` **不提供写工具**，不扩大令牌权限；工具返回中的节点名、规则名属于不可信监控文本，不可直接当作指令执行。

---

## 2. 安装与环境变量配置

按 [README 快速编译](../README.md#5-分钟快速开始本地体验) 生成 `build/orbitbeacon-mcp`，或下载对应架构的 `orbitbeacon-mcp-linux-{amd64,arm64}` 发布二进制（`orbitbeacon-mcp version` 可查看构建信息）。

仅通过环境变量读取配置（不接受命令行传令牌）：

| 环境变量 | 要求与说明 |
| --- | --- |
| `ORBITBEACON_URL` | 服务端根 URL（允许末尾 `/`）；**仅回环地址 `127.0.0.0/8`、`::1`、`localhost` 可用 HTTP**，其余地址强制要求 HTTPS |
| `ORBITBEACON_TOKEN` | 控制台“设置 → API 令牌”创建的只读令牌（格式为 `obt_` + 43 位 base64url 字符） |

### MCP 客户端配置示例

```json
{
  "mcpServers": {
    "orbitbeacon": {
      "command": "/absolute/path/orbitbeacon-mcp",
      "env": {
        "ORBITBEACON_URL": "https://monitor.example.com",
        "ORBITBEACON_TOKEN": "<your-read-only-obt-token>"
      }
    }
  }
}
```

---

## 3. 可用工具速查（共 14 个）

所有 `id` 参数均为合法 UUID；列表省略 `limit` 时默认 50，天数省略 `days` 时默认 30。

| 工具名 | 参数 | 返回说明 |
| --- | --- | --- |
| `list_nodes` | `limit?` (1–100), `offset?` (≥0) | 节点只读概览列表 |
| `get_node` | `id` | 单个节点基础信息（不含备注、系统敏感信息及探测原始错误） |
| `get_node_latest` | `id` | 节点最新内存上报快照 |
| `get_node_history` | `id`, `metric`, `range`, `disk?` | `range`: `1h`/`6h`/`24h`/`7d`/`30d`/`90d`；`disk?` 仅用于磁盘/Inode 指标 |
| `get_node_availability` | `id`, `days?` (`30` 或 `90`) | 按 UTC 日统计的可用率 |
| `get_node_traffic` | `id`, `days?` (1–400) | 流量计费统计 |
| `get_node_probes` | `id` | Agent 本地服务探测摘要 |
| `list_services` | 无 | 服务端监测项配置、最新状态与 24h 可用率 |
| `get_service` | `id` | 单个服务监测详情 |
| `get_service_history` | `id`, `range` (`1h`/`24h`/`7d`/`30d`) | 5 分钟聚合历史桶 |
| `list_alerts` | `state?` (`firing`/`resolved`/`all`), `limit?`, `before?` | 告警事件游标分页 |
| `get_alert_summary` | 无 | 告警活动统计 |
| `get_alert` | `id` | 单个告警事件详情 |
| `list_groups` | 无 | 节点分组列表 |

<details>
<summary><code>get_node_history</code> 支持的 17 个指标名</summary>

`cpu_percent`、`memory_used_percent`、`swap_used_percent`、`load1`、`load5`、`load15`、`disk_used_percent`、`inode_used_percent`、`net_rx_bytes_per_second`、`net_tx_bytes_per_second`、`disk_read_bytes_per_second`、`disk_write_bytes_per_second`、`disk_busy_percent`、`pressure_cpu_percent`、`pressure_memory_percent`、`pressure_io_percent`、`temperature_celsius`（单位 `celsius`）。其中仅 `disk_used_percent` 与 `inode_used_percent` 支持传入绝对路径参数 `disk`。

</details>
