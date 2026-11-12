import {t} from './i18n.js';
import {errorText} from '../shared/errors.js';
import {h, reducedMotion} from '../shared/dom.js';
import {ensureSession, onSession, pauseSessionVerification, session, setNavigationSignal, verifySession} from './api.js';
import {revealNavigation, setActiveNav} from './shell.js';
import {motionTiming} from '../shared/motion.js';
import {button, linkButton, pageHead, panel, resetIDs, skeleton} from './ui/components.js';
import {closeDialogs, confirmDiscard, dialogActionStale, hasUnsavedDialogChanges} from './ui/dialogs.js';
import {progress} from './ui/feedback.js';

// History API 路由器。页面是返回元素（或重定向路径字符串）的异步函数。
// 路由可提供与页面布局一致的 placeholder()：导航时立即换上占位，数据到达后原位替换；
// 没有占位的路由在加载超过 160ms 时显示通用骨架。静默刷新始终等数据就绪后原地替换。
// 页面进入只在 #view 上做 WAAPI 淡入，不使用视图过渡（快照会绘制在顶层、盖住侧栏）。
let view;
let routes = [];
let navigation = 0;
let controller;
let currentURL = location.pathname + location.search + location.hash;
let intent = 0;
let viewAnimation;
let confirmedUnload = false;

class NavigationCancelled extends DOMException {
  constructor() { super('The navigation was cancelled.', 'AbortError'); }
}

// 事件触发的导航没有调用方等待结果；只忽略本路由器产生的取消错误，
// 其他加载或过渡错误仍须向外抛出。
export function ignoreNavigationCancellation(error) {
  if (!(error instanceof NavigationCancelled)) throw error;
}

const hasUnsavedChanges = () => Boolean(view.querySelector('form[data-dirty="true"]'));
// SPA 导航与后退必须与语言切换、卸载路径一致地保护打开的对话框输入。
const hasUnsavedWork = () => hasUnsavedChanges() || hasUnsavedDialogChanges();

const localeChangeBusy = () => Boolean(document.querySelector('[data-busy="true"],button.busy'));
export async function confirmLocaleChange() {
  if (localeChangeBusy()) return false;
  if (hasUnsavedWork() && !(await confirmDiscard())) return false;
  return !localeChangeBusy();
}

export function reloadAfterConfirmation() {
  confirmedUnload = true;
  location.reload();
}

export async function go(target, options = {}) {
  const attempt = ++intent;
  if (!options.force && hasUnsavedWork() && !(await confirmDiscard())) throw new NavigationCancelled();
  if (attempt !== intent) throw new NavigationCancelled();
  const destination = new URL(target, location.href);
  if (destination.pathname === '/' || destination.pathname === '/public.html') {
    confirmedUnload = true;
    location[options.replace ? 'replace' : 'assign'](target);
    return;
  }
  if (target !== location.pathname + location.search + location.hash) {
    history.replaceState({...history.state, scroll: window.scrollY}, '');
    history[options.replace ? 'replaceState' : 'pushState']({scroll: 0}, '', target);
  }
  await render({scroll: 0});
}

// 变更后原地重渲染当前页面，保留滚动位置和焦点。
export const refresh = () => dialogActionStale() ? Promise.reject(new NavigationCancelled()) : render({quiet: true});

