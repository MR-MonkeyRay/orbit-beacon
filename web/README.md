# Web UI

英文管理控制台与中文公开节点页，使用原生 JavaScript/CSS 和 esbuild，无前端运行时依赖或外部字体/CDN。产物提交在 `internal/server/webui/dist` 并由 Go `embed` 打包；普通 Go 构建和 Server 运行不需要 Node.js。

## 修改界面

需要 Node.js 22+（仓库 `.nvmrc` 为 22）。在仓库根目录执行：

```sh
cd web
npm ci
npm run build   # 更新提交的内嵌产物
npm run watch   # 监听 src 并重建；与 build 二选一
```

这里没有独立开发服务器。重建网页后，还需重新编译并重启 Server 才能看到变更；部署和启动方式见[部署说明](../deploy/README.md)。

提交界面变更时一并提交源码与产物，并运行：

```sh
npm run check  # 临时构建并逐文件比较 dist，不修改已提交产物
npm test       # Node.js 前端回归
```

CI 会运行这两项检查。构建目标为 Chrome 111、Firefox 115、Safari 16.4；这不是浏览器实测兼容性保证。

异步测试通过 `test/console-dom.js` 的 `flush()` 等待事件循环检查点，不依赖固定微任务次数；模拟计时器仍需显式推进。

## 源码入口

| 路径 | 用途 |
| --- | --- |
| `src/console/` | 管理控制台：`main.js` 路由、`api.js` 会话/CSRF 客户端、`pages/` 页面、`ui/` 组件 |
| `src/public/` | 公开页：`main.js` 更新与分页、`validate.js` 投影校验、卡片和星轨图 |
| `src/shared/` | `live.js` WS/SSE/HTTP 通道、`sse.js` 有界解析器、共享设计 token、DOM 工具、图标与主题 |
| `test/` | Node.js 测试；不替代真实浏览器验收 |
| `build.mjs` | 两个入口构建、HTML 外壳和内联 SVG 图标生成 |

## 必须保持的边界

