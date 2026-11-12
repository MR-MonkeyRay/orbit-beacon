import test from 'node:test';
import assert from 'node:assert/strict';
import {h, reducedMotion} from '../src/shared/dom.js';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, flush, installDOM} from './console-dom.js';

async function setup(t, {mobile = true, tablet = false, nav = null, storageBlocked = false} = {}) {
  const dom = installDOM();
  const originals = new Map(['window', 'fetch', 'matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  globalThis.window = new EventTarget();
  window.scrollTo = () => {};
  globalThis.requestAnimationFrame = () => 0;
  const phone = new EventTarget(); phone.matches = mobile;
  const desktop = new EventTarget(); desktop.matches = !mobile && !tablet;
  const compact = new EventTarget(); compact.matches = tablet;
  globalThis.matchMedia = (query) => query === '(max-width: 767px)' ? phone : query === '(min-width: 1200px)' ? desktop : compact;
  localStorage.getItem = (key) => {
    if (storageBlocked) throw new Error('Storage unavailable');
    return key === 'orbitbeacon.nav' ? nav : null;
  };
  const motion = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => true});
  Object.assign(location, {pathname: '/overview', search: '', hash: '', href: 'https://example.test/overview', origin: 'https://example.test'});
  const setURL = (target) => {
    if (!target) return;
    const url = new URL(target, location.href);
    Object.assign(location, {pathname: url.pathname, search: url.search, hash: url.hash, href: url.href});
  };
  globalThis.history = {state: {}, replaceState(_state, _title, target) { setURL(target); }, pushState(_state, _title, target) { setURL(target); }};
  const user = {id: 'mobile-admin', username: 'Admin', must_change_password: false};
  const account = {user, csrf_token: 'csrf', expires_at: ''};
  const requests = [];
  globalThis.fetch = async (url) => {
    requests.push(url);
    if (url === '/api/v1/session') return new Response(null, {status: 204});
    if (url === '/api/v1/me') return Response.json(account);
    if (url === '/api/v1/alerts/summary') return Response.json({firing: {total: 0, critical: 0}});
    if (url === '/api/v1/notifications/summary') return Response.json({unread_count: 0});
    if (url.startsWith('/api/v1/nodes?')) return Response.json({nodes: [], has_more: false});
    throw new Error(`Unexpected request ${url}`);
  };
  const menu = h('button', {id: 'mobile-menu', 'aria-label': copy('shell.menu'), 'aria-controls': 'sidebar', 'aria-expanded': 'false'});
  const bar = h('header', {id: 'mobile-bar', hidden: true}, menu);
  const backdrop = h('div', {id: 'drawer-backdrop', hidden: true});
  const anchor = h('a', {href: '/nodes', dataset: {nav: '/nodes'}}, h('span', null, copy('shell.nodes')));
  const overview = h('a', {href: '/overview', dataset: {nav: '/overview'}}, copy('shell.overview'));
  const command = h('button', {id: 'command-palette', 'aria-haspopup': 'dialog'}, copy('shell.commandPalette'));
  const notifications = h('button', {id: 'notifications', class: 'notification-entry', 'aria-haspopup': 'dialog'},
    h('span', {class: 'notification-count'}), h('small', {class: 'notification-freshness'}));
  const sidebar = h('aside', {id: 'sidebar'}, h('a', {class: 'brand'}, 'OrbitBeacon'), overview, anchor, command, notifications, h('div', {id: 'account'}));
  const railClicks = [];
  const listen = sidebar.addEventListener.bind(sidebar);
  sidebar.addEventListener = (type, listener, options) => { if (type === 'click') railClicks.push(options); listen(type, listener, options); };
  const view = h('main', {id: 'view'}, h('h1', null, copy('shell.overview')));
  dom.body.append(bar, backdrop, sidebar, view, h('div', {id: 'progress'}), h('div', {id: 'toasts'}));
  setNavigationSignal(null); setSession(account);
  const shell = await import('../src/console/shell.js');
  const dispose = shell.initShell();
  let stopRouter;
  t.after(() => {
    stopRouter?.(); dispose();
    setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''});
    Object.defineProperty(reducedMotion, 'matches', motion);
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    dom.restore();
  });
  const route = async (page = async () => h('section', null, h('h1', null, copy('shell.nodes')))) => {
    const router = await import('../src/console/router.js');
    stopRouter = router.startRouter([{match: () => true, nav: '/nodes', page}]);
    await flush();
    return router;
  };
  return {...dom, menu, bar, backdrop, sidebar, view, anchor, overview, command, phone, user, requests, route, railClicks, desktop, compact, ...shell,
    resize(matches) { phone.matches = matches; phone.dispatchEvent(new Event('change')); }};
}