async function render(options = {}) {
  const token = ++navigation;
  controller?.abort();
  controller = new AbortController();
  setNavigationSignal(controller.signal);
  const openerID = closeDialogs();
  currentURL = location.pathname + location.search + location.hash;
  const matched = routes.find((item) => item.match(location.pathname));
  // 兜底路由只用于高亮导航和骨架；拼错的深链必须显式报 404，而不是静默渲染概览。
  const route = matched || routes.find((item) => item.fallback);
  const focusID = options.quiet ? openerID || document.activeElement?.id : '';
  progress.start();
  // 第一次可见的替换（占位或骨架）负责进入动画与回到顶部，数据到达后的替换不再重复。
  let shown = false;
  const showLoading = (content) => {
    if (token !== navigation || shown) return;
    shown = true;
    swap(content, route, 'enter', token);
    if (options.scroll !== undefined) window.scrollTo({top: options.scroll, behavior: 'instant'});
  };
  // 会话校验较慢（首次打开）时同样优先用路由自己的占位；占位不含私有数据。
  const skeletonTimer = options.quiet ? 0 : setTimeout(() => showLoading(matched?.placeholder ? matched.placeholder() : skeleton()), 160);
  const loadingController = controller;
  const cancelSkeleton = () => clearTimeout(skeletonTimer);
  loadingController.signal.addEventListener('abort', cancelSkeleton, {once: true});
  let content;
  try {
    resetIDs();
    // 在页面加载器读取私有数据或构建私有 DOM 前，先校验会话。
    if (location.pathname !== '/login') {
      try { await ensureSession(); }
      catch (error) { if (error.status === 401) return go('/login', {replace: true, force: true}); throw error; }
    }
    if (token !== navigation) throw new NavigationCancelled();
    if (session.user?.must_change_password && location.pathname !== '/change-password') {
      return go('/change-password', {replace: true, force: true});
    }
    if (location.pathname === '/change-password' && !session.user?.must_change_password) {
      return go(session.user ? '/overview' : '/login', {replace: true, force: true});
    }
    if (!matched) throw Object.assign(new Error('Not found'), {status: 404});
    if (!options.quiet && route.placeholder) {
      clearTimeout(skeletonTimer);
      showLoading(route.placeholder());
      // 占位不得占用页面的稳定 ID；静默刷新恢复焦点依赖它们与首次渲染一致。
      resetIDs();
    }
    content = await route.page();
  } catch (error) {
    if (token !== navigation) throw new NavigationCancelled();
    content = errorPage(error);
  } finally {
    clearTimeout(skeletonTimer);
    loadingController.signal.removeEventListener('abort', cancelSkeleton);
    if (token === navigation) progress.done();
  }
  if (token !== navigation) throw new NavigationCancelled();
  if (typeof content === 'string') return go(content, {replace: true, force: true});
  swap(content, route, options.quiet ? 'none' : shown ? 'fill' : 'enter', token);
  if (options.quiet) {
    const previous = focusID && document.getElementById(focusID);
    const target = previous && !previous.disabled ? previous : view.querySelector('h1');
    if (target) target.focus({preventScroll: true});
    if (document.activeElement !== target) view.querySelector('h1')?.focus({preventScroll: true});
    return;
  }
  if (!shown && options.scroll !== undefined) window.scrollTo({top: options.scroll, behavior: 'instant'});
  const target = view.querySelector('[data-autofocus]') || view.querySelector('h1');
  if (target) {
    target.focus({preventScroll: true});
    if (target.dataset.sectionLink !== undefined) target.scrollIntoView({block: 'start', behavior: 'instant'});
  }
}

// 同步替换：内容、导航高亮与标题在同一任务内更新，浏览器不会画出中间状态。
// enter：#view 淡入并上移 4px；fill：占位换成真实内容，不重播动画；none：静默刷新。
function swap(content, route, mode, token) {
  if (token !== navigation) return;
  const root = document.documentElement;
  const wasBare = root.classList.contains('route-login');
  root.classList.toggle('route-login', Boolean(route.bare));
  setActiveNav(route.nav);
  view.replaceChildren(content);
  const heading = content.querySelector('h1');
  document.title = heading?.textContent ? `${heading.textContent} – OrbitBeacon` : t('shell.consoleTitle');
  if (mode !== 'enter') return;
  animateView();
  if (wasBare && !route.bare) revealNavigation();
}

