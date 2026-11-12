import {t} from './i18n.js';
import {initCommandPalette} from './command-palette.js';
import {menuButton} from './ui/menu.js';
import {localeToggle} from '../shared/locale-select.js';
import {confirmLocaleChange, reloadAfterConfirmation} from './router.js';
import {h, icon, reducedMotion} from '../shared/dom.js';
import {motionTiming} from '../shared/motion.js';
import {currentTheme, onThemeChange, setTheme} from '../shared/theme.js';
import {getNavigationSignal, onSession, session, signOut} from './api.js';
import {busy} from './ui/components.js';
import {toastError} from './ui/feedback.js';
import {initNotifications, syncNotifications} from './notifications.js';

// 侧边栏导航与账户区块。标记是 index.html 中的静态内容，
// 导航在脚本加载前即可呈现。768–1199px 为图标栏（可临时浮出展开），手机复用为抽屉。
let sidebar;
let account;
let drawer;
let accountLifetime;
let syncNavigation = () => {};
let closePeek = () => {};
let railTip;
function shellElements() {
  sidebar = document.getElementById('sidebar');
  account = document.getElementById('account');
}


function renderAccount() {
  shellElements();
  accountLifetime?.abort();
  accountLifetime = new AbortController();
  drawer?.sync();
  if (session.user?.must_change_password) document.documentElement.classList.add('route-login');
  if (!session.user || session.user.must_change_password) { account.replaceChildren(); return; }
  const {username} = session.user;
  const userMenu = menuButton({id: 'user-menu', signal: accountLifetime.signal,
    label: h('span', {class: 'avatar', 'aria-hidden': 'true'}, username.slice(0, 1)),
    items: [
      ...['system', 'light', 'dark'].map((value) => ({group: t('controls.theme'),
        label: value === 'system' ? t('controls.followSystem') : t(`controls.theme.${value}`),
        checked: () => currentTheme() === value, onSelect: () => setTheme(value)})),
      {id: 'sign-out', label: t('common.signOut'), onSelect: async () => {
        busy(trigger, true);
        try { await signOut(); } catch (error) { busy(trigger, false); toastError(error); }
      }}
    ]});
  userMenu.classList.add('user-menu');
  const trigger = userMenu.querySelector('button');
  onThemeChange(userMenu.syncChecks, accountLifetime.signal);
  trigger.setAttribute('aria-label', username);
  trigger.title = username;
  const locale = consoleLocaleToggle({signal: accountLifetime.signal});
  locale.classList.add('locale-toggle');
  const collapse = h('button', {type: 'button', class: 'icon-button nav-toggle', 'aria-controls': 'sidebar', onclick: toggleNavigation});
  account.replaceChildren(userMenu, locale, collapse);
  syncNavigation();
}

export function consoleLocaleToggle({navigation = false, signal} = {}) {
  return localeToggle({beforeChange: confirmLocaleChange, reload: reloadAfterConfirmation, onError: toastError,
    signal: navigation ? getNavigationSignal() : signal});
}

// 标记当前激活区块。只有一个 pill：移到新条目后用 FLIP 从旧位置滑过来；
// 它位于 .nav 的层叠上下文底层，滑动时不会盖住任何条目文字。
export function setActiveNav(nav) {
  shellElements();
  drawer?.route();
  closePeek();
  railTip?.hide();
  let pill = sidebar.querySelector('.nav-pill');
  const from = pill?.parentElement ? measure(pill) : null;
  let moved = false;
  for (const anchor of sidebar.querySelectorAll('[data-nav]')) {
    if (anchor.dataset.nav === nav) {
      anchor.setAttribute('aria-current', 'page');
      pill = pill || h('span', {class: 'nav-pill', 'aria-hidden': 'true'});
      if (pill.parentElement !== anchor) { anchor.prepend(pill); moved = true; }
    } else anchor.removeAttribute('aria-current');
  }
  if (!nav && pill) pill.remove();
  else if (moved && from) slide(pill, from);
  syncNotifications();
}

const measure = (element) => typeof element.getBoundingClientRect === 'function' ? element.getBoundingClientRect() : null;

function slide(element, from) {
  if (reducedMotion.matches || document.hidden || typeof element.animate !== 'function' || !from.height) return;
  const to = measure(element);
  if (!to?.height) return;
  const dx = from.left - to.left;
  const dy = from.top - to.top;
  if (!dx && !dy) return;
  element.animate([{transform: `translate(${dx}px,${dy}px)`}, {transform: 'none'}], motionTiming('--motion-slow', '--ease'));
}

// 登录后侧栏首次出现时从左侧滑入，内容区自己淡入，二者互不覆盖。
export function revealNavigation() {
  shellElements();
  if (reducedMotion.matches || document.hidden || typeof sidebar.animate !== 'function') return;
  sidebar.animate([{opacity: 0, transform: 'translateX(-12px)'}, {opacity: 1, transform: 'none'}], motionTiming('--motion-slow', '--ease'));
}