function state(env, opened, mobile = true) {
  assert.equal(env.menu.getAttribute('aria-expanded'), String(opened));
  assert.equal(env.view.inert, opened);
  assert.equal(env.sidebar.inert, mobile && !opened);
  assert.equal(env.backdrop.hidden, !opened);
  assert.equal(document.documentElement.classList.contains('drawer-open'), opened);
}
function press(key, props = {}) {
  const event = Object.assign(new Event('keydown', {cancelable: true}), {key, ...props});
  document.dispatchEvent(event); return event;
}

test('mobile menu toggles the existing sidebar, ARIA, inert, scroll lock and focus together', async (t) => {
  const env = await setup(t);
  assert.equal(env.bar.hidden, false); state(env, false);
  assert.equal(env.menu.getAttribute('aria-label'), copy('shell.menu'));
  assert.equal(env.menu.getAttribute('aria-controls'), env.sidebar.id);
  env.menu.click(); state(env, true);
  assert.equal(document.activeElement, env.overview, 'skip the CSS-hidden sidebar brand');
  env.menu.click(); state(env, false);
  assert.equal(document.activeElement, env.menu);
});

for (const reason of ['Escape', 'backdrop', 'link']) {
  test(`mobile drawer closes on ${reason} and returns focus to the menu`, async (t) => {
    const env = await setup(t); env.menu.click();
    if (reason === 'Escape') assert.equal(press('Escape').defaultPrevented, true);
    if (reason === 'backdrop') env.backdrop.click();
    if (reason === 'link') {
      const click = new Event('click');
      Object.defineProperty(click, 'target', {value: env.anchor.firstElementChild});
      env.sidebar.dispatchEvent(click);
    }
    state(env, false); assert.equal(document.activeElement, env.menu);
  });
}

test('route navigation closes the drawer without overriding router heading focus', async (t) => {
  const env = await setup(t); const router = await env.route();
  env.menu.click(); await router.go('/nodes');
  state(env, false);
  assert.equal(document.activeElement, env.view.querySelector('h1'));
  assert.notEqual(document.activeElement, env.menu);
});

test('navigation abort closes the drawer before the next page finishes loading', async (t) => {
  const env = await setup(t);
  let release;
  let loading = false;
  const router = await env.route(async () => {
    if (loading) await new Promise((resolve) => { release = resolve; });
    return h('section', null, h('h1', null, copy('shell.nodes')));
  });
  loading = true; env.menu.click();
  const pending = router.go('/nodes');
  state(env, false); assert.notEqual(document.activeElement, env.menu);
  await flush(); release(); await pending;
  assert.equal(document.activeElement, env.view.querySelector('h1'));
});

test('tablet resize clears all drawer state and returning to mobile keeps it closed', async (t) => {
  const env = await setup(t); env.menu.click(); env.resize(false);
  state(env, false, false);
  assert.equal(document.activeElement, env.menu);
  env.menu.click(); state(env, false, false);
  env.resize(true); state(env, false);
});

test('desktop shell starts without inert navigation or an active drawer', async (t) => {
  const env = await setup(t, {mobile: false});
  state(env, false, false); env.menu.click(); state(env, false, false);
});

