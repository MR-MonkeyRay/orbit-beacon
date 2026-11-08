# 术语与约定

## 术语

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
| 告警静默 | Alert silence |
| 通知通道 | Notification channel |
| 站内通知 | Notifications |
| 公告 | Announcement |
| 在线 / 离线 / 未知 / 已过期 | Online / Offline / Unknown / Stale |
| 待注册 / 未上报 / 禁用 | Pending / Never seen / Disabled |
| 可用率 | Availability |
| 两步验证 / 恢复码 | Two-step verification / Recovery code |
| 只读 API 令牌 | Read-only API token |
| 导出 / 备份 / 恢复 | Export / Backup / Restore |
| 流量计费 | Traffic billing |
| 数据管理 | Data management |

状态页指匿名页面，其 H1 使用“节点状态 / Node status”。

## 日志

Go 日志约定：

- 每个可执行程序创建一个 `slog` logger：服务器使用 JSON，Agent 使用 text；向组件注入，不调用全局 `slog`。
- `msg` 使用稳定的小写 `snake_case` 事件名，如 `server_started`、`retention_cleanup_failed`。
- 字段名使用 `snake_case`：`component`、`request_id`、`route`、`method`、`status`、`duration_ms`、`node_id`、`task_id`、`channel_id`；`route` 是有界路由标签，不记录原始路径。
- 错误仅记录 `error_kind`、`error_code`；SQLite 可加 `sqlite_code`，MySQL 可加 `mysql_code`。统一使用 `internal/logging.ErrorAttrs`，不记录原始错误文本、SQL、绑定值或 DSN；MySQL 驱动日志也只输出类型及数值元数据。
- INFO 表示生命周期与完成；WARN 表示降级与重试；ERROR 表示操作失败。同一错误只记录一次，不逐层重复。
- CLI 帮助和命令输出（包括 stderr 上的命令错误）使用纯文本；显式 `--json` 输出仍按命令约定提供 JSON。

## 文案

- 标题用名词，按钮用动词；页头默认只保留标题和操作，不另写功能介绍或重复副标题。
- 计数、范围和状态由对应控件表达；正常加载完成后清空临时状态说明，保留加载、失败、陈旧及部分数据提示。
- 字段说明只保留影响输入或决策的信息；破坏性操作、凭据恢复、公开隐私、必要格式与单位就近说明一次，不重复占用多个段落。无障碍操作说明与校验反馈不得因精简而移除。
- 错误文案说明“发生了什么 + 如何处理”。
- HTTPS、TLS、DNS、SMTP、STARTTLS、Cron 等协议或技术名称保留英文。
- 不翻译用户输入或标识符；配置键、命令、日志事件名与代码名称保留原文。
- 内部错误、测试诊断和固定探针输出不为中文化而改写；CLI 帮助和操作提示使用中文，中英文界面各用对应语言。
