import test from 'node:test';
import assert from 'node:assert/strict';
import {h, reducedMotion} from '../src/shared/dom.js';
import {invalidate, setNavigationSignal, setSession} from '../src/console/api.js';
import {closeDialogs, modal, hasUnsavedDialogChanges} from '../src/console/ui/dialogs.js';
import {alertTabs, consoleRoutes, settingsTabs} from '../src/console/routes.js';
import {copy, elements, flush, installDOM} from './console-dom.js';

const id = '00000000-0000-4000-8000-000000000001';
const nodePath = consoleRoutes.find((route) => route.page === 'node').path.replace('{id}', id);
const pagePath = (page) => consoleRoutes.find((route) => route.page === page).path;
const options = () => elements(document.body).filter((entry) => entry.getAttribute('role') === 'option');
const entry = () => document.body.querySelector('dialog').querySelector('input');
const type = (value) => { entry().value = value; entry().dispatchEvent(new Event('input')); };
const press = (target, key, props = {}) => {
  const event = Object.assign(new Event('keydown', {cancelable: true}), {key, ...props});
  target.dispatchEvent(event); return event;
};
async function setup(t, {fail = false, pending = false, nodes = [{id, name: '<Host Alpha>'}]} = {}) {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window', 'history', 'requestAnimationFrame', 'setTimeout', 'clearTimeout'].map((key) => [key, globalThis[key]]));
  const motion = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => true});
  globalThis.window = new EventTarget(); window.scrollTo = () => {};
  globalThis.requestAnimationFrame = () => 0;
  globalThis.setTimeout = () => 0; globalThis.clearTimeout = () => {};
  Object.assign(location, {pathname: pagePath('overview'), href: 'https://example.test/overview', origin: 'https://example.test', search: '', hash: ''});
  const setURL = (value) => {
    if (!value) return;
    const url = new URL(value, location.href);
    Object.assign(location, {pathname: url.pathname, search: url.search, hash: url.hash, href: url.href});
  };
  globalThis.history = {state: null, replaceState(_state, _unused, value) { setURL(value); }, pushState(_state, _unused, value) { setURL(value); }};
  const sidebar = h('div', {id: 'sidebar'});
  const trigger = h('button', {id: 'command-palette'});
  sidebar.append(trigger);
  dom.body.append(sidebar, h('div', {id: 'account'}), h('div', {id: 'view'}), h('div', {id: 'progress'}), h('div', {id: 'toasts'}));
  const requests = [];
  const user = {id, username: 'Admin', must_change_password: false};
  const account = {user, csrf_token: 'csrf', expires_at: ''};
  globalThis.fetch = async (url, options) => {
    requests.push({url, ...options});
    if (url === '/api/v1/me') return new Response(JSON.stringify(account));
    if (url === '/api/v1/nodes' && options.method === 'POST') return new Response(JSON.stringify({id, name: 'New Host'}));
    if (url === '/api/v1/enrollments') return new Response(JSON.stringify({token: 'test-deployment-secret', server_version: '1.2.3', expires_at: new Date(Date.now() + 1800000).toISOString()}));
    if (url.startsWith('/api/v1/nodes?')) {
      if (pending) return new Promise(() => {});
      if (fail) return new Response('{}', {status: 503});
      return new Response(JSON.stringify({nodes, has_more: false}));
    }
    throw new Error(`Unexpected request ${url}`);
  };
  setNavigationSignal(null); invalidate('nodes'); setSession(account);
  const router = await import('../src/console/router.js');
  const stop = router.startRouter(consoleRoutes.map((route) => ({...route,
    match: (path) => route.path === '/nodes/{id}' ? path === nodePath : path === route.path,
    page: async () => route.redirect || h('div', null, h('h1', null, location.pathname))
  })));
  await flush();
  const palette = await import('../src/console/command-palette.js');
  const dispose = palette.initCommandPalette();
  t.after(() => {
    dispose(); stop(); closeDialogs(); setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''});
    Object.defineProperty(reducedMotion, 'matches', motion);
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    dom.restore();
  });
  return {...dom, trigger, requests, ...palette, ...router};
}

