import {t} from './i18n.js';
import {initCommandPalette} from './command-palette.js';
import {localeSelect} from '../shared/locale-select.js';
import {confirmLocaleChange, reloadAfterConfirmation} from './router.js';
import {h} from '../shared/dom.js';
import {themeButton} from '../shared/theme.js';
import {getNavigationSignal, onSession, session, signOut} from './api.js';
import {busy, button} from './ui/components.js';
import {toastError} from './ui/feedback.js';
import {initNotifications, syncNotifications} from './notifications.js';

// 侧边栏导航与账户区块。标记是 index.html 中的静态内容，
// 导航在脚本加载前即可呈现。768–1199px 为图标栏，手机复用为抽屉。
let sidebar;
let account;
let drawer;
function shellElements() {
  sidebar = document.getElementById('sidebar');
  account = document.getElementById('account');
}


function renderAccount() {
  shellElements();
  drawer?.sync();
  if (session.user?.must_change_password) document.documentElement.classList.add('route-login');
  if (!session.user || session.user.must_change_password) { account.replaceChildren(); return; }
  // 图标栏中，标签 span 充当按钮的工具提示。
  const exit = button(h('span', {class: 'nav-label'}, t('common.signOut')), {id: 'sign-out', icon: 'logout', small: true, onClick: async () => {
    busy(exit, true);
    try { await signOut(); } catch (error) { busy(exit, false); toastError(error); }
  }});
  const {username} = session.user;
  account.replaceChildren(
    h('div', {class: 'who'}, h('span', {class: 'avatar', 'aria-hidden': 'true', title: username}, username.slice(0, 1)),
      h('div', {class: 'who-text'}, h('div', {class: 'who-name', title: username}, username))),
    h('div', {class: 'console-locale account-locale'}, consoleLocaleSelect()),
    h('div', {class: 'account-actions'}, themeButton(), exit));
}

export function consoleLocaleSelect({navigation = false} = {}) {
  return localeSelect({beforeChange: confirmLocaleChange, reload: reloadAfterConfirmation, onError: toastError,
    signal: navigation ? getNavigationSignal() : undefined});
}

// 标记当前激活区块；pill 元素带有 view-transition-name，
// 页面过渡时在各条目间平滑滑动。
export function setActiveNav(nav) {
  shellElements();
  drawer?.route();
  let pill = sidebar.querySelector('.nav-pill');
  for (const anchor of sidebar.querySelectorAll('[data-nav]')) {
    if (anchor.dataset.nav === nav) {
      anchor.setAttribute('aria-current', 'page');
      pill = pill || h('span', {class: 'nav-pill', 'aria-hidden': 'true'});
      anchor.prepend(pill);
    } else anchor.removeAttribute('aria-current');
  }
  if (!nav && pill) pill.remove();
  syncNotifications();
}

export function brandMark() { shellElements(); return sidebar.querySelector('.mark').cloneNode(true); }

export function initShell() {
  shellElements();
  drawer = mobileNavigation();
  const unsubscribe = onSession(renderAccount);
  renderAccount();
  const stopNotifications = initNotifications();
  const stopCommands = initCommandPalette();
  return () => {
    unsubscribe();
    drawer.dispose();
    drawer = undefined;
    stopNotifications();
    stopCommands();
  };
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
  const controls = () => [...rail.querySelectorAll('a,button,select,input,textarea,[tabindex]')]
    .filter((element) => !element.disabled && !element.hidden && !element.classList.contains('brand') && element.getAttribute('tabindex') !== '-1');
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
    if (event.key === 'Escape') { event.preventDefault(); close(); }
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
