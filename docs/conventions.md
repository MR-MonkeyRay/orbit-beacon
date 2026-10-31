# 术语与约定

## 术语

文档与界面统一使用下表。

| zh-CN | en |
| --- | --- |
| 服务器 | Server |
| Agent | Agent |
| 控制台 | Console |
| 节点 | Node |
| 状态页 | Status page |
| 注册票据 / 注册 | Enrollment ticket / Enroll |
| 节点凭据 | Node credential |
| 上报 | Report |
| 告警规则 / 告警事件 | Alert rule / Alert event |
| 通知通道 | Notification channel |
| 站内通知 | Notifications |
| 在线 / 离线 / 未知 / 已过期 | Online / Offline / Unknown / Stale |
| 导出 / 备份 / 恢复 | Export / Backup / Restore |
| 流量计费 | Traffic billing |

状态页指匿名页面，其 H1 使用“节点状态 / Node status”。

## 日志

Go 日志约定：

- 每个可执行程序创建一个 `slog` logger：服务器使用 JSON，Agent 使用 text；向组件注入，不调用全局 `slog`。
- `msg` 使用稳定的小写 `snake_case` 事件名，如 `server_started`、`retention_cleanup_failed`。
- 字段名使用 `snake_case`：`component`、`request_id`、`route`、`method`、`status`、`duration_ms`、`node_id`、`task_id`、`channel_id`；`route` 是有界路由标签，不记录原始路径。
- 错误仅记录 `error_kind`、`error_code`，SQLite 错误可加 `sqlite_code`；使用 `internal/logging.ErrorAttrs`，不记录原始错误文本。
- INFO 表示生命周期与完成；WARN 表示降级与重试；ERROR 表示操作失败。同一错误只记录一次，不逐层重复。
- CLI 帮助和命令输出（包括 stderr 上的命令错误）使用纯文本；显式 `--json` 输出仍按命令约定提供 JSON。

## 文案

- 标题用名词，按钮用动词，页面副标题只写一句话。
- 错误文案说明“发生了什么 + 如何处理”。
- HTTPS、TLS、DNS、SMTP、STARTTLS、Cron 等协议或技术名称保留英文。
- 不翻译用户输入或标识符；配置键、命令、日志事件名与代码名称保留原文。
- 内部错误、测试诊断和固定探针输出不为中文化而改写；CLI 帮助和操作提示使用中文，中英文界面各用对应语言。
