# 状态页主题开发

主题只改变匿名状态页 `/` 的呈现，不改变登录页或控制台。`default` 保留现有外观；`example` 使用暖白／深绿配色、侧栏概览和更方正的节点卡片，包含完整的公告、筛选、分页、语言和主题切换。

## 从示例开始

以下命令在仓库根目录执行，环境与 [Web 开发](../README.md) 相同：

```sh
cp -R web/themes/example web/themes/custom
```

编辑 `web/build.config.json`，把 `publicTheme` 改为 `custom`，保留现有的 `assetBase`。未启用 CDN 时完整配置如下：

```json
{"publicTheme":"custom","assetBase":""}
```

先修改 `web/themes/custom/public.css` 的配色或布局，再构建：

```sh
npm --prefix web ci
npm --prefix web run build
npm --prefix web test
npm --prefix web run check
mkdir -p build
go build -o build/orbitbeacon-server ./cmd/orbitbeacon-server
```

用新二进制重启**现有**服务器，保持原启动参数和数据目录。Go `embed` 在编译时收录产物，只跑前端构建或刷新浏览器不会更新运行中的服务器。访问 `/`，不要直接打开源 HTML。

控制台“设置 → 状态页”必须开启，并且至少有一个已注册、启用、显式发布的节点才会出现卡片；没有节点就显示真实空集合，不要在主题里塞假节点。切回默认主题：把 `publicTheme` 改回 `default`，重新构建、编译、重启。

`npm --prefix web run watch` 只重建静态产物，不会重启 Go 服务器。一个构建只选一个状态页主题，不提供浏览器内主题选择器；页面明暗切换仍然可用。

## 文件与职责

每个 `themes/<名称>/` 至少提供：

| 文件 | 契约 |
| --- | --- |
| `index.html` | 完整 HTML 文档，包含下面四个构建占位符和运行时需要的 DOM |
| `main.js` | 固定路径导入 CSS、呈现组件与共享运行时，页面中只挂载一次 |
| CSS、图片等 | 经 `main.js` 或 CSS 导入，由构建器收集 |

HTML 中保留以下占位符，各一次：

```html
<!--theme-->
<link rel="stylesheet" href="%STYLE%">
<script type="module" src="%SCRIPT%"></script>
```

`<!--theme-->` 放在样式之前，用于首次绘制前应用明暗偏好；`<!--icons-->` 放在 `<body>` 内，用于注入图标 sprite。不要手写它们生成的代码、产物路径或版本号。

共享边界：

- `src/public/page.js`：`mountPublicPage(...)` 管理公开 API、校验、传输回退、分页、筛选、连接状态和可见性生命周期；只导入固定的公共源码，不依赖所选主题。
- `src/public/validate.js`：公开快照校验，保持在呈现组件之前。
- `themes/default/`：原有 HTML、CSS、节点卡片、星轨图与公告呈现。
- `themes/default/announcements.js`：公告渲染；输入已按窗口过滤，按 critical、warning、info 及开始时间排序，最多 5 条纯文本。
- `themes/example/`：独立 HTML/CSS/入口，复用默认卡片、星轨图与公告组件；复制后仍可使用这些固定的相对导入。
- `src/shared/`：DOM、实时通道、明暗与语言控件、格式化和公开词条；不要导入 `src/console/`、私有 API 客户端或控制台设置代码。

### 共享运行时所需 DOM

两个内置主题都使用同一组 ID。重排或添加容器可以，ID 必须唯一且保留；不要把运行时会写入的节点换成仅展示副本。

| ID | 元素与用途 |
| --- | --- |
| `top-actions` | 语言选择器插入的容器，保留其他操作 |
| `theme` | `button type="button"`，明暗切换 |
| `locale-error` | 初始 `hidden` 的错误区域，保留 `role="alert"` |
| `headline`、`summary` | 主标题使用 `h1`；摘要仅显示本页范围或临时连接状态，完整集合正常时为空，不重复统计 |
| `updated` | 更新时间与连接状态文字；其子节点由运行时创建 |
| `countdown` | 装饰性刷新指示器，保留 `aria-hidden="true"` |
| `refresh` | `button type="button"`，刷新当前页 |
| `constellation` | 图形容器，保留 `aria-hidden="true"` |
| `announcements` | 初始 `hidden` 的公告区域，保留翻译后的无障碍名称 |
| `filters` | 初始 `hidden` 的筛选容器，保留 `role="group"` 与无障碍名称 |
| `announcement` | 屏幕阅读器状态文字，保留 `role="status"`、`aria-live="polite"` 与 `.sr` |
| `empty`、`empty-text` | 空筛选或加载失败的区域与文字，外层初始 `hidden` |
| `retry` | `button type="button"`，初始 `hidden` |
| `nodes` | `ul`，卡片列表，保留翻译后的无障碍名称 |
| `pagination` | 初始 `hidden` 的 `nav`，保留翻译后的无障碍名称 |
| `previous`、`next` | 分页按钮，初始 `disabled` |
| `page` | 页码文字 |

