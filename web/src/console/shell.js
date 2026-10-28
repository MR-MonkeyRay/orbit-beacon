import {h} from '../shared/dom.js';
import {themeButton} from '../shared/theme.js';
import {api, onSession, session, signOut} from './api.js';
import {ROLE} from './format.js';
import {busy, button} from './ui/components.js';
import {toastError} from './ui/feedback.js';

// Sidebar navigation, account block and the mobile drawer. The markup is static
// in index.html so navigation paints before the script loads.
const sidebar = document.getElementById('sidebar');
const account = document.getElementById('account');
const menuButton = document.getElementById('menu');
const scrim = document.getElementById('scrim');
const narrow = matchMedia('(max-width: 860px)');

const themeNames = {system: 'System', light: 'Light', dark: 'Dark'};

function syncMenuInert() { sidebar.inert = narrow.matches && !sidebar.classList.contains('open'); }

function renderAccount() {
  if (!session.user) { account.replaceChildren(); return; }
  const exit = button('Sign out', {id: 'sign-out', icon: 'logout', small: true, onClick: async () => {
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
  syncAlertsBadge();
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
  syncMenuInert();
  scrim.hidden = false;
  menuButton.setAttribute('aria-expanded', 'true');
  sidebar.querySelector('.nav a').focus();
}

export function closeMenu(restoreFocus = false) {
  if (!sidebar.classList.contains('open')) return;
  sidebar.classList.remove('open');
  syncMenuInert();
  scrim.hidden = true;
  menuButton.setAttribute('aria-expanded', 'false');
  if (restoreFocus) menuButton.focus();
}

export function brandMark() { return sidebar.querySelector('.mark').cloneNode(true); }

export function initShell() {
  syncMenuInert();
  narrow.addEventListener('change', syncMenuInert);
  onSession(() => { renderAccount(); syncAlertsBadge(); });
  document.addEventListener('visibilitychange', syncAlertsBadge);
  window.addEventListener('pagehide', stopAlertsBadge);
  window.addEventListener('pageshow', syncAlertsBadge);
  syncAlertsBadge();
  menuButton.addEventListener('click', () => (sidebar.classList.contains('open') ? closeMenu(true) : openMenu()));
  scrim.addEventListener('click', () => closeMenu(true));
  document.addEventListener('keydown', (event) => { if (event.key === 'Escape') closeMenu(true); });
}

// The shell survives page swaps; only poll while the authenticated console is visible.
let alertTimer;
let alertRequest;
let alertLastUpdated = 0;
function stopAlertsBadge() {
  clearInterval(alertTimer);
  alertTimer = undefined;
  alertRequest?.abort();
  alertRequest = null;
}
function syncAlertsBadge() {
  const anchor = sidebar.querySelector('[data-nav="/alerts"]');
  if (!anchor) return;
  let badge = anchor.querySelector('.alert-nav-count');
  if (!session.user || document.hidden || location.pathname === '/login') {
    stopAlertsBadge();
    if (!session.user) {
      badge?.remove();
      anchor.setAttribute('aria-label', 'Alerts');
      alertLastUpdated = 0;
    }
    return;
  }
  if (!badge) {
    badge = h('span', {class: 'alert-nav-count', 'aria-hidden': 'true'}, '…');
    anchor.append(badge);
  }
  const update = async () => {
    if (alertRequest) return;
    const request = new AbortController();
    alertRequest = request;
    try {
      const {firing} = await api('/alerts/summary', {signal: request.signal});
      if (alertRequest !== request || document.hidden) return;
      badge.textContent = String(firing.total);
      anchor.setAttribute('aria-label', `Alerts, ${firing.total} firing`);
      alertLastUpdated = Date.now();
    } catch (error) {
      if (alertRequest === request && error.name !== 'AbortError') {
        badge.textContent = '?';
        anchor.setAttribute('aria-label', 'Alerts, firing count unavailable');
      }
    } finally { if (alertRequest === request) alertRequest = null; }
  };
  if (!alertTimer) alertTimer = setInterval(update, 60000);
  // Refresh on return once the count is half a cadence old, bounding staleness.
  if (!alertLastUpdated || Date.now() - alertLastUpdated >= 30000) update();
}
