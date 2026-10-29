# Web 开发

管理控制台与公开节点页均支持简体中文和英文，使用原生 JavaScript/CSS 和 esbuild，无前端运行时依赖、外部字体或 CDN。产物提交在 `internal/server/webui/dist`，由 Go `embed` 打包；普通 Go 构建和 Server 运行不需要 Node.js。

## 构建与检查

需要 Node.js 22+，仓库版本见 [.nvmrc](.nvmrc)。在仓库根目录执行：

```sh
cd web
npm ci
npm run build   # 更新内嵌产物
# 或 npm run watch：监听源码并重建
npm run check   # 临时构建并比较 dist，不修改已提交产物
npm test
```

没有独立开发服务器。重建网页后还需重新编译并重启 Server，启动方式见[部署文档](../deploy/README.md)。提交时同时提交源码与产物，不直接修改 `dist`。

CI 执行产物一致性检查和 Node.js 测试。构建目标为 Chrome 111、Firefox 115、Safari 16.4，不代表已完成这些浏览器的实测；界面变更仍需真实浏览器验收。异步测试使用 `test/console-dom.js` 的 `flush()`，模拟计时器另行推进。

## 源码入口

| 路径 | 用途 |
| --- | --- |
| `src/console/` | `main.js` 路由、`api.js` 会话与 CSRF、`pages/` 页面、`ui/` 组件 |
| `src/public/` | `main.js` 更新与分页、`validate.js` 白名单投影校验、卡片与星轨图 |
| `src/shared/` | `live.js` 实时通道、`sse.js` 有界解析器、`freshness.js`、设计 token、DOM 工具与主题 |
| `test/` | Node.js 回归与 DOM 测试工具 |
| `build.mjs` | 双入口构建、HTML 外壳和内联 SVG 图标生成 |

## 页面与交互约束

- 默认按浏览器语言顺序选择中文或英文，其他语言回退英文。公开页顶栏、登录/改密卡片及控制台侧栏提供“跟随系统 / 中文 / English”原生选择框；同源页面共享浏览器本地 `orbitbeacon-locale` 偏好，不写入账户。切换保存后整页重载，保留路径、查询和哈希；有未保存表单或对话框时只确认一次，取消或存储写入失败保留当前内容，操作进行中不能切换。
- 所有可见错误、校验、日期和数字遵循应用所选语言，不依赖浏览器原生校验文案。用户输入、标识符、URL、命令和协议键不翻译；BigInt 流量精度、百分比与时间范围语义保持不变。
- 已打开页面保持当前语言至重载，避免其他标签页修改偏好后混用旧导航、日期与新文案；不会自动重载并丢弃草稿。

- 公开页默认开启，`/` 与 `/public.html` 都返回公开 HTML，不因登录状态跳转。Settings → Public page 可关闭全站公开入口、匿名 API 与实时流；关闭后正常会话进入 `/overview`，待改密会话进入 `/change-password`，匿名访问进入 `/login`。控制台前往这两个公开入口使用整页导航，保留脏表单确认。静态资源不受此开关限制；启用不会自动公开私有节点。配置备份包含该开关，仅还原 Monitoring / Events 保留当前设置。
- 控制台最小宽度 744px，面向桌面和平板；1200px 以下使用图标窄侧栏，保留可访问名称与悬停/聚焦提示。公开页适配手机到大屏。
- `/login` 与 `/change-password` 共享无侧栏登录卡片。初始化管理员必须改密；新增管理员可勾选首次登录改密，默认不勾选。受限会话仅保留改密与退出操作，不加载管理数据、通知或私有实时通道；公开页仍可匿名浏览。
- 复用现有组件与原生 modal，保留 Escape、Tab、焦点恢复和脏表单保护。动画遵循 `prefers-reduced-motion`；主题键为 `orbitbeacon-theme`。字号、控件高度、层级使用 `shared/tokens.css`，断点约定见文件开头。
- 节点页保留 Details、Public page、Agent settings（`#agent`）、Enroll agent。runtime 只读；Traffic billing 独立保存 Server 账期与配额，不下发 Agent，不刷新整页或清除其他草稿。
- Settings 页签需支持哈希直达和窄窗口；末尾 Public page（`#public-page`）开关即时保存，关闭前确认，失败恢复已知服务器状态。Data operations 复用 Export、Backup plans、Task history 三块 panel。任务详情 `/settings?task=<uuid>#data`、告警详情 `/alerts?event=<uuid>#history` 不依赖列表当前页。
- 侧栏只有一个 Notifications controller，以单个 15 秒前台摘要轮询服务铃铛与 Data 页；隐藏、退出时停止并 abort，恢复立即刷新。失败保留旧数并标旧，不显示假零；未读数不是当前触发告警数。
- 下载使用普通同源附件链接，不经路由或 JSON/Blob 客户端；导出受理后不自动下载。归档密码仅在 Reveal / Copy / Download recovery info 时显式 POST 获取，不进入列表、日志或初始 HTML；30 秒无交互遮罩，关闭/导航清除，复制失败提供可选文本。