运行时会添加 `.update-time`、`.update-mode`、`.loading`、`.running`、`.enter`、筛选按钮和分页状态，并更新 `hidden`、`disabled`、`aria-busy`、`aria-pressed`、`data-state` 以及动画用 CSS 变量。若仍复用默认组件，保留对应样式和 `[hidden] { display:none!important; }`，不要用布局样式让隐藏数据重新出现。刷新完成不能依赖 `animationend`。

### 呈现函数接口

入口可直接沿用示例：

```js
import './public.css';
import {mountPublicPage} from '../../src/public/page.js';
import {createCard, skeletonCards} from '../default/cards.js';
import {createConstellation} from '../default/constellation.js';
import {announcementRenderer} from '../default/announcements.js';

mountPublicPage({createCard, skeletonCards, createConstellation, announcementRenderer});
```

需要替换组件时，函数契约如下：

- `createCard()` 返回一个 `li` DOM 节点，提供 `update(node)` 和 `setStale()` 方法。`update` 接收已校验的公开节点并清除连接过期标记；`setStale` 只表达“数据可能过期”，不能把节点改为离线或清空已有指标。运行时控制该节点的筛选隐藏与入场类。
- `skeletonCards(count)` 返回占位 `li` 数组，使用 `.placeholder` 以便加载结束清除；占位内容对辅助技术隐藏。
- `createConstellation(container)` 返回 `{update(nodes), clear()}`。图形只反映当前页已校验的集合；`clear()` 必须撤下全部节点相关信息。
- `announcementRenderer(container)` 返回 `render(items)`。空数组必须清除所有公告并隐藏区域；重复快照尽量保留相同内容的 DOM。标题和正文只能作为纯文本呈现。

## 选择修改层级

1. **配色、密度、圆角**：只改自定义主题 CSS 中的 token／覆盖规则，保留示例对默认 CSS 的导入。控制台样式不受影响。
2. **页面布局、品牌、文案位置**：改自定义 HTML/CSS，保留上面的 ID、语义、翻译属性和占位符。
3. **卡片或图形结构**：把要修改的组件复制到自定义目录，修改 `main.js` 的导入；遵守呈现接口，保留指标新鲜度、无障碍信息和撤回行为。不必复制其余组件。

以上层级都复用 `mountPublicPage`。不要为样式调整复制网络与生命周期代码，也不要加入与服务器状态页设置竞争的前端开关。

## 图片、字体与版本

图片可在 JavaScript 中导入后赋给 DOM：

```js
import logoURL from './assets/logo.svg';
const logo = document.createElement('img');
logo.src = logoURL;
logo.alt = 'Example';
document.querySelector('.brand').prepend(logo);
```

CSS 使用相对导入：

```css
.example-overview { background-image:url('./assets/pattern.svg'); }
@font-face {
  font-family:ThemeText;
  src:url('./assets/theme.woff2') format('woff2');
  font-display:swap;
}
```

这些片段要求先加入实际的素材文件。构建器支持 SVG、PNG、JPG/JPEG、GIF、WebP、AVIF、ICO、WOFF/WOFF2、TTF、OTF，生成稳定文件名和自动 `?v=` 内容版本；不手动改名或维护查询参数。只使用有权分发的素材，字体保留系统字体回退。不要在源 HTML 写相对图片／字体路径或手写 `/_app/` 地址：它们不会成为模块依赖，构建器也无法自动处理；内置 favicon 使用的 `data:` 图片不受此限。静态资源基址、缓存和 CDN 部署见 [Web 开发](../README.md)，此处不重复配置步骤。

## 数据、安全与撤回

### 公开接口与帧格式

共享运行时仅连接原站点的以下只读端点，分页参数为 `limit=50&offset=...`：

- WebSocket：`/api/public/v1/ws`
- SSE：`/api/public/v1/stream`
- HTTP：`/api/public/v1/nodes`

三者使用同一公开帧格式，HTTP 也不是直接返回节点数组：

```json
{
  "type":"snapshot",
  "server_time":"2026-11-07T12:00:00Z",
  "update_interval_seconds":10,
  "data":{
    "announcements":[],
    "nodes":[],
    "generated_at":"2026-11-07T12:00:00Z",
    "has_more":false,
    "next_offset":null
  }
}
```

