# Web 前端开发指南

OrbitBeacon 前端采用 **原生 JS / CSS + esbuild**，零运行时第三方依赖或外链字体；构建产物提交于仓库并通过 Go `go:embed` 打入单一服务端二进制。

- 状态页自定义主题开发：[themes/README.md](themes/README.md)
- 术语、文案与无障碍规范：[docs/conventions.md](../docs/conventions.md)
- HTTP / WS / SSE 接口契约：[api/openapi.yaml](../api/openapi.yaml)

---

## 1. 双入口构建流水线

```text
web/src/console/  (管理控制台源码) ──┐
web/src/public/   (公开页运行时)     ├──▶ [esbuild (web/build.mjs)] ──▶ internal/server/webui/dist/
web/src/shared/   (公共组件/传输/i18n)│        (受 build.config.json 控制)    ├── 200.html / public.html
web/themes/<name>/(状态页主题)       ──┘                                    ├── _app/assets/*.{js,css}?v=<hash>
                                                                           └── asset-manifest.json
                                                                                      │
                                                                           go:embed 打包进 orbitbeacon-server
```

- **版本一致性**：`internal/server/webui/dist` 受 Git 跟踪，提交源码修改须同步提交构建产物（勿手改 `dist`）。
- **缓存模型**：HTML 与 API 永不缓存；服务器启动校验 `asset-manifest.json`，仅版本查询串 `?v=...` 完全匹配的资源返回一年 `immutable` 缓存，错误版本返回不可缓存的 404。

---

## 2. 开发与测试命令

需要 Node.js 22+（CI 版本见 [.nvmrc](.nvmrc)）。在仓库根目录或 `web/` 下执行：

```sh
cd web
npm ci
npm run build   # 构建产物至 internal/server/webui/dist
npm run watch   # 监听源码与主题变化自动重构建 (需重启 Go 进程才生效)
npm run check   # 临时构建并比对已提交 dist 是否一致 (CI 必检)
npm test        # 运行前端单测与 DOM 回归
```

---

## 3. 目录职责

| 路径 | 职责 |
| --- | --- |
| `src/console/main.js` / `router.js` | 控制台路由声明、History 导航、会话门禁与草稿离开确认 |
| `src/console/pages/` | 各业务页面（概览、节点、服务、告警、审计、设置等） |
| `src/console/ui/` & `lib/` | Modal、图表、分页、表单、批处理与注册交互辅助组件 |
| `src/console/api.js` / `live.js` / `notifications.js` | 会话/CSRF 请求封装、私有实时流与 15s 通知摘要 controller |
| `src/shared/` | WS→SSE→HTTP 自动回退、新鲜度判定、DOM `h()`、明暗主题与 `tokens.css` |
| `src/shared/locales/` | `zh-CN` / `en` 多语言词条（控制台与状态页按需共享） |
| `src/public/` | 公开快照校验、只读页面生命周期与状态渲染 |
| `themes/{default,example}/` | 状态页专属 HTML/CSS/组件（只影响 `/` 与 `/public.html`） |

---

## 4. 构建配置与核心开发约定

### 4.1 主题与 CDN 配置（`web/build.config.json`）

```json
{
  "publicTheme": "default",
  "assetBase": ""
}
```

- `publicTheme`：指定 `themes/` 下的目录名（可切换 `example` 预览示例）。
- `assetBase`：留空使用同源静态资源；启用独立静态 CDN 时填 ASCII HTTPS 根地址（如 `https://static.example.com/orbitbeacon/release-2026-11`，不含凭据/查询/百分号编码）。静态资源必须**先于**新版二进制上线。

### 4.2 代码与交互红线

- **复用意图**：复用 `src/console/ui/` 与原生 modal，保留 `Escape` / `Tab` / 焦点恢复及 `prefers-reduced-motion`；响应式断点与字号使用 `shared/tokens.css` 与 `console/styles/responsive.css`（`<=767px` 切为抽屉导航与卡片列表）。
- **实时与生命周期**：复用 `shared/live.js` / `console/live.js`；取消导航、页面隐藏或登出时须中止在途请求与计时器，禁止迟到响应回写 DOM。
- **路由与直达**：保留 `/settings#data-storage`（兼容 `/data` 跳转）、`/settings#api-tokens`、`/settings?task=<uuid>#data`、`/alerts?event=<uuid>#history` 及节点设置分段哈希（`#settings-{details,public,agent,enrollment}`）。
- **隐私与归档**：恢复密码仅主动点击时展示，30s 无操作自动遮罩，关闭或切换路由立即清除内存副本，绝不写入列表或日志。