- `/` 返回 `public.html`；控制台路由返回 `200.html`。改路由时同时更新 `src/console/main.js` 与 `internal/server/http.go`，刷新直达链接也必须可用。API 契约见 [OpenAPI](../api/openapi.yaml)。
- 内容哈希脚本和样式放在 `_app/immutable/`，Server 缓存一年；HTML 为 `no-store`。不要手改产物或去掉 Go 的 `all:dist` 嵌入声明。
- 只修改 `src` 再构建，不直接编辑 `dist`。所有控制台账户均为管理员。Settings 的 Data operations 使用 Export、Backup plans、Task history 三块 panel，不增加 pathname 路由或第二套组件体系。
- Settings 页签在哈希直达和窗口缩窄后保持当前标签可见；页面导航移除监听并释放已挂载页签的请求订阅。
- 节点页保留 Details、Public page、Agent settings（`#agent`）、Enroll agent 四页签；旧 `#config` 不作兼容跳转。Agent settings 经 `runtime` API 只读展示 Agent 最近报告的本地运行参数；修改本地 JSON 后 reload，以日志和下一份报告确认生效，不提供网页修改或 reload 按钮。
- Details 的 Traffic billing 独立管理账期（1–28）与配额（十进制 Bytes/KB/MB/GB/TB，0 不限额），使用 `traffic-settings` API，不下发 Agent。保存后以响应重设该表单基线，仅重载 Traffic，不刷新整页或清除其他表单的未保存内容。
- 控制台只支持桌面和 iPad 宽度（最小 744px），不做手机布局；1200px 以下侧栏收为图标窄栏，文字保留为可访问名称并在悬停/聚焦时显示提示。公开页需适配手机、平板、桌面和大屏。
- 侧栏只有一个 Notifications 铃铛（窄栏中为角标），由单一 controller 管理。只用一个 15 秒前台 summary timer；隐藏、pagehide、退出时停止并 abort，恢复立即刷新，旧会话结果不得回写。Data 页订阅该 tick 刷新任务，detach 时解除，不另起 timer；失败保留旧计数并标为 stale，不伪装成零。铃铛计数是当前管理员的未读，不是 firing 告警数。
- 通知使用现有原生 modal，支持 All/Unread、分页和逐用户已读；详情成功展示后才标已读。保留 Escape、Tab 与关闭后的焦点恢复，来源过期仍展示安全快照；管理员可查看告警与备份结果。
- 导出受理后可跨页面运行，不自动下载，也不提供取消/重试操作。下载用普通同源附件链接，不加 `data-link`，不经 JSON 客户端或大 Blob。`/settings?task=<uuid>#data` 与 `/alerts?event=<uuid>#history` 直接读取详情，不依赖当前列表页。
- 密码仅在 Reveal、Copy、Download recovery info 操作后显式 POST 获取，不放入列表、toast、日志或初始 HTML。仅在当前组件内存短暂保存，30 秒无交互重新遮罩，关闭/导航清除；恢复凭据小 txt 在浏览器生成并由用户另存，不上传。Copy 失败提供可选文本，不使用 `execCommand`。
- 公开页使用统一 snapshot/heartbeat/close/error frame，按 WS→SSE→HTTP 自动回退；HTTP/SSE fetch 使用 `credentials: 'omit'`。浏览器 WebSocket 不能禁止自动附带同源 Cookie，公开端点始终忽略凭据并只返回匿名白名单。首份有效快照及每次 HTTP 请求限时 8 秒，WS/SSE 30 秒无有效快照/心跳即换通道；注释和未知消息不续命。HTTP 完成后按 frame 的 Server 周期等待（默认 10 秒、2–60 秒），429/503 遵守更大的 Retry-After；等待不使用流 watchdog。
- SSE/HTTP 每 300 秒关闭旧通道并重新优选 WS；刷新、切页和恢复可见也从 WS 开始。隐藏/pagehide 关闭通道及所有 timer，恢复先保留旧数据并标旧；generation 与 AbortSignal 同时拦截迟到响应。连接故障不把节点改成离线，无首份快照时显示真实加载失败；下一有效快照立即移除撤回节点。只有 HTTP 显示服务端周期倒计时，心跳只校时，不刷新采样时间或重复 ARIA 播报。
- HTTP 重试截止时间使用单调时钟，跨 300 秒优选及回退保留，不能借重建通道绕过 Retry-After；HTTP-date 优先相对响应 Date 计算。倒计时也使用单调增量，不受浏览器墙钟调整影响。
- 公开卡片只消费白名单数值、`last_seen` 与四项 `metric_status`，离线不清空卡片或最后数值；过期数字弱化并显示“最后数据 · 时间”，真正不可用显示“—”。不向匿名页面增加 runtime、原始来源或采样周期。
- Details 的 Latest sample 只读保留最后报告，错误/缺失指标不显示报告中的残留数字，也不拼补较早成功值。Sampled、Last report 及各管理页 Last seen/Last reported 共用绝对日期和秒级时间，静默心跳不会留下永久“just now”。单个页级 deadline timer 使用 `server_time` 加 `performance.now()`，超过来源新鲜度边界才标 Stale；只改标签/样式，不改数字或递增 uptime。隐藏、pagehide、页签分离、导航及会话失效停 timer，恢复重新计算；后续校时不会倒退或刷新原报告时间。
- Overview、Nodes 和节点详情各使用一条 `console/live.js` 订阅；详情所有页签共享，history/probes/traffic 的原有 timer 不重复迁入。只读事实原位更新，不写 input/select/textarea 或调用整页 refresh。状态计数只改文字并显示新出现的非零筛选，不隐藏聚焦的零计数控件；重筛由 `Apply current filters` 显式触发。`Refresh list` 保留筛选并钳制页码：暂停旧通道，失效 nodes 缓存读取 metadata，再用 `readConsoleFleet` 获取因果上更晚的完整集合，应用后重开一条通道；不凭帧到达顺序推测 HTTP 新增/删除。列表手动与批量后的重载串行，仅手动 Refresh 确认清理既有 Deleted 行；批量操作保留删除提示。失败保留旧视图并恢复订阅。删除行不可操作，只取消该行选择；批量确认目标冻结。详情删除提示保留草稿，但禁用写操作。
- `api.js` 集中 session epoch、abort 与私有 cache；同一会话校验/更名不重建页面，身份替换、退出、401、密码撤销或已知 expires_at 到期取消旧请求与通道。HTTP Date/frame 配合单调时钟到期，断网或 HTTP60 等待也清空私有页面与 dialog，并强制去登录，不能让 dirty 确认阻挡。旧 epoch 响应不回写；被替换的导航/刷新以取消结束，旧操作不能继续发私有成功提示，非等待式导航只消费预期取消。未知远端撤销须下次通信获知。
- 动画遵循 `prefers-reduced-motion`；主题偏好共用 `localStorage` 的 `orbitbeacon-theme`。字号、控件高度、z-index 使用 `shared/tokens.css` 的 token，断点约定写在该文件开头；两份深色调色板由测试保证一致。不加入外部资源或必须联网加载的运行时依赖。

## 共享实时客户端

`connectLive({urls, signal, credentials, validateSnapshot, onSnapshot, onState, onClock, onUnauthorized})` 返回 `{refresh(), close()}`；`urls` 为 `{ws, sse, http}`。`refresh` 重新优选 WS，`close`/外部 abort 为最终关闭。每份快照只校验一次，再调用 `onSnapshot(data, frame)`；`onClock(serverTime)` 只接收通过协议校验的时钟。`onState` 为 `{state: 'connecting'|'live'|'stale', transport: 'ws'|'sse'|'http', intervalSeconds, retryAt}`，`retryAt` 是毫秒 Unix 时间或 null；transport 仅供内部展示策略，ARIA 只播报 state 真正变化。

`close: renew` 立即重建同种通道，`shutdown` 标旧后退避。已知 HTTP 401/`error: unauthenticated` 终止通道，不继续降级。可选异步 `onUnauthorized('verify')` 在未成功打开的 WS 失败后、降级前调用（浏览器不暴露握手状态）；返回 false 最终停止，网络未知可允许降级。`onUnauthorized('unauthenticated')` 用于已知失效处理，不会重开通道。公开页不需要认证 hook。

`shared/sse.js` 用 fatal UTF-8 解码和 1 MiB 行/事件累计上限处理分块输入；只保留一个活动 reader，取消时释放。无运行时 UI 或协议库依赖。

`console/live.js` 的 `connectConsoleLive({nodeID, signal, onSnapshot, onState, onClock})` 返回相同控制器；省略 nodeID 为紧凑 fleet，否则为 detail。包装管理 Cookie、schema、session epoch/abort 和认证复核，页面不直接拼私有通道。恢复可见必须无条件 `/me` 成功后才创建新通道，不能让共享客户端自动恢复抢先连接；网络失败在已知期限前保留 stale。工具栏为 Live / Reconnecting… / Stale，HTTP 正常也为 Live，技术传输名不向用户展示。
