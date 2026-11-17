# 术语、日志与文案约定

本文统一 OrbitBeacon 代码、文档与界面中的命名、结构化日志及交互文案规范。

---

## 1. 中英文核心术语对照

| zh-CN | en | 说明 |
| --- | --- | --- |
| 服务器 / 控制台 | Server / Console | 服务端进程与管理员 Web 界面 |
| Agent / 节点 | Agent / Node | 采集端二进制与被监控资产 |
| 服务 | Service | 服务端主动探测的 HTTP/TCP/ICMP 端点 |
| 状态页 | Status page | 匿名公开页面（H1 标题：`节点状态 / Node status`） |
| 密钥 / 注册 | Key / Enroll | 一次性 30 分钟激活密钥与首次注册动作 |
| 节点凭据 | Node credential | 注册后绑定的持久 256-bit 认证凭据 |
| 上报 | Report | Agent 周期发送的指标、流量与探测载荷 |
| 告警规则 / 告警事件 / 告警静默 | Alert rule / Alert event / Alert silence | 规则定义、持久化触发记录与静默窗口 |
| 通知通道 / 站内通知 | Notification channel / Notifications | 外部投递渠道与控制台共享通知收件箱 |
| 在线 / 离线 / 未知 / 已过期 | Online / Offline / Unknown / Stale | 运行在线态与数据新鲜度状态 |
| 待注册 / 未上报 / 禁用 | Pending / Never seen / Disabled | 节点生命周期状态 |
| 可用率 / 流量计费 | Availability / Traffic billing | 日级可用百分比与账期字节用量 |
| 两步验证 / 恢复码 | Two-step verification / Recovery code | TOTP 认证与 10 个一次性备用码 |
| 只读 API 令牌 | Read-only API token | `obt_` 前缀只读集成凭据 |
| 导出 / 备份 / 恢复 / 存储与清理 | Export / Backup / Restore / Storage & cleanup | 加密归档与存储维护操作 |

---

## 2. Go 结构化日志规范（`slog`）

```json
{"time":"2026-11-16T08:00:00Z","level":"ERROR","msg":"retention_cleanup_failed","component":"retention","error_kind":"sqlite","error_code":"cleanup_failed","sqlite_code":5}
```

- **Logger 实例**：每个可执行程序独立创建 `slog.Logger` 并显式注入组件（Server 用 JSON，Agent 用 text），**禁止调用全局 `slog` 默认函数**。
- **事件名与字段**：`msg` 使用稳定小写 `snake_case`（如 `server_started`、`retention_cleanup_failed`）；字段名如 `component`、`request_id`、`route`（有界路由模板，非原始 URL）、`method`、`status`、`duration_ms`、`node_id`、`task_id`、`channel_id`。
- **安全错误属性**：统一使用 `internal/logging.ErrorAttrs(err, code)`，仅输出 `error_kind`、`error_code` 及可选 `sqlite_code` / `mysql_code`；**绝不输出原始错误文本、SQL 语句、绑定参数或 DSN**（`MySQLLogger` 同理）。
- **级别语义**：`INFO` 表示生命周期与完成；`WARN` 表示降级与重试；`ERROR` 表示操作失败。同一失败只记录一次，不在调用栈逐层重复打日志。
- **CLI 输出**：命令行帮助与错误（`stderr`）使用纯文本；显式指定 `--json` 时输出机器可读 JSON。

---

## 3. 界面与文档文案准则

- **精简直观**：标题用名词，按钮用动词；页头默认只放标题和操作，不堆砌引导长文。
- **就近说明**：破坏性操作、凭据不可恢复提示、公开隐私警示及单位/格式要求在控件旁说明一次，不重复占位；不因精简移除无障碍标签与校验提示。
- **错误结构**：所有面向用户的错误须说明 **“发生了什么 + 如何处理”**。
- **技术词保留**：协议/技术专有名词（HTTPS、TLS、DNS、SMTP、STARTTLS、Cron）、配置键、命令、日志事件名与代码标识符均保持英文原文，不作强行翻译。

---

## 4. 测试夹具约定

数据库驱动包装器面向项目自用的 SQLite/MySQL 驱动，通过 `driver.ConnBeginTx` 转发上下文与事务隔离/只读选项；保留 `Begin` 仅用于满足标准库接口契约。