test('sign-out hides the mobile bar and closes the drawer synchronously with session loss', async (t) => {
  const env = await setup(t); env.menu.click();
  document.getElementById('user-menu').click();
  document.getElementById('sign-out').click(); await flush();
  assert.ok(env.requests.includes('/api/v1/session'));
  assert.equal(env.bar.hidden, true); state(env, false);
  env.menu.click(); state(env, false);
});

test('forced-password and bare routes hide the mobile bar and cannot open navigation', async (t) => {
  const env = await setup(t); env.menu.click();
  setSession({user: {...env.user, must_change_password: true}, csrf_token: 'csrf', expires_at: ''});
  assert.equal(env.bar.hidden, true); state(env, false);
  setSession({user: env.user, csrf_token: 'csrf', expires_at: ''});
  for (const path of ['/login', '/change-password']) {
    location.pathname = path; env.setActiveNav();
    assert.equal(env.bar.hidden, true); env.menu.click(); state(env, false);
  }
});

test('drawer traps keyboard focus but leaves an open dialog to its native focus and Escape handling', async (t) => {
  const env = await setup(t); env.menu.click();
  env.menu.focus(); assert.equal(press('Tab', {shiftKey: true}).defaultPrevented, true);
  assert.equal(document.activeElement, document.getElementById('account').children[1]);
  assert.equal(press('Tab').defaultPrevented, true); assert.equal(document.activeElement, env.menu);
  const {modal} = await import('../src/console/ui/dialogs.js');
  const dialog = modal(copy('shell.commandPalette'), () => h('input'));
  assert.equal(press('Escape').defaultPrevented, false); state(env, true);
  dialog.close('done'); await dialog.closed;
  assert.equal(press('Escape').defaultPrevented, true); state(env, false);
});

test('dialog entries close the drawer first so a closing dialog returns focus to the menu', async (t) => {
  const env = await setup(t); env.menu.click(); env.command.focus();
  assert.ok(env.railClicks.every((options) => options?.capture === true));
  // 真实浏览器先走捕获阶段：抽屉收起后才执行按钮自身的打开逻辑。
  const click = new Event('click');
  Object.defineProperty(click, 'target', {value: env.command});
  env.sidebar.dispatchEvent(click);
  state(env, false);
  assert.equal(document.activeElement, env.menu);
  env.command.click(); await flush();
  const dialog = document.querySelector('dialog');
  assert.ok(dialog.open);
  assert.equal(dialog.querySelector('h2').textContent, copy('shell.commandPalette'));
  assert.equal(document.activeElement, dialog.querySelector('input'));
  dialog.dispatchEvent(new Event('cancel', {cancelable: true})); await flush();
  assert.equal(document.querySelector('dialog'), null);
  assert.equal(document.activeElement, env.menu);
  state(env, false);
});

test('a closing dialog abandons an opener the drawer turned inert', async (t) => {
  const env = await setup(t);
  const {modal} = await import('../src/console/ui/dialogs.js');
  // 模拟对话框打开期间视口跨越 767px：抽屉收起把侧栏置为 inert，入口不能再收回焦点。
  env.command.closest = (selector) => selector === '[inert]' ? env.sidebar : null;
  env.sidebar.inert = true;
  env.command.focus();
  const {close, closed} = modal(copy('shell.commandPalette'), () => h('input', {dataset: {autofocus: ''}}));
  const input = document.querySelector('dialog').querySelector('[data-autofocus]');
  assert.equal(document.activeElement, input);
  close('done'); await closed;
  assert.notEqual(document.activeElement, env.command, 'focus must not land on the inert opener');
  env.command.closest = undefined;
});

test('theme control stays out of the drawer close path', async (t) => {
  const env = await setup(t); env.menu.click();
  const theme = h('button', {class: 'icon-button'});
  env.sidebar.append(theme);
  const click = new Event('click');
  Object.defineProperty(click, 'target', {value: theme});
  env.sidebar.dispatchEvent(click);
  state(env, true);
});