test('command palette Ctrl/Cmd K works from inputs, prevents default, and guards IME and other dialogs', async (t) => {
  const env = await setup(t);
  const editing = h('input'); env.body.append(editing); editing.focus();
  for (const modifier of ['ctrlKey', 'metaKey']) {
    assert.equal(press(document, 'k', {[modifier]: true}).defaultPrevented, true);
    assert.equal(document.body.querySelector('dialog').querySelector('h2').textContent, copy('shell.commandPalette'));
    assert.equal(document.activeElement, entry());
    const count = env.requests.length;
    assert.equal(press(document, 'k', {[modifier]: true}).defaultPrevented, false);
    assert.equal(document.body.querySelectorAll('dialog').length, 1);
    assert.equal(env.requests.length, count);
    closeDialogs(); await flush();
  }
  assert.equal(press(document, 'k', {ctrlKey: true, isComposing: true}).defaultPrevented, false);
  assert.equal(document.body.querySelector('dialog'), null);
  const other = modal(copy('nodes.add'), () => h('input'));
  assert.equal(press(document, 'k', {metaKey: true}).defaultPrevented, false);
  assert.equal(document.body.querySelectorAll('dialog').length, 1);
  other.close('done'); await other.closed;
  env.trigger.click(); assert.ok(document.body.querySelector('dialog'));
});

test('command palette is absent from login and forced-password routes and unauthenticated sessions', async (t) => {
  const env = await setup(t);
  for (const page of ['login', 'changePassword']) {
    location.pathname = pagePath(page);
    assert.equal(press(document, 'k', {ctrlKey: true}).defaultPrevented, false);
    assert.equal(env.openCommandPalette(), undefined);
  }
  location.pathname = pagePath('nodes');
  setSession({user: {id, username: 'Admin', must_change_password: true}, csrf_token: 'csrf', expires_at: ''});
  assert.equal(env.trigger.hidden, true);
  assert.equal(press(document, 'k', {ctrlKey: true}).defaultPrevented, false);
  setSession({user: null, csrf_token: '', expires_at: ''});
  assert.equal(env.trigger.hidden, true);
  assert.equal(env.openCommandPalette(), undefined);
});

test('command palette includes route-derived pages and all settings and alert tabs, filters plain-text node names, and Enter navigates', async (t) => {
  const env = await setup(t); env.openCommandPalette(); await flush();
  const labels = options().map((option) => option.textContent);
  for (const route of consoleRoutes.filter((route) => route.label)) assert.ok(labels.includes(copy(route.label)));
  for (const [page, tabs] of [['alerts', alertTabs], ['settings', settingsTabs]]) {
    const route = consoleRoutes.find((route) => route.page === page);
    for (const tab of tabs) assert.ok(labels.includes(`${copy(route.label)} · ${copy(tab.label)}`));
  }
  assert.ok(labels.includes(copy('nodes.add')));
  assert.ok(labels.includes('<Host Alpha>'));
  type('ALPh'); assert.deepEqual(options().map((option) => option.textContent), ['<Host Alpha>']);
  assert.equal(document.querySelector('dialog').querySelector('Host'), null);
  assert.equal(hasUnsavedDialogChanges(), false);
  assert.equal(press(entry(), 'Enter', {isComposing: true}).defaultPrevented, false);
  assert.equal(location.pathname, pagePath('overview'));
  press(entry(), 'Enter'); await flush();
  assert.equal(location.pathname, nodePath);
  assert.equal(document.querySelector('dialog'), null);
});

test('command palette arrows and Enter navigate tab links through the shared listbox', async (t) => {
  const env = await setup(t); env.openCommandPalette(); await flush();
  type(copy('shell.settings'));
  const filtered = options(); assert.ok(filtered.length > 1);
  press(entry(), 'ArrowDown'); press(entry(), 'ArrowDown'); press(entry(), 'ArrowUp');
  assert.equal(entry().getAttribute('aria-activedescendant'), filtered[1].id);
  press(entry(), 'Enter'); await flush();
  assert.equal(location.pathname, pagePath('settings')); assert.equal(location.hash, '#account');
});

