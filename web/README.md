# Web 开发

原生 JavaScript / CSS 与 esbuild，无前端运行时依赖、外部字体或 CDN。术语和文案见[约定](../docs/conventions.md)，功能入口见[主文档](../README.md#功能)。

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
- 产物在 [`internal/server/webui/dist`](../internal/server/webui/dist)，由 Go `embed` 打包；提交时同步源码与产物，不直接编辑产物。`check` 临时构建并比较，不修改已提交文件。
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
| `src/shared/` | 实时传输、SSE 解析、新鲜度、DOM、主题与设计 token |
| `src/shared/locales/` | `zh-CN` / `en` 语言目录，控制台与状态页共用 |
| `src/public/` | 状态页入口、投影校验、卡片与星轨图 |
| `test/`、`build.mjs` | 回归工具与双入口构建 |

## 开发约定

- 复用现有组件与原生 modal，保留 Escape、Tab、焦点恢复及草稿保护；动画遵守 `prefers-reduced-motion`。字号、控件及断点使用 `shared/tokens.css`。
- 可见文案、校验、日期与数字使用应用语言，不依赖浏览器原生校验文本。语言选择按浏览器偏好，其他语言回退英文；同源 `orbitbeacon-locale` 保存“跟随系统 / 中文 / English”，不写入账户。
- 切换语言确认草稿后整页重载并保留 URL；取消、保存失败或操作进行中不切换。其他标签页修改偏好不自动重载当前页。主题使用 `orbitbeacon-theme`。
- 页面复用 `shared/live.js`，私有订阅通过 `console/live.js`；实时回退、状态和会话边界见[实时传输](../docs/architecture.md#实时传输)、[安全](../docs/architecture.md#安全)，不在页面各自实现。
- 取消导航、隐藏或退出时停止请求与计时器；阻止迟到响应回写。通知摘要共用单个 controller，失败保留旧数并标记已过期。
- 保留页签哈希与详情直达：任务 `/settings?task=<uuid>#data`，告警 `/alerts?event=<uuid>#history`；详情不依赖列表当前页。
- 下载用普通同源附件链接。恢复密码仅主动请求，30 秒无交互遮罩，关闭或导航时清除，不放入列表、日志或初始 HTML；归档操作见[部署指南](../deploy/README.md#导出与备份)。
- 字段、认证、错误码与实时帧格式见[接口契约（OpenAPI）](../api/openapi.yaml)，此处不重复。
