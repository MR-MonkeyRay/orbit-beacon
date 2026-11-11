# Web 开发

前端用原生 JavaScript/CSS 和 esbuild，没有运行时依赖或外部字体；默认资源同源，也支持自托管静态 CDN。术语和文案见[约定](../docs/conventions.md)，功能入口见[主文档](../README.md#功能)。

## 开发流程

需要 Node.js 22+，CI 使用的当前 LTS 版本见 [.nvmrc](.nvmrc)（26.13.0）。从仓库根目录执行：

```sh
cd web
npm ci
npm run build
# 也可用 npm run watch 在源码变更时自动重新构建。
npm run check
npm test
```

- 无独立开发服务器；构建后重新编译并重启服务器，见[快速开始](../README.md#快速开始)。
- 产物在 [`internal/server/webui/dist`](../internal/server/webui/dist)，由 Go `embed` 打包；提交时同步源码与产物，不直接编辑产物。文件名固定，构建器自动更新 URL 的内容版本与 HTML 引用，不要手动改文件名。`check` 临时构建并比较，不修改已提交文件。
- CI 检查产物一致性与 Node.js 测试。构建目标为 Chrome 111、Firefox 115、Safari 16.4，仍需真实浏览器验收。
- DOM 异步测试用 `test/console-dom.js` 的 `flush()`，模拟计时器另行推进。

## 目录

| 路径 | 职责 |
| --- | --- |
| `src/console/main.js` | 声明路由表，初始化 shell 与 router |
| `src/console/router.js` | History API 导航、页面加载、会话门禁与草稿确认 |
| `src/console/pages/` | 页面及分区 |
| `src/console/ui/` | 表单、对话框、图表等复用组件 |
| `src/console/lib/` | 节点列表、批量操作、告警与注册等辅助逻辑 |
| `src/console/api.js`、`live.js`、`notifications.js` | 会话/CSRF、私有实时订阅、站内通知 controller |
| `src/shared/` | 实时传输、SSE 解析、新鲜度、DOM、明暗模式与设计 token |
| `src/shared/locales/` | `zh-CN` / `en` 语言目录，控制台与状态页共用 |
| `src/public/` | 公开数据校验、状态页生命周期与实时更新 |
| `themes/default/`、`themes/example/` | 仅状态页使用的 HTML、CSS 与展示组件 |
| `themes/README.md` | 自定义首页主题开发指南 |
| `build.config.json` | 首页主题和静态资源 CDN 地址 |
| `test/`、`build.mjs` | 回归工具与双入口构建 |

状态页公告面板位于 `src/console/pages/settings-announcements.js`，公开渲染由所选主题的公告渲染器提供；显示窗口和数量规则见[公告](../docs/architecture.md#公告)。

## 构建与缓存

编辑 [`build.config.json`](build.config.json)，然后执行 `npm run build`：

```json
{
  "publicTheme": "default",
  "assetBase": ""
}
```

- `publicTheme` 指定 `themes/` 下的目录名。切换为 `example` 可查看示例；复制与开发步骤见[主题指南](themes/README.md)。主题只影响 `/` 和 `/public.html`，不改变控制台。
- `assetBase` 为空时使用同源资源；独立 CDN 可设为 `https://static.example.com/orbitbeacon/release-2026-11`。只允许 ASCII HTTPS 源与可选路径前缀，不接受凭据、查询、片段、百分号编码、需自动转义的字符或 `..`。
- 构建器生成固定的 `_app/assets/{console,public}.{js,css}`，并为 HTML 引用自动添加 `?v=<内容版本>`。图片、字体应从 JS 或 CSS 导入；构建器保留稳定路径并为引用添加版本。不要直接引用未版本化的图片路径，也不要在 HTML 中手写产物文件名。
- `asset-manifest.json` 把每个资源路径与版本绑定。服务器启动时校验清单，只有匹配版本的资源才缓存一年；未带版本的资源重新验证，错误版本返回不可缓存的 404。HTML 与 API 不缓存。不要分开部署清单、HTML 与资源。
- `watch` 监听源码、主题与构建配置。产物先完整写入暂存目录，再替换 `dist`；写入失败不影响旧产物，切换失败回滚，切换中断后的下一次构建会恢复 `dist.previous`。Go 仍内嵌构建时的文件，重新构建网页不会热更新已运行的二进制。

CDN 上传 `internal/server/webui/dist/_app/assets/`，保持目录层级，并把 `v` 纳入缓存键。API、WS、SSE、导航和认证仍访问网站本身，不能把 `public_url` 改成静态 CDN。部署顺序、旧版本保留及缓存规则见[CDN 部署](../deploy/README.md#cdn)。

## 开发约定

- 复用现有组件与原生 modal，保留 Escape、Tab、焦点恢复及草稿保护；动画遵守 `prefers-reduced-motion`。字号、控件及断点使用 `shared/tokens.css`。
- 控制台在 `max-width:767px` 使用顶部栏与抽屉导航，节点表使用同一 DOM 显示为卡片列表，其他宽表格横向滚动；公开页覆盖所有设备。响应式规则见 `console/styles/responsive.css`。
- 可见文案、校验、日期与数字使用应用语言，不依赖浏览器原生校验文本。语言选择按浏览器偏好，其他语言回退英文；同源 `orbitbeacon-locale` 保存“跟随系统 / 中文 / English”，不写入账户。
- 切换语言确认草稿后整页重载并保留 URL；取消、保存失败或操作进行中不切换。其他标签页修改偏好不自动重载当前页。主题使用 `orbitbeacon-theme`。
- 页面复用 `shared/live.js`，私有订阅通过 `console/live.js`；实时回退、状态和会话边界见[实时传输](../docs/architecture.md#实时传输)、[安全](../docs/architecture.md#安全)，不在页面各自实现。
- 取消导航、隐藏或退出时停止请求与计时器；阻止迟到响应回写。通知摘要共用单个 controller，失败保留旧数并标记已过期。
- 保留页签哈希与详情直达：任务 `/settings?task=<uuid>#data`，告警 `/alerts?event=<uuid>#history`；详情不依赖列表当前页。
- 下载用普通同源附件链接。恢复密码仅主动请求，30 秒无交互遮罩，关闭或导航时清除，不放入列表、日志或初始 HTML；归档操作见[部署指南](../deploy/README.md#导出与备份)。
- 字段、认证、错误码与实时帧格式见[接口契约（OpenAPI）](../api/openapi.yaml)，此处不重复。