test('command palette read failures leave page and tab navigation available', async (t) => {
  const env = await setup(t, {fail: true}); env.openCommandPalette(); await flush();
  const error = document.querySelector('dialog').querySelector('.form-error');
  assert.equal(error.hidden, false); assert.equal(error.textContent, copy('errors.generic'));
  type(copy('settings.audit'));
  assert.equal(options().length, 1);
  press(entry(), 'Enter'); await flush();
  assert.equal(location.pathname, pagePath('settings')); assert.equal(location.hash, '#audit');
});

test('command palette close, navigation and Escape abort pending node reads without a discard prompt', async (t) => {
  const env = await setup(t, {pending: true});
  const read = () => env.requests.findLast((request) => request.url.startsWith('/api/v1/nodes?'));
  const dialog = env.openCommandPalette(); await flush();
  type('draft query'); assert.equal(hasUnsavedDialogChanges(), false);
  dialog.close(); await dialog.closed; assert.equal(read().signal.aborted, true);
  env.openCommandPalette(); await flush();
  const request = read(); await env.go(pagePath('nodes'));
  assert.equal(request.signal.aborted, true); assert.equal(document.querySelector('dialog'), null);
  env.openCommandPalette(); await flush();
  const escape = press(entry(), 'Escape'); assert.equal(escape.defaultPrevented, false);
  assert.equal(entry().getAttribute('aria-expanded'), 'true');
  document.querySelector('dialog').dispatchEvent(new Event('cancel', {cancelable: true})); await flush();
  assert.equal(read().signal.aborted, true); assert.equal(document.querySelector('dialog'), null);
});

test('command palette add-node creates once, deploys in a second dialog, then navigates to enrollment', async (t) => {
  const env = await setup(t); env.openCommandPalette(); await flush();
  type(copy('nodes.add')); assert.equal(options().length, 1);
  press(entry(), 'Enter'); await flush();
  const dialog = document.querySelector('dialog');
  assert.equal(dialog.querySelector('h2').textContent, copy('nodes.add'));
  const name = dialog.querySelector('input'); name.value = 'New Host';
  const form = dialog.querySelector('form');
  form.dispatchEvent(new Event('submit', {cancelable: true}));
  form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
  const deployment = document.querySelector('dialog');
  assert.notEqual(deployment, dialog); assert.equal(deployment.querySelectorAll('code').length, 2);
  assert.equal(location.pathname, pagePath('overview'));
  const posts = env.requests.filter((request) => request.url === '/api/v1/nodes' && request.method === 'POST');
  assert.equal(posts.length, 1); assert.deepEqual(JSON.parse(posts[0].body), {name: 'New Host'});
  assert.deepEqual(JSON.parse(env.requests.find((request) => request.url === '/api/v1/enrollments').body), {node_id: id});
  deployment.querySelectorAll('button').find((button) => button.textContent === copy('nodes.close')).click(); await flush();
  assert.equal(location.pathname, nodePath); assert.equal(location.hash, '');
  assert.equal(document.querySelector('dialog'), null);
});

test('command palette retains typed filtering when node reads finish and paginates the existing inventory API', async (t) => {
  const env = await setup(t);
  let release;
  const original = globalThis.fetch;
  const reads = [];
  globalThis.fetch = async (url, options) => {
    if (!url.startsWith('/api/v1/nodes?')) return original(url, options);
    reads.push({url, ...options});
    if (reads.length === 1) return new Promise((resolve) => { release = () => resolve(new Response(JSON.stringify({nodes: [{id, name: 'Alpha node'}], has_more: true, next_offset: 100}))); });
    return new Response(JSON.stringify({nodes: [{id: id.replace(/1$/, '2'), name: 'Beta node'}], has_more: false}));
  };
  env.openCommandPalette(); type('bet'); await flush();
  assert.equal(options().length, 0);
  release(); await flush();
  assert.equal(reads.length, 2); assert.ok(reads[1].url.endsWith('offset=100'));
  assert.deepEqual(options().map((option) => option.textContent), ['Beta node']);
  assert.equal(entry().value, 'bet');
});