test('pageHead keeps primary action separate from live status for the mobile title row', async () => {
  const dom = installDOM();
  try {
    const {button, liveStatus, pageHead} = await import('../src/console/ui/components.js');
    const status = liveStatus();
    const action = button(copy('nodes.add'), {variant: 'primary'});
    const header = pageHead(copy('nodes.listTitle'), status.element, action);
    assert.equal(header.querySelector('.page-title').querySelector('h1').textContent, copy('nodes.listTitle'));
    assert.equal(header.querySelector('.head-actions').firstElementChild, action);
    assert.equal(header.querySelector('.head-status').firstElementChild, status.element);
    status.update({state: 'live'});
    assert.equal(header.querySelector('.head-status').textContent, copy('controls.live.live'));
  } finally { dom.restore(); }
});

test('user menu exposes only theme radios, persists explicit themes and preserves the drawer', async (t) => {
  const env = await setup(t); env.menu.click();
  const root = document.querySelector('.user-menu');
  const trigger = document.getElementById('user-menu');
  const menu = root.querySelector('[role="menu"]');
  const radios = root.querySelectorAll('[role="menuitemradio"]');
  assert.equal(trigger.getAttribute('aria-label'), 'Admin');
  assert.equal(root.querySelector('.avatar').textContent, 'A');
  assert.equal(radios.length, 3);
  assert.deepEqual(radios.map((radio) => radio.getAttribute('aria-checked')), ['true', 'false', 'false']);
  trigger.click(); state(env, true);
  assert.equal(menu.hidden, false);
  const writes = [];
  localStorage.setItem = (key, value) => writes.push([key, value]);
  localStorage.removeItem = (key) => writes.push([key, null]);
  radios[2].click();
  assert.equal(document.documentElement.dataset.theme, 'dark');
  assert.equal(radios[2].getAttribute('aria-checked'), 'true');
  assert.equal(radios[0].getAttribute('aria-checked'), 'false');
  assert.deepEqual(writes, [['orbitbeacon-theme', 'dark']]);
  assert.equal(menu.hidden, true); state(env, true);
  trigger.click(); radios[0].click();
  assert.equal(document.documentElement.dataset.theme, undefined);
  assert.deepEqual(writes.at(-1), ['orbitbeacon-theme', null]);
  await env.route(); trigger.click();
  assert.equal(menu.hidden, false, 'route navigation must not dispose the persistent account menu');
});

test('account language toggle follows draft confirmation, cancellation, busy guard, storage and full reload', async (t) => {
  const env = await setup(t); await env.route();
  let preference = 'system';
  const writes = [];
  let reloads = 0;
  localStorage.getItem = () => preference;
  localStorage.setItem = (key, value) => { writes.push([key, value]); preference = value; };
  location.reload = () => reloads++;
  const dirty = h('form', {dataset: {dirty: 'true'}}, h('input', {value: 'draft'})); env.view.append(dirty);
  const language = document.getElementById('account').children[1];
  language.click(); await flush();
  assert.ok(document.querySelector('dialog'));
  assert.deepEqual(writes, []);
  document.querySelector('dialog').querySelector('button').click(); await flush();
  assert.equal(dirty.querySelector('input').value, 'draft'); assert.equal(reloads, 0);
  assert.equal(language.title, 'Switch to 中文');
  dirty.dataset.busy = 'true'; language.click(); await flush();
  assert.equal(document.querySelector('dialog'), null); assert.deepEqual(writes, []);
  delete dirty.dataset.busy;
  language.click(); await flush();
  const dialog = document.querySelector('dialog');
  dialog.querySelectorAll('button').find((button) => button.textContent === copy('controls.discard')).click(); await flush();
  assert.deepEqual(writes, [['orbitbeacon-locale', 'zh-CN']]); assert.equal(reloads, 1);
  assert.equal(location.pathname, '/overview');
});