function animateView() {
  viewAnimation?.cancel();
  viewAnimation = null;
  if (reducedMotion.matches || document.hidden || typeof view.animate !== 'function') return;
  viewAnimation = view.animate([{opacity: 0, transform: 'translateY(4px)'}, {opacity: 1, transform: 'none'}], motionTiming('--motion-normal', '--ease'));
}

function errorPage(error) {
  const notFound = error.status === 404;
  return h('div', null, pageHead(notFound ? t('shell.notFound') : t('shell.pageLoadFailed')),
    panel(null, {}, h('p', null, notFound ? t('shell.resourceMissingHint') : errorText(error)),
      notFound ? linkButton('/nodes', t('shell.goToNodes')) : button(t('shell.tryAgain'), {icon: 'refresh', variant: 'primary', onClick: () => { void render().catch(ignoreNavigationCancellation); }})));
}

export function startRouter(table) {
  view = document.getElementById('view');
  routes = table;
  const lifetime = new AbortController();
  const listen = (target, type, listener) => target.addEventListener(type, listener, {signal: lifetime.signal});
  const unsubscribe = onSession((current, detail) => {
    if (!detail.boundary || (!detail.previousUser && !['expired', 'unauthenticated', 'logout', 'password', 'deleted'].includes(detail.reason))) return;
    if (!detail.previousUser && location.pathname === '/login') return;
    // 会话安全边界必须同步清理，不等待视图过渡或未保存更改提示。
    // 会话过期后，私有视图和挂在 body 下的对话框都必须移除。
    navigation += 1;
    intent += 1;
    controller?.abort();
    closeDialogs();
    document.getElementById('toasts')?.replaceChildren();
    view.replaceChildren();
    const target = current.user?.must_change_password ? '/change-password'
      : current.user ? location.pathname + location.search + location.hash
        : detail.reason === 'password' ? '/login?password-changed' : '/login';
    if (current.user && !current.user.must_change_password && location.pathname === '/login') return;
    void go(target, {replace: true, force: true}).catch(ignoreNavigationCancellation);
  });
  history.scrollRestoration = 'manual';
  listen(document, 'click', (event) => {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const anchor = event.target.closest('a[data-link]');
    if (!anchor || anchor.target) return;
    const url = new URL(anchor.href);
    if (url.origin !== location.origin) return;
    event.preventDefault();
    void go(url.pathname + url.search + url.hash).catch(ignoreNavigationCancellation);
  });
  listen(window, 'popstate', async (event) => {
    const attempt = ++intent;
    const previousURL = currentURL;
    const scroll = window.scrollY;
    if (hasUnsavedWork() && !(await confirmDiscard())) {
      if (attempt !== intent) return;
      history.pushState({scroll}, '', previousURL);
      window.scrollTo({top: scroll, behavior: 'instant'});
      return;
    }
    if (attempt === intent) void render({scroll: (event.state && event.state.scroll) || 0}).catch(ignoreNavigationCancellation);
  });
  // 标签页原地替换 hash；表单被编辑时记住它。
  for (const type of ['input', 'change', 'tags']) listen(document, type, () => {
    currentURL = location.pathname + location.search + location.hash;
  });
  const revalidate = () => {
    if (location.pathname === '/login' || !session.user || document.hidden) return;
    void verifySession({signal: lifetime.signal}).catch(() => { /* 网络故障时保留已有快照，但会话仍按已知时间过期 */ });
  };
  listen(document, 'visibilitychange', () => { if (document.hidden) pauseSessionVerification(); else revalidate(); });
  listen(window, 'pagehide', pauseSessionVerification);
  listen(window, 'pageshow', revalidate);
  listen(window, 'beforeunload', (event) => {
    if (confirmedUnload) { confirmedUnload = false; return; }
    if (hasUnsavedWork()) event.preventDefault();
  });
  void render().catch(ignoreNavigationCancellation);
  return () => {
    lifetime.abort();
    unsubscribe();
    navigation += 1;
    intent += 1;
    controller?.abort();
    viewAnimation?.cancel();
    closeDialogs();
    progress.done();
  };
}
