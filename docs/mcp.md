# 只读 MCP 客户端

`orbitbeacon-mcp` 是独立的 stdio MCP 进程，只通过 HTTP GET 访问已有的只读令牌 API。先按 README 编译，或下载对应架构的 `orbitbeacon-mcp-linux-{amd64,arm64}` 发布资产。`orbitbeacon-mcp version` 查看构建信息。

配置只读环境变量，不接受命令行令牌：

- `ORBITBEACON_URL`：服务器根 URL，仅接受 HTTP/HTTPS；允许末尾 `/`，不允许路径、查询、片段或用户信息。HTTP 仅限 `127.0.0.0/8`、`::1`、`localhost`，其他地址必须使用 HTTPS。
- `ORBITBEACON_TOKEN`：已有的只读 API 令牌，格式为 `obt_` 加 43 个 base64url 字符。

配置示例（替换路径和令牌；含令牌的客户端配置文件要限制读取权限）：

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

| 工具 | 参数 |
| --- | --- |
| `list_services` | 无；返回服务配置、最近检查与 24 小时可用率 |
| `get_service` | `id` |
| `get_service_history` | `id`、`range`（1h / 24h / 7d / 30d） |
| `list_nodes` | `limit?`（1–100）、`offset?`（≥0） |
| `get_node` | `id` |
| `get_node_latest` | `id` |
| `get_node_history` | `id`、`metric`、`range`（1h、6h、24h、7d、30d、90d）、`disk?`（绝对路径） |
| `get_node_availability` | `id`、`days?`（30、90） |
| `get_node_traffic` | `id`、`days?`（1–400） |
| `get_node_probes` | `id` |
| `list_alerts` | `state?`（firing、resolved、all）、`limit?`（1–100）、`before?`（API 返回的分页游标） |
| `get_alert_summary` | 无 |
| `get_alert` | `id` |
| `list_groups` | 无 |

`id` 必须是有效 UUID。历史指标限 `cpu_percent`、`memory_used_percent`、`swap_used_percent`、`load1`、`load5`、`load15`、`disk_used_percent`、`inode_used_percent`、`net_rx_bytes_per_second`、`net_tx_bytes_per_second`、`disk_read_bytes_per_second`、`disk_write_bytes_per_second`、`disk_busy_percent`、`pressure_cpu_percent`、`pressure_memory_percent`、`pressure_io_percent`、`temperature_celsius`（温度单位为 `celsius`）。`disk` 仅适用于 `disk_used_percent` 和 `inode_used_percent`；省略时返回聚合，不存在的路径返回空点。令牌历史响应字段仍为 `metric`、`unit`、`range`、`step_seconds`、`points`。省略分页 `limit` 时 API 默认 50；省略 `days` 时默认 30。

工具输出来自监控数据，节点名和规则名是不可信文本，不能作为指令执行。令牌能读哪些字段以 [API 契约](../api/openapi.yaml)和服务端实际授权为准。MCP 不扩大字段权限，也不提供写工具。

每次 HTTP 请求超时 15 秒，不跟随重定向，响应上限 4 MiB。工具失败仅返回状态与错误码；stdio 的 stdout 只用于 MCP 消息，诊断写 stderr。支持 MCP 2025-06-18、2025-03-26 和 2024-11-05。