test('login keeps language in the card header and required validation without visual asterisks', async (t) => {
  const env = await setup(t);
  await env.route();
  const {h} = await import('../src/shared/dom.js');
  const mark = h('span', {class: 'mark'}); mark.cloneNode = () => h('span', {class: 'mark'});
  env.sidebar.append(mark);
  globalThis.fetch = async () => Response.json({code: 'unauthenticated'}, {status: 401});
  setSession({user: null, csrf_token: '', expires_at: ''}); await flush(); location.pathname = '/login';
  const {loginPage} = await import('../src/console/pages/login.js');
  const page = await loginPage(); env.body.append(page);
  const card = page.querySelector('.login-card');
  const head = card.querySelector('.auth-card-head');
  assert.equal(card.firstElementChild, head);
  assert.equal(head.lastElementChild.className, 'icon-button');
  const language = head.lastElementChild;
  assert.ok(language.getAttribute('aria-label'));
  assert.equal(head.querySelector('select'), null);
  assert.ok(card.querySelector('h1'));
  const form = card.querySelector('form');
  assert.equal(form.querySelectorAll('.field-required').length, 0);
  for (const name of ['username', 'password']) {
    const input = form.querySelectorAll('input').find((item) => item.name === name);
    assert.equal(input.required, true);
    input.value = ''; input.dispatchEvent(new Event('input'));
    assert.ok(input.validationMessage);
  }
  let stored = 'system'; let reloads = 0;
  localStorage.getItem = () => stored;
  localStorage.setItem = (_key, value) => { stored = value; };
  location.reload = () => reloads++;
  language.click(); await flush();
  assert.equal(stored, 'zh-CN'); assert.equal(reloads, 1);
});

test('desktop collapse persists, repaints its icon and survives account re-rendering', async (t) => {
  const env = await setup(t, {mobile: false, nav: 'collapsed'});
  const root = document.documentElement;
  let toggle = document.querySelector('.nav-toggle');
  assert.equal(root.classList.contains('nav-collapsed'), true);
  assert.equal(toggle.hidden, false);
  assert.equal(toggle.getAttribute('aria-expanded'), 'false');
  assert.equal(toggle.title, copy('shell.expandNav'));
  assert.equal(toggle.querySelector('use').getAttribute('href'), '#i-sidebar-expand');
  const writes = [];
  localStorage.setItem = (key, value) => writes.push([key, value]);
  toggle.click();
  assert.equal(root.classList.contains('nav-collapsed'), false);
  assert.equal(toggle.getAttribute('aria-expanded'), 'true');
  assert.equal(toggle.title, copy('shell.collapseNav'));
  assert.deepEqual(writes, [['orbitbeacon.nav', 'expanded']]);
  localStorage.setItem = () => { throw new Error('Storage unavailable'); };
  toggle.click();
  assert.equal(root.classList.contains('nav-collapsed'), true);
  setSession({user: env.user, csrf_token: 'new', expires_at: ''});
  toggle = document.querySelector('.nav-toggle');
  assert.equal(toggle.getAttribute('aria-expanded'), 'false');
  assert.equal(document.getElementById('account').children.length, 3);
  assert.equal(document.querySelector('.who-name'), null);
});

test('tablet rail and mobile drawer hide collapse without losing the stored desktop choice', async (t) => {
  const env = await setup(t, {mobile: false, tablet: true, nav: 'collapsed'});
  const toggle = document.querySelector('.nav-toggle');
  assert.equal(toggle.hidden, true);
  assert.equal(document.documentElement.classList.contains('nav-compact'), true);
  env.compact.matches = false; env.compact.dispatchEvent(new Event('change'));
  env.resize(true);
  assert.equal(toggle.hidden, true);
  assert.equal(document.documentElement.classList.contains('nav-compact'), false);
  env.resize(false); env.desktop.matches = true; env.desktop.dispatchEvent(new Event('change'));
  assert.equal(toggle.hidden, false);
  assert.equal(toggle.getAttribute('aria-expanded'), 'false');
});

test('desktop navigation still collapses when preference storage cannot be read or written', async (t) => {
  await setup(t, {mobile: false, storageBlocked: true});
  localStorage.setItem = () => { throw new Error('Storage unavailable'); };
  const toggle = document.querySelector('.nav-toggle');
  toggle.click();
  assert.equal(document.documentElement.classList.contains('nav-collapsed'), true);
  toggle.click();
  assert.equal(document.documentElement.classList.contains('nav-collapsed'), false);
});
