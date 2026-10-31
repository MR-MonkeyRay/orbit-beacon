import {t} from './i18n.js';
import {localeSelect} from '../shared/locale-select.js';
import {confirmLocaleChange, reloadAfterConfirmation} from './router.js';
import {h} from '../shared/dom.js';
import {themeButton} from '../shared/theme.js';
import {getNavigationSignal, onSession, session, signOut} from './api.js';
import {busy, button} from './ui/components.js';
import {toastError} from './ui/feedback.js';
import {initNotifications, syncNotifications} from './notifications.js';

// 侧边栏导航与账户区块。标记是 index.html 中的静态内容，
// 导航在脚本加载前即可呈现。1200px 以下侧边栏为常驻图标栏；
// 控制台不支持手机宽度。
let sidebar;
let account;
function shellElements() {
  sidebar = document.getElementById('sidebar');
  account = document.getElementById('account');
}


function renderAccount() {
  shellElements();
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
  onSession(renderAccount);
  renderAccount();
  initNotifications();
}

