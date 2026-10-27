import {h} from '../shared/dom.js';
import {themeButton} from '../shared/theme.js';
import {onSession, session, signOut} from './api.js';
import {ROLE} from './format.js';
import {busy, button} from './ui/components.js';
import {toastError} from './ui/feedback.js';

// Sidebar navigation, account block and the mobile drawer. The markup is static
// in index.html so navigation paints before the script loads.
const sidebar = document.getElementById('sidebar');
const account = document.getElementById('account');
const menuButton = document.getElementById('menu');
const scrim = document.getElementById('scrim');

const themeNames = {system: 'System', light: 'Light', dark: 'Dark'};

function renderAccount() {
  if (!session.user) { account.replaceChildren(); return; }
  const exit = button('Sign out', {icon: 'logout', small: true, onClick: async () => {
    busy(exit, true);
    try { await signOut(); } catch (error) { busy(exit, false); toastError(error); }
  }});
  const {username, role} = session.user;
  account.replaceChildren(
    h('div', {class: 'who'}, h('span', {class: 'avatar', 'aria-hidden': 'true'}, username.slice(0, 1)),
      h('div', {class: 'who-text'}, h('div', {class: 'who-name', title: username}, username), h('div', {class: 'who-role'}, ROLE[role]))),
    h('div', {class: 'account-actions'}, themeButton(themeNames, (now, next) => `Theme: ${now}. Switch to ${next}.`), exit));
}

// Marks the active section; the pill element carries a view-transition-name so
// it glides between items during page transitions.
export function setActiveNav(nav) {
  let pill = sidebar.querySelector('.nav-pill');
  for (const anchor of sidebar.querySelectorAll('[data-nav]')) {
    if (anchor.dataset.nav === nav) {
      anchor.setAttribute('aria-current', 'page');
      pill = pill || h('span', {class: 'nav-pill', 'aria-hidden': 'true'});
      anchor.prepend(pill);
    } else anchor.removeAttribute('aria-current');
  }
  if (!nav && pill) pill.remove();
}

function openMenu() {
  sidebar.classList.add('open');
  scrim.hidden = false;
  menuButton.setAttribute('aria-expanded', 'true');
  sidebar.querySelector('.nav a').focus();
}

export function closeMenu(restoreFocus = false) {
  if (!sidebar.classList.contains('open')) return;
  sidebar.classList.remove('open');
  scrim.hidden = true;
  menuButton.setAttribute('aria-expanded', 'false');
  if (restoreFocus) menuButton.focus();
}

export function brandMark() { return sidebar.querySelector('.mark').cloneNode(true); }

export function initShell() {
  onSession(renderAccount);
  menuButton.addEventListener('click', () => (sidebar.classList.contains('open') ? closeMenu(true) : openMenu()));
  scrim.addEventListener('click', () => closeMenu(true));
  document.addEventListener('keydown', (event) => { if (event.key === 'Escape') closeMenu(true); });
}