功能语义与操作入口见[主 README](../README.md)，数据保留和安全边界见[架构文档](../docs/architecture.md)，接口字段以 [OpenAPI](../api/openapi.yaml) 为准。

## 实时更新与会话

公开页和控制台共用 WS → SSE → HTTP 状态机：首份快照/HTTP 请求限时 8 秒，流 30 秒无有效快照或心跳则回退，SSE/HTTP 每 300 秒重新优选 WS。HTTP 按 Server 周期等待，429/503 遵守更大的 `Retry-After`；重建通道不能绕过该期限。重试、新鲜度和会话期限使用 Server 时钟与单调增量，不能依赖浏览器墙钟。

- 隐藏或 pagehide 关闭通道和 timer，generation 与 AbortSignal 阻止迟到回写。公开 fetch 使用 `credentials: 'omit'`；浏览器 WS 可能携带 Cookie，公开服务端始终忽略凭据。
- 心跳只校时，不更新采样时间、数字或 uptime。断网保留旧数据并标旧，不推断节点离线；新快照移除已撤回公开节点，失败/缺失指标不拼补旧成功值。
- 明确收到 `public_page_disabled` 错误帧，或 HTTP/SSE setup 的 403 JSON 同名 code 后，先永久关闭通道和计时器，再清空公开节点并 `location.replace('/')`，由服务端按会话分流。一般 403、5xx、断网、错误或超限 JSON 继续原回退/重试，不冒充关闭；错误体最多读取 64 KiB，遵守通道取消与超时。
- Overview、Nodes、节点详情各只有一条私有订阅，详情页签共享。实时补丁只改只读状态、样本与 runtime，不替换表单、历史/探测/流量容器或已确认批量目标。
- 列表保留筛选和勾选，重筛由 `Apply current filters` 触发；手动 `Refresh list` 先暂停旧通道，读取 metadata 后再读 HTTP fleet，按更晚集合收敛成员，再恢复订阅。不能用响应到达顺序推断新增/删除。删除行保留提示并禁用操作；详情删除保留草稿。
- `api.js` 统一管理 session epoch、abort 和私有缓存。退出、身份替换、401、密码撤销或已知到期清空私有 DOM 与 dialog，绕过脏表单确认转登录；旧响应不得回写。恢复可见先无缓存 `/me` 成功，再重开私有通道；离线不能提前获知远端撤销。
- `must_change_password` 必须是会话 user 中的布尔值。收到 `password_change_required` 时保留 CSRF/会话凭据但切换 epoch，清空私有 UI/缓存并进入 `/change-password`；迟到 `/me` 不能解除限制，只有改密后重新登录才恢复管理权限。改密成功清空密码输入、退出全部会话并前往 `/login?password-changed`。

### 客户端接口

`shared/live.js`：

```js
connectLive({
  urls, // {ws, sse, http}
  signal, credentials, validateSnapshot,
  onSnapshot, onState, onClock, onUnauthorized, onPublicPageDisabled,
}); // {refresh(), close()}
```

`onSnapshot(data, frame)` 接收校验后的数据；`onClock(serverTime)` 只接收有效协议时钟。`onState` 为 `{state, transport, intervalSeconds, retryAt}`：state 为 `connecting | live | stale`，retryAt 为毫秒 Unix 时间或 null。界面只展示 Live / Reconnecting… / Stale，不展示技术通道名；ARIA 仅播报状态变化。

`refresh()` 重新优选 WS，`close()` 或外部 abort 最终关闭。`close: renew` 重建同种通道，`shutdown` 标旧后退避。已知 401 / `unauthenticated` 终止，不继续降级；WS 握手失败可通过异步 `onUnauthorized('verify')` 复核，返回 false 停止，网络未知可降级。

`onPublicPageDisabled()` 只在精确禁用控制码下调用一次；调用前已关闭所有传输、请求与计时器，回调异常也不能重启轮询。`onUnauthorized` 仍只处理私有认证失败。

`console/live.js` 的 `connectConsoleLive({nodeID, signal, onSnapshot, onState, onClock})` 包装私有认证、schema 与会话隔离；省略 nodeID 读取 fleet，否则读取 detail。页面不自行拼私有通道。

`shared/sse.js` 使用 fatal UTF-8 解码，行/事件累计上限 1 MiB；取消时释放唯一活动 reader。
