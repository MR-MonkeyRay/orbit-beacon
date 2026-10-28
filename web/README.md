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
- 只修改 `src` 再构建，不直接编辑 `dist`。Settings 的管理员专属 Data operations 使用 Export、Backup plans、Task history 三块 panel，不增加 pathname 路由或第二套组件体系；viewer 直达 `#data` 显示拒绝访问且不请求备份 API。
- 桌面侧栏与移动顶栏都提供 Notifications 铃铛，admin/viewer 均可见，由单一 controller 管理。只用一个 15 秒前台 summary timer；隐藏、pagehide、退出时停止并 abort，恢复立即刷新，旧会话结果不得回写。Data 页订阅该 tick 刷新任务，detach 时解除，不另起 timer；失败保留旧计数并标为 stale，不伪装成零。铃铛计数是本人可见未读，不是 firing 告警数。
- 通知使用现有原生 modal，支持 All/Unread、分页和逐用户已读；详情成功展示后才标已读。保留 Escape、Tab 与关闭后的焦点恢复，来源过期仍展示安全快照；备份结果仅管理员可见，viewer 仍能查看并标记告警。
- 导出受理后可跨页面运行，不自动下载，也不提供取消/重试操作。下载用普通同源附件链接，不加 `data-link`，不经 JSON 客户端或大 Blob。`/settings?task=<uuid>#data` 与 `/alerts?event=<uuid>#history` 直接读取详情，不依赖当前列表页。
- 密码仅在 Reveal、Copy、Download recovery info 操作后显式 POST 获取，不放入列表、toast、日志或初始 HTML。仅在当前组件内存短暂保存，30 秒无交互重新遮罩，关闭/导航清除；恢复凭据小 txt 在浏览器生成并由用户另存，不上传。Copy 失败提供可选文本，不使用 `execCommand`。
- 公开页请求不带凭据，优先 SSE；建流失败降级为 10 秒轮询。建流首帧及普通请求超时为 8 秒，收到流帧后以 18 秒检查中断。响应严格校验；页面隐藏、离开或更新失败时清空节点数据，避免继续展示旧公开信息。
- 动画遵循 `prefers-reduced-motion`；主题偏好共用 `localStorage` 的 `orbitbeacon-theme`。不加入外部资源或必须联网加载的运行时依赖。
