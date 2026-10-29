import {h} from '../shared/dom.js';
import {themeButton} from '../shared/theme.js';
import {onSession, session, signOut} from './api.js';
import {busy, button} from './ui/components.js';
import {toastError} from './ui/feedback.js';
import {initNotifications, syncNotifications} from './notifications.js';

// Sidebar navigation and account block. The markup is static in index.html so
// navigation paints before the script loads. Below 1200px the sidebar is an
// always-visible icon rail; the console does not support phone widths.
let sidebar;
let account;
function shellElements() {
  sidebar = document.getElementById('sidebar');
  account = document.getElementById('account');
}

const themeNames = {system: 'System', light: 'Light', dark: 'Dark'};

function renderAccount() {
  shellElements();
  if (!session.user) { account.replaceChildren(); return; }
  // In the icon rail the label span becomes the button's tooltip.
  const exit = button(h('span', {class: 'nav-label'}, 'Sign out'), {id: 'sign-out', icon: 'logout', small: true, onClick: async () => {
    busy(exit, true);
    try { await signOut(); } catch (error) { busy(exit, false); toastError(error); }
  }});
  const {username} = session.user;
  account.replaceChildren(
    h('div', {class: 'who'}, h('span', {class: 'avatar', 'aria-hidden': 'true', title: username}, username.slice(0, 1)),
      h('div', {class: 'who-text'}, h('div', {class: 'who-name', title: username}, username))),
    h('div', {class: 'account-actions'}, themeButton(themeNames, (now, next) => `Theme: ${now}. Switch to ${next}.`), exit));
}

// Marks the active section; the pill element carries a view-transition-name so
// it glides between items during page transitions.
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