`mountPublicPage` 的呈现函数收到的是校验后的 `data`，不必解包帧。节点只有公开名称、地区、提供商、状态、最后上报时间、30 日可用性和 CPU／内存／负载／运行时间等公开指标；`metric_status` 独立表达 `ok`、`stale`、`unavailable`。缺失值是 `null`，不能显示成 `0`；离线不代表指标必须清空。公告是最多五条的当前生效公开内容，不含内部 ID。完整字段、分页候选游标与 heartbeat／close／error 帧见 [OpenAPI](../../api/openapi.yaml)。不要从当前节点数推算下一页偏移，也不要跨页拼出隐藏的总数。

### 无凭据与 CSP

- 状态页不需要账户、会话探测、CSRF token、Agent token 或管理接口。HTTP/SSE 使用 `credentials:'omit'`；浏览器 WebSocket 没有这个选项，可能自动带同源 Cookie，但公开端点完全忽略会话，主题不能读取、依赖或传递凭据。
- 主题代码和静态素材对匿名访客可见，不能放密码、密钥、私有主机地址、内部 ID 或未公开节点数据；也不要把节点快照写进 localStorage、日志、分析服务或远端图片 URL。
- 公告、节点名、地区、提供商等内容不要拼进 `innerHTML`、属性名、脚本或样式；用 `textContent` 或共享 `h()` 的文本子节点。
- API、WebSocket 和导航留在原站点；不要用静态资源基址拼它们，也不要从 CDN 取 API 数据。
- 页面受服务器 CSP 约束：不引入第三方脚本、外部字体、内联事件、`eval`，也不要自行放宽。自定义代码通过模块导入，样式通过 CSS 导入；构建生成的明暗启动脚本由服务器处理。外部请求会被策略阻止，不要让用户关 CSP。
- 只安装可信作者的主题，并保护 CDN 写入权限。主题脚本和网站有相同浏览器权限；目录和样式隔离不是沙箱，恶意主题仍可能借已登录会话发请求。

### 关闭、撤回与过期

- 收到已校验的 `public_page_disabled`，必须先停止通道与计时器，清除节点、公告、图形及衍生缓存，再 `location.replace('/')` 让服务器分流。保留旧画面并显示“已关闭”不符合隐私边界。
- 后续有效快照是完整的当前页集合。节点或公告消失时必须立即从 DOM 和图形撤下；空集合也是有效撤回。分页期间公告可保留，直到下一页有效快照替换。
- 普通断线、页面隐藏和临时错误不是撤回：保留最后一次有效值，明确标记过期／重连中，不伪造离线。无初始快照时显示加载失败与重试，不伪造空集合。
- 无效快照绝不能替换最后一次有效数据。隐藏页面停止网络和倒计时；重新显示、BFCache 返回时使用新的连接范围，迟到响应不能回写。继续复用共享运行时即可保留这些行为。

## 语言、明暗与验收

HTML 保留 `data-i18n`、`data-i18n-content`、`data-i18n-aria-label`；组件使用 `src/shared/i18n.js` 的 `t()`／`tParts()` 与当前语言的 `Intl` 格式化。新增文案同时补齐中英文公开词条，不为主题引入控制台完整词条。主题和语言偏好沿用现有控件，不增设另一套存储键。

发布前检查：

- [ ] 默认主题外观不变，自定义主题只影响状态页，登录页和控制台不变。
- [ ] 执行 `npm --prefix web test`、`npm --prefix web run build`、`npm --prefix web run check`；重新编译并重启 Go 后检查实际页面。
- [ ] 中英文、跟随系统／显式深色／显式浅色均可用；长名称、长公告、超长地区不撑破布局；手机窄屏、横屏安全区、平板、桌面无页面级横向溢出，筛选局部滚动可用。
- [ ] 键盘操作、可见焦点、筛选 `aria-pressed`、分页禁用、语言保存错误和屏幕阅读器播报可用；`prefers-reduced-motion` 下动画停止，刷新、加载、分页和连接状态仍可理解。
- [ ] 在线／离线／未知、缺失或过期指标、无节点、无匹配筛选、加载失败与重试都表现正确；WS → SSE → HTTP 回退、手动刷新、分页、隐藏／显示、BFCache 不重复订阅，不接受旧页或迟到数据。
- [ ] 撤销节点发布、撤回公告、全局关闭状态页后没有残留卡片、图形或自定义缓存；关闭后由服务器重新路由。
- [ ] 所有素材经构建导入，未手写产物版本；浏览器无 CSP、资源加载或运行时错误。

行为回归可扩展 `test/public-main.test.js` 与 `test/announcements.test.js`，以输入快照和用户操作断言实际 DOM／通道结果；不要用源码字符串匹配证明导入或功能正确。