export function brandMark() { shellElements(); return sidebar.querySelector('.mark').cloneNode(true); }

let desktop;
let tablet;

// 图标栏状态：平板未浮出、或桌面已收起。手机使用抽屉，没有图标栏。
const railActive = () => {
  const root = document.documentElement;
  return Boolean(tablet?.matches && !root.classList.contains('nav-peek')
    || desktop?.matches && root.classList.contains('nav-collapsed'));
};

function toggleNavigation() {
  const root = document.documentElement;
  railTip?.hide();
  if (tablet?.matches) {
    root.classList.toggle('nav-peek', !root.classList.contains('nav-peek'));
    syncNavigation();
    return;
  }
  const collapsed = !root.classList.contains('nav-collapsed');
  root.classList.toggle('nav-collapsed', collapsed);
  try { localStorage.setItem('orbitbeacon.nav', collapsed ? 'collapsed' : 'expanded'); } catch {}
  syncNavigation();
}

export function initShell() {
  shellElements();
  const root = document.documentElement;
  try { root.classList.toggle('nav-collapsed', localStorage.getItem('orbitbeacon.nav') === 'collapsed'); } catch {}
  desktop = matchMedia('(min-width: 1200px)');
  tablet = matchMedia('(min-width: 768px) and (max-width: 1199px)');
  const lifetime = new AbortController();
  const listen = (target, type, listener, options) => target.addEventListener(type, listener, {...options, signal: lifetime.signal});
  syncNavigation = () => {
    root.classList.toggle('nav-compact', tablet.matches);
    if (!tablet.matches) root.classList.remove('nav-peek');
    railTip?.hide();
    const toggle = account.querySelector('.nav-toggle');
    if (!toggle) return;
    const rail = railActive();
    toggle.hidden = !desktop.matches && !tablet.matches;
    toggle.replaceChildren(icon(rail ? 'sidebar-expand' : 'sidebar-collapse'));
    toggle.setAttribute('aria-expanded', String(!rail));
    toggle.title = t(rail ? 'shell.expandNav' : 'shell.collapseNav');
    toggle.setAttribute('aria-label', toggle.title);
  };
  // 平板浮出的侧栏是临时的：导航、Esc、点击侧栏之外或断点变化都会收回，不写入偏好。
  closePeek = ({restoreFocus = false} = {}) => {
    if (!root.classList.contains('nav-peek')) return;
    root.classList.remove('nav-peek');
    syncNavigation();
    if (restoreFocus) account.querySelector('.nav-toggle')?.focus({preventScroll: true});
  };
  listen(document, 'keydown', (event) => {
    if (event.key !== 'Escape' || event.defaultPrevented || !root.classList.contains('nav-peek')) return;
    if ([...document.querySelectorAll('dialog')].some((dialog) => dialog.open)) return;
    event.preventDefault();
    closePeek({restoreFocus: true});
  });
  listen(document, 'pointerdown', (event) => {
    if (root.classList.contains('nav-peek') && !sidebar.contains(event.target)) closePeek();
  });
  listen(desktop, 'change', syncNavigation);
  listen(tablet, 'change', syncNavigation);
  railTip = railTooltip(lifetime.signal, railActive);
  drawer = mobileNavigation();
  const shortcut = sidebar.querySelector('.command-shortcut');
  if (shortcut) shortcut.textContent = /Mac|iPhone|iPad|iPod/.test(navigator.userAgentData?.platform || navigator.platform || '') ? '⌘K' : 'Ctrl K';
  const unsubscribe = onSession(renderAccount);
  renderAccount();
  const stopNotifications = initNotifications();
  const stopCommands = initCommandPalette();
  return () => {
    unsubscribe();
    lifetime.abort();
    railTip?.remove();
    railTip = undefined;
    closePeek = () => {};
    accountLifetime?.abort();
    drawer.dispose();
    drawer = undefined;
    stopNotifications();
    stopCommands();
  };
}

