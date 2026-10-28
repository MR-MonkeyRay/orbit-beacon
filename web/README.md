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

## 源码入口

| 路径 | 用途 |
| --- | --- |
| `src/console/` | 管理控制台：`main.js` 路由、`api.js` 会话/CSRF 客户端、`pages/` 页面、`ui/` 组件 |
| `src/public/` | 公开页：`main.js` 更新与分页、`stream.js` SSE、`validate.js` 响应校验、卡片和星轨图 |
| `src/shared/` | 共享设计 token、DOM 工具、图标与主题 |
| `test/` | Node.js 测试；不替代真实浏览器验收 |
| `build.mjs` | 两个入口构建、HTML 外壳和内联 SVG 图标生成 |

## 必须保持的边界

- `/` 返回 `public.html`；控制台路由返回 `200.html`。改路由时同时更新 `src/console/main.js` 与 `internal/server/http.go`，刷新直达链接也必须可用。API 契约见 [OpenAPI](../api/openapi.yaml)。
- 内容哈希脚本和样式放在 `_app/immutable/`，Server 缓存一年；HTML 为 `no-store`。不要手改产物或去掉 Go 的 `all:dist` 嵌入声明。
- 只修改 `src` 再构建，不直接编辑 `dist`。所有控制台账户均为管理员。Settings 的 Data operations 使用 Export、Backup plans、Task history 三块 panel，不增加 pathname 路由或第二套组件体系。
- Settings 页签在哈希直达和窗口缩窄后保持当前标签可见；页面导航移除监听并释放已挂载页签的请求订阅。
- 节点页保留 Details、Public page、Agent settings（`#agent`）、Enroll agent 四页签；旧 `#config` 不作兼容跳转。Agent settings 经 `runtime` API 只读展示 Agent 最近报告的本地运行参数；修改本地 JSON 后 reload，以日志和下一份报告确认生效，不提供网页修改或 reload 按钮。
- Details 的 Traffic billing 独立管理账期（1–28）与配额（十进制 Bytes/KB/MB/GB/TB，0 不限额），使用 `traffic-settings` API，不下发 Agent。保存后以响应重设该表单基线，仅重载 Traffic，不刷新整页或清除其他表单的未保存内容。
- 桌面侧栏与移动顶栏都提供 Notifications 铃铛，由单一 controller 管理。只用一个 15 秒前台 summary timer；隐藏、pagehide、退出时停止并 abort，恢复立即刷新，旧会话结果不得回写。Data 页订阅该 tick 刷新任务，detach 时解除，不另起 timer；失败保留旧计数并标为 stale，不伪装成零。铃铛计数是当前管理员的未读，不是 firing 告警数。
- 通知使用现有原生 modal，支持 All/Unread、分页和逐用户已读；详情成功展示后才标已读。保留 Escape、Tab 与关闭后的焦点恢复，来源过期仍展示安全快照；管理员可查看告警与备份结果。
- 导出受理后可跨页面运行，不自动下载，也不提供取消/重试操作。下载用普通同源附件链接，不加 `data-link`，不经 JSON 客户端或大 Blob。`/settings?task=<uuid>#data` 与 `/alerts?event=<uuid>#history` 直接读取详情，不依赖当前列表页。
- 密码仅在 Reveal、Copy、Download recovery info 操作后显式 POST 获取，不放入列表、toast、日志或初始 HTML。仅在当前组件内存短暂保存，30 秒无交互重新遮罩，关闭/导航清除；恢复凭据小 txt 在浏览器生成并由用户另存，不上传。Copy 失败提供可选文本，不使用 `execCommand`。
- 公开页请求不带凭据，优先 SSE；建流失败降级为 10 秒轮询。建流首帧及普通请求超时为 8 秒，收到流帧后以 18 秒检查中断。失败或隐藏时保留最后有效集合并标旧，不据此把节点改成离线；无首份快照时显示真实加载失败。恢复后的有效快照立即移除已撤回节点。
- 公开卡片只消费白名单数值、`last_seen` 与四项 `metric_status`，离线不清空卡片或最后数值；过期数字弱化并显示“最后数据 · 时间”，真正不可用显示“—”。不向匿名页面增加 runtime、原始来源或采样周期。
- Details 的 Latest sample 只读保留最后报告，错误/缺失指标不显示报告中的残留数字，也不拼补较早成功值。Sampled 与 Last report 使用绝对时间，避免静默时保留“just now”文案。单个页级 deadline timer 使用 `server_time` 加 `performance.now()`，超过来源新鲜度边界才标 Stale；只改标签/样式，不改数字或递增 uptime。隐藏、pagehide、页签分离、导航及会话失效停 timer，恢复重新计算；后续校时不会倒退或刷新原报告时间。
- 动画遵循 `prefers-reduced-motion`；主题偏好共用 `localStorage` 的 `orbitbeacon-theme`。不加入外部资源或必须联网加载的运行时依赖。