test('creation failure never issues a deployment credential', async (t) => {
  const env = await setup(t); const original = globalThis.fetch;
  globalThis.fetch = async (url, options) => url === '/api/v1/nodes' && options.method === 'POST'
    ? new Response(JSON.stringify({message: 'Create failed'}), {status: 500}) : original(url, options);
  const {createNodeDialog} = await import('../src/console/lib/add-node-dialog.js');
  await createNodeDialog(); const dialog = document.querySelector('dialog'); dialog.querySelector('input').value = 'New Host';
  dialog.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true})); await flush();
  assert.equal(document.querySelector('dialog'), dialog);
  const error = dialog.querySelector('.form-error');
  assert.equal(error.hidden, false); assert.ok(error.textContent);
  assert.equal(env.requests.some((request) => request.url === '/api/v1/enrollments'), false);
});

test('failed issuance preserves the new node and navigates to its enrollment page', async (t) => {
  const env = await setup(t); const original = globalThis.fetch;
  globalThis.fetch = async (url, options) => url === '/api/v1/enrollments'
    ? new Response(JSON.stringify({message: 'Issue failed'}), {status: 500}) : original(url, options);
  const {createNodeDialog} = await import('../src/console/lib/add-node-dialog.js');
  await createNodeDialog(); const dialog = document.querySelector('dialog'); dialog.querySelector('input').value = 'New Host';
  dialog.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true})); await flush(); await flush();
  assert.equal(location.pathname, nodePath); assert.equal(location.hash, '');
  assert.ok(document.querySelector('#toasts').textContent.includes(copy('nodes.enrollment.issueFailedAfterCreate')));
  assert.equal(env.requests.filter((request) => request.url === '/api/v1/nodes' && request.method === 'POST').length, 1);
  assert.equal(env.requests.some((request) => request.method === 'DELETE'), false);
});

for (const phase of ['create', 'issue']) for (const boundary of ['close', 'navigation', 'session']) {
  if (phase === 'issue' && boundary === 'close') continue;
  test(`node deployment ignores late ${phase} response after ${boundary}`, async (t) => {
    const env = await setup(t); const original = globalThis.fetch; let release;
    globalThis.fetch = async (url, options) => {
      if ((phase === 'create' && url === '/api/v1/nodes' && options.method === 'POST') || (phase === 'issue' && url === '/api/v1/enrollments')) {
        return new Promise((resolve) => { release = () => resolve(new Response(JSON.stringify(phase === 'create' ? {id, name: 'New Host'} : {token: 'late-fake-secret', server_version: '1.2.3', expires_at: new Date(Date.now() + 1800000).toISOString()}))); });
      }
      return original(url, options);
    };
    const {createNodeDialog} = await import('../src/console/lib/add-node-dialog.js');
    await createNodeDialog(); const dialog = document.querySelector('dialog'); dialog.querySelector('input').value = 'New Host';
    dialog.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(typeof release, 'function');
    if (boundary === 'close') closeDialogs();
    if (boundary === 'navigation') await env.go(pagePath('settings'), {force: true});
    if (boundary === 'session') setSession({user: {id: 'other-admin', username: 'Other', must_change_password: false}, csrf_token: 'changed', expires_at: ''});
    const expectedPath = location.pathname;
    release(); await flush(); await flush();
    assert.equal(document.querySelector('dialog'), null);
    assert.equal(document.body.textContent.includes('late-fake-secret'), false);
    assert.equal(document.querySelector('#toasts').textContent.includes(copy('nodes.enrollment.issueFailedAfterCreate')), false);
    assert.equal(location.pathname, expectedPath);
  });
}