// 图标栏的悬停与键盘聚焦提示。标签仍留在条目内作为可访问名称（只是被裁剪、淡出），
// 提示是 body 下的装饰副本，固定定位在条目右侧，不会被侧栏裁剪，也不影响布局。
function railTooltip(signal, active) {
  const tip = h('div', {class: 'rail-tip', 'aria-hidden': 'true'});
  document.body.append(tip);
  let owner = null;
  const item = (target) => {
    for (let element = target; element && element !== sidebar; element = element.parentElement) {
      if (element.querySelector?.(':scope > .nav-label') && !element.classList.contains('brand')) return element;
    }
    return null;
  };
  const hide = () => { owner = null; tip.classList.remove('show'); };
  const show = (element) => {
    if (!element || !active()) { hide(); return; }
    const rect = measure(element);
    const label = element.querySelector(':scope > .nav-label')?.textContent.trim();
    if (!rect || !label) { hide(); return; }
    owner = element;
    tip.textContent = label;
    tip.style.left = `${Math.round(rect.right + 10)}px`;
    tip.style.top = `${Math.round(rect.top + rect.height / 2)}px`;
    tip.classList.add('show');
  };
  const listen = (target, type, listener, options) => target.addEventListener(type, listener, {...options, signal});
  listen(sidebar, 'pointerover', (event) => { if (event.pointerType === 'mouse') show(item(event.target)); });
  listen(sidebar, 'pointerout', (event) => { if (owner && !owner.contains(event.relatedTarget)) hide(); });
  listen(sidebar, 'focusin', (event) => { const element = item(event.target); if (element?.matches?.(':focus-visible')) show(element); });
  listen(sidebar, 'focusout', hide);
  listen(sidebar, 'click', hide, {capture: true});
  listen(sidebar, 'scroll', hide);
  listen(window, 'scroll', hide, {passive: true});
  return {hide, remove() { hide(); tip.remove(); }};
}


// 抽屉只管理外壳；路由继续负责页面焦点，原生 dialog 继续负责模态焦点。
function mobileNavigation() {
  const bar = document.getElementById('mobile-bar');
  const menu = document.getElementById('mobile-menu');
  const backdrop = document.getElementById('drawer-backdrop');
  const view = document.getElementById('view');
  const rail = sidebar;
  const root = document.documentElement;
  const phone = matchMedia('(max-width: 767px)');
  const lifetime = new AbortController();
  const listen = (target, type, listener, options) => target.addEventListener(type, listener, {...options, signal: lifetime.signal});
  let opened = false;
  let navigation;
  const navigating = () => close(false);
  const available = () => Boolean(session.user && !session.user.must_change_password)
    && !root.classList.contains('route-login') && !['/login', '/change-password'].includes(location.pathname);
  const close = (restoreFocus = true) => {
    const wasOpen = opened;
    opened = false;
    root.classList.remove('drawer-open');
    menu.setAttribute('aria-expanded', 'false');
    backdrop.hidden = true;
    view.inert = false;
    rail.inert = phone.matches;
    if (wasOpen && restoreFocus && !bar.hidden) menu.focus({preventScroll: true});
  };
  const sync = () => {
    bar.hidden = !available();
    if (!phone.matches || bar.hidden) close(false);
    rail.inert = phone.matches && !opened;
  };
  const hiddenAncestor = (element) => {
    for (let parent = element.parentElement; parent && parent !== rail; parent = parent.parentElement) if (parent.hidden) return true;
    return false;
  };
  const controls = () => [...rail.querySelectorAll('a,button,select,input,textarea,[tabindex]')]
    .filter((element) => !element.disabled && !element.hidden && !hiddenAncestor(element) && !element.classList.contains('brand') && element.getAttribute('tabindex') !== '-1');
  listen(menu, 'click', () => {
    if (opened) { close(); return; }
    if (!phone.matches || !available()) return;
    opened = true;
    rail.inert = false;
    view.inert = true;
    root.classList.add('drawer-open');
    menu.setAttribute('aria-expanded', 'true');
    backdrop.hidden = false;
    controls()[0]?.focus({preventScroll: true});
  });
  listen(backdrop, 'click', () => close());
  // 捕获阶段先收起：dialog 打开时记录当前焦点，抽屉收起后原入口已 inert，焦点回不去。
  listen(rail, 'click', (event) => {
    // 不依赖 closest；SVG 内的点击同样沿父链找到入口。
    for (let target = event.target; target && target !== rail; target = target.parentElement) {
      if (target.tagName.toLowerCase() === 'a' || target.getAttribute('aria-haspopup') === 'dialog') { close(); break; }
    }
  }, {capture: true});
  listen(document, 'keydown', (event) => {
    if (!opened || [...document.querySelectorAll('dialog')].some((dialog) => dialog.open)) return;
    if (event.key === 'Escape' && !event.defaultPrevented) { event.preventDefault(); close(); }
    if (event.key !== 'Tab') return;
    const items = [menu, ...controls()];
    const index = items.indexOf(document.activeElement);
    if (event.shiftKey && index <= 0 || !event.shiftKey && (index < 0 || index === items.length - 1)) {
      event.preventDefault();
      items[event.shiftKey ? items.length - 1 : 0].focus();
    }
  });
  listen(phone, 'change', () => { close(); sync(); });
  const route = () => {
    close(false);
    sync();
    navigation?.removeEventListener('abort', navigating);
    navigation = getNavigationSignal();
    navigation?.addEventListener('abort', navigating, {once: true});
  };
  route();
  return {route, sync, dispose() {
    close(false);
    rail.inert = false;
    navigation?.removeEventListener('abort', navigating);
    lifetime.abort();
  }};
}
