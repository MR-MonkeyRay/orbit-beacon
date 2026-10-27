# Web UI 源码

管理控制台（英文）与公开节点首页（中文）的源码。构建产物提交在 `internal/server/webui/dist`，由 Go `embed` 打包进 Server，所以编译 Go 不需要 Node.js；只有修改界面时才需要。

```sh
cd web
npm ci
npm run build   # 生成 dist；提交前运行
npm run watch   # 开发时自动重建（需重新编译并重启 Server 才会生效）
npm run check   # 校验 dist 与源码一致，CI 会运行
```

## 结构

```
src/
  shared/        两个页面共用：设计 token（tokens.css）、DOM 工具、图标、主题切换
  console/       管理控制台，单页应用
    main.js        路由表（须与 Go 服务端返回 200.html 的路径一致）
    router.js      History 路由：先取数据再切换，慢时显示骨架屏，支持 View Transitions
    api.js         /api/v1 客户端、会话、CSRF、短时请求缓存
    shell.js       侧栏、账户区、移动端抽屉
    ui/            组件、表单（UTF-8 字节校验、脏状态）、对话框、Tabs、Toast
    pages/         各页面
    styles/        按关注点拆分的样式，responsive.css 必须最后导入
  public/        公开首页：轮询、卡片原地更新、星轨图
```

## 约定

- 不引入运行时依赖和外部资源（字体、CDN），页面可在离线内网使用。
- 脚本和样式输出到 `_app/immutable/` 并带内容哈希，服务端对该目录返回一年 `immutable` 缓存；HTML 外壳为 `no-store`。
- 图标在构建时以 SVG sprite 内联进 HTML，首屏无需等待脚本。
- 公开首页的数据处理语义（8 秒超时、严格校验响应、隐藏或失败时清空数据）属于安全约定，修改时保持不变。
- 动画遵循 `prefers-reduced-motion`；主题偏好保存在 `localStorage` 的 `orbitbeacon-theme`，两个页面共享。