test('search stays top-anchored, provides a placeholder and labelled icon groups with continuous keyboard navigation', async (t) => {
  const env = await setup(t); env.openCommandPalette(); await flush();
  const dialog = document.querySelector('dialog');
  assert.equal(dialog.classList.contains('command-dialog'), true);
  assert.equal(entry().getAttribute('placeholder'), copy('shell.searchPlaceholder'));
  const groups = dialog.querySelectorAll('[role="group"]');
  assert.deepEqual(groups.map((group) => document.getElementById(group.getAttribute('aria-labelledby')).textContent),
    [copy('shell.pages'), copy('shell.nodes'), copy('shell.actions')]);
  for (const option of options()) assert.ok(option.querySelector('svg'));
  const all = options();
  const pageCount = groups[0].querySelectorAll('[role="option"]').length;
  for (let index = 0; index < pageCount; index++) press(entry(), 'ArrowDown');
  assert.equal(entry().getAttribute('aria-activedescendant'), all[pageCount].id);
  press(entry(), 'ArrowDown');
  assert.equal(entry().getAttribute('aria-activedescendant'), all[pageCount + 1].id);
  press(entry(), 'ArrowUp');
  assert.equal(entry().getAttribute('aria-activedescendant'), all[pageCount].id);
});


test('legacy /data replaces its history entry with the storage section without creating a data page', async (t) => {
  const env = await setup(t);
  const replace = history.replaceState;
  const writes = [];
  history.replaceState = (...args) => { writes.push(args[2]); replace(...args); };
  await env.go('/data');
  assert.equal(location.pathname, '/settings');
  assert.equal(location.hash, '#data-storage');
  assert.ok(writes.includes('/settings#data-storage'));
});


test('new-node View node link navigates to the overview without the post-dialog enrollment redirect', async (t) => {
  const env = await setup(t);
  const originalFetch = globalThis.fetch;
  const account = {user: {id, username: 'Admin', must_change_password: false}, csrf_token: 'csrf', expires_at: '2099-01-01T00:00:00Z'};
  globalThis.fetch = (url, options) => url === '/api/v1/me' ? Promise.resolve(Response.json(account)) : originalFetch(url, options);
  setSession(account); await flush();
  const previousSocket = globalThis.WebSocket;
  const sockets = [];
  globalThis.WebSocket = class extends EventTarget {
    constructor() { super(); sockets.push(this); }
    close() { this.closed = true; }
  };
  t.after(() => { if (previousSocket === undefined) delete globalThis.WebSocket; else globalThis.WebSocket = previousSocket; });
  const {createNodeDialog} = await import('../src/console/lib/add-node-dialog.js');
  await createNodeDialog();
  const create = document.querySelector('dialog'); create.querySelector('input').value = 'New Host';
  create.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true})); await flush();
  const deployment = document.querySelector('dialog');
  const socket = sockets[0]; assert.ok(socket);
  socket.dispatchEvent(new Event('open'));
  const node = {id, name: 'New Host', notes: '', region: '', provider: '', group_id: null, tags: [], created_at: '2026-11-09T00:00:00Z',
    status: 'online', presence: 'online', stopped: false, last_seen: '2026-11-09T00:00:00Z', agent_version: '1.2.3'};
  const snapshot = (data) => socket.dispatchEvent(Object.assign(new Event('message'), {data: JSON.stringify({type: 'snapshot', server_time: '2026-11-09T00:00:00Z', update_interval_seconds: 10,
    data: {node: data, latest: null}})}));
  // 新建节点先以 pending 出现，Agent 上报后才视为成功。
  snapshot({...node, status: 'pending', presence: 'pending', last_seen: null, agent_version: ''}); await flush();
  snapshot(node); await flush();
  const view = deployment.querySelector('a'); assert.ok(view);
  assert.equal(view.getAttribute('href'), nodePath);
  // The tiny DOM does not bubble. Dispatch the same activation to the document router.
  view.href = `${location.origin}${view.getAttribute('href')}`;
  view.closest = (selector) => selector === 'a[data-link]' ? view : null;
  view.click();
  const click = new Event('click', {cancelable: true});
  Object.defineProperties(click, {target: {value: view}, button: {value: 0}});
  document.dispatchEvent(click); await flush(); await flush();
  assert.equal(click.defaultPrevented, true);
  assert.equal(location.pathname, nodePath); assert.equal(location.hash, '');
  assert.equal(document.querySelector('dialog'), null); assert.equal(socket.closed, true);
});
