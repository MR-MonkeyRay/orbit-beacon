# 公开状态页主题开发指南

主题仅自定义匿名状态页（`/` 与 `/public.html`）的视觉与组件呈现，**不影响登录页与管理控制台**；每次构建选择一个主题编入二进制，明暗模式切换继续保留。

---

## 1. 修改层级决策树

```text
你想修改什么?
 ├── 仅改配色 / 圆角 / 间距 ──▶ 复制 example/ 改 public.css token (保留默认 CSS 导入)
 ├── 改页面布局 / 品牌 / 结构 ──▶ 修改自定义主题 index.html + public.css (须保留必需 DOM ID 与占位符)
 └── 改卡片或星轨图组件     ──▶ 复制 cards.js / constellation.js 修改 (须遵守呈现函数契约与撤回逻辑)
```

> [!IMPORTANT]
> 所有层级均复用 `src/public/page.js`（`mountPublicPage`）负责网络回退、校验、分页与生命周期，**严禁在主题中复制网络轮询逻辑**。

---

## 2. 快速新建与构建验证

```sh
cp -R web/themes/example web/themes/custom
# 编辑 web/build.config.json: {"publicTheme":"custom","assetBase":""}
# 修改 web/themes/custom/public.css 后执行构建与 Go 重编译:
npm --prefix web run build && npm --prefix web test && npm --prefix web run check
go build -o build/orbitbeacon-server ./cmd/orbitbeacon-server
```

重启 `orbitbeacon-server` 后访问 `/` 查看效果（`npm run watch` 不会更新已运行的 Go 二进制；控制台 `设置 → 状态页` 须开启且至少存在一个已发布节点）。

---

## 3. 主题文件与运行时挂载契约

每个 `themes/<name>/` 至少包含 `index.html`、`main.js` 及样式/静态素材：

```text
index.html (含 4 个必选构建占位符)           main.js (挂载入口)
 ├── <!--theme-->      (首绘前防闪白)  ──▶  mountPublicPage({
 ├── %STYLE%           (构建样式 URL)         createCard,
 ├── %SCRIPT%          (构建脚本 URL)         skeletonCards,
 └── <!--icons-->      (SVG sprite)           createConstellation,
                                              createRegionHeading? // 可选分组/地区标题
                                            })
```

### 3.1 必须保留的 DOM ID

重排结构或增删容器均可，但下列唯一 ID 由共享运行时直接写入（不可替换成仅展示副本）：

| 区域 | 必需 ID 与约束 |
| --- | --- |
| **顶部与状态摘要** | `top-actions`（插入语言切换）、`theme`（按钮）、`sign-in`（可选，已登录时替换为 `.account-chip`）、`locale-error`（初始 `hidden`, `role="alert"`）、`headline`（`h1`）、`summary`（`data-tone`）、`updated`、`countdown`（`aria-hidden="true"`）、`refresh`（按钮） |
| **图形与筛选工具** | `constellation`（`aria-hidden="true"`）、`filters`（初始 `hidden`, `role="group"`；运行时在后插入 `.node-search`）、`services`（可选公开服务占位） |
| **列表、空态与分页** | `announcement`（`role="status"`, `.sr`）、`empty`、`empty-text`、`retry`、`nodes`（`ul`）、`pagination`、`previous`、`next`、`page` |

### 3.2 呈现函数接口

- `createCard()` → 返回 `li`，暴露 `update(node)` 与 `setStale()`（`setStale` 仅标注陈旧，不得伪造离线或清空旧指标；缺失值显示为不可用，不得显示为 `0`）。
- `skeletonCards(count)` → 返回 `.placeholder` 占位 `li` 数组（对辅助技术隐藏）。
- `createConstellation(container)` → 返回 `{ update(nodes), clear() }`（`clear()` 须彻底清除节点信息）。
- `createRegionHeading(title)`（可选）→ 当前页超 12 个节点按地区插入，或开启“按分组显示”且有分组时按分组插入（未分组归入“其他”）。

### 3.3 素材引用规则

图片与字体必须通过 JS `import` 或 CSS `url('./assets/...')` 引入，使 esbuild 自动版本化与打包；**不要在 HTML 中手写相对资源路径或 `/_app/` 地址**（内置 `data:` favicon 除外）。

---

## 4. 安全、CSP 与验收清单

- **无凭据与 XSS 防护**：状态页请求不带认证凭据（仅有 `orbitbeacon.signed-in` 标记时运行时会轻查 `/api/v1/me` 显示头像）；节点名/地区/提供商严禁拼入 `innerHTML`，必须使用 `textContent` 或 `h()`。
- **CSP 合规**：禁止外链第三方脚本/字体、内联事件或 `eval`；API/WS/SSE 始终访问原站，不得从静态 CDN 读取数据。
- **实时撤回要求**：收到 `public_page_disabled` 时必须清空全部卡片/图形缓存并重定向 `/`；新快照缺少某节点时立即移除其 DOM 与图形表现；普通断线保留最后一次有效快照并标记 `stale`。

<details>
<summary>发布前验收 Checklist</summary>

- [ ] `npm --prefix web test && npm --prefix web run check` 通过，且浏览器控制台无 CSP / 资源 / 运行时错误
- [ ] 默认主题样式未受影响，自定义主题不影响登录页与控制台
- [ ] 中英文、浅/深/跟随系统主题、窄屏手机/平板/桌面、`prefers-reduced-motion` 及键盘无障碍均正常
- [ ] 在线/离线/未知、缺失/过期指标、空集/筛选、WS→SSE→HTTP 回退、取消公开节点与全局关闭状态页均正确撤回

</details>
