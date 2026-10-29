import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import * as client from '../src/console/api.js';
import {errorText} from '../src/shared/errors.js';
import {flush, TestElement} from './console-dom.js';
import {frame, liveHarness, response, stream} from './live-harness.js';

const user = {id: '00000000-0000-4000-8000-000000000001', username: 'administrator', must_change_password: false};
let fixtureProof = '';
const proof = (extra = {}) => ({user, csrf_token: fixtureProof, expires_at: '2026-10-28T00:10:00Z', ...extra});
const me = (value = proof(), time = 'Wed, 28 Oct 2026 00:00:00 GMT') => response(value, {headers: {Date: time}});
const fleet = () => frame('snapshot', {data: {nodes: [{id: user.id, status: 'unknown', presence: 'offline', last_seen: '2026-10-27T23:59:00Z', agent_version: 'test'}]}});
function setup(t, respond) {
  fixtureProof = `session:${t.name}`;
  const env = liveHarness(t, respond);
  location.pathname = '/overview';
  location.origin = 'https://example.test';
  const page = new AbortController();
  client.setNavigationSignal(page.signal);
  client.setSession({user: null, csrf_token: '', expires_at: ''});
  return {...env, page, clean() { page.abort(); client.setSession({user: null, csrf_token: '', expires_at: ''}); client.setNavigationSignal(null); }};
}

test('API client classifies network and unreadable failures without exposing transport wording', async (t) => {
  const env = setup(t, () => { throw new TypeError('private transport details'); });
  try {
    await assert.rejects(client.api('/nodes'), (error) => error.code === 'network');
    globalThis.fetch = async () => new Response('not json', {status: 200});
    await assert.rejects(client.api('/nodes'), (error) => error.code === 'unreadable_response');
    const controller = new AbortController(); controller.abort();
    await assert.rejects(client.api('/nodes', {signal: controller.signal}), (error) => error.name === 'AbortError');
  } finally { env.clean(); }
});

test('session replacement aborts old requests and fences late paginated cache results', async (t) => {
  let release;
  const env = setup(t, () => new Promise((resolve) => { release = resolve; }));
  try {
    client.setSession(proof());
    const old = client.allNodes();
    const rejected = assert.rejects(old, (error) => error.name === 'AbortError');
    const oldSignal = env.requests[0].signal;
    client.setSession(proof({csrf_token: 'session-two'}));
    assert.equal(oldSignal.aborted, true);
    release(response({nodes: [{id: 'secret-old-node'}], has_more: true, next_offset: 100}));
    await rejected;
    assert.equal(env.requests.length, 1, 'old epoch must not continue pagination');
    globalThis.fetch = async () => response({nodes: [{id: 'current-node'}], has_more: false});
    assert.deepEqual(await client.allNodes(), [{id: 'current-node'}]);
  } finally { env.clean(); }
});

test('same-session revalidation and username updates preserve the active session lifetime', async (t) => {
  const env = setup(t, () => me());
  try {
    client.setSession(proof());
    const signal = client.getSessionSignal();
    const epoch = client.getSessionEpoch();
    const cached = await client.cached('private-test', 60000, async () => ({draft: 'still here'}));
    await client.revalidateSession({signal: env.page.signal});
    client.updateUser({username: 'renamed'});
    assert.equal(client.getSessionEpoch(), epoch);
    assert.equal(client.getSessionSignal(), signal);
    assert.equal(signal.aborted, false);
    assert.equal(await client.cached('private-test', 60000, () => assert.fail('same proof cleared cache')), cached);
  } finally { env.clean(); }
});

test('password-change-required403 preserves credentials but aborts and clears the private lifetime', async (t) => {
  let release;
  const env = setup(t, (request) => request.url === '/api/v1/nodes'
    ? response({code: 'password_change_required'}, {status: 403})
    : new Promise((resolve) => { release = resolve; }));
  const details = [];
  const unsubscribe = client.onSession((_session, detail) => details.push(detail));
  try {
    client.setSession(proof());
    const previous = client.getSessionSignal();
    const epoch = client.getSessionEpoch();
    await client.cached('settled-private', 60000, async () => 'private');
    const pending = client.api('/me');
    const rejected = assert.rejects(pending, (error) => error.name === 'AbortError');
    await assert.rejects(client.api('/nodes'), (error) => error.code === 'password_change_required');
    assert.equal(previous.aborted, true);
    assert.equal(client.getSessionEpoch(), epoch + 1);
    assert.equal(client.session.csrf, fixtureProof);
    assert.equal(client.session.expires, proof().expires_at);
    assert.equal(client.session.user.must_change_password, true);
    assert.equal(details.at(-1).reason, 'password-change-required');
    assert.equal(details.at(-1).boundary, true);
    assert.equal(await client.cached('settled-private', 60000, async () => 'fresh'), 'fresh');
    release(me()); await rejected;
    assert.equal(client.session.user.must_change_password, true);
    globalThis.fetch = async (_url, options) => {
      assert.equal(options.headers['X-CSRF-Token'], fixtureProof);
      return new Response(null, {status: 204});
    };
    await client.api('/me/password', {method: 'PATCH', body: {old_password: 'old-password', new_password: 'new-password'}});
  } finally { unsubscribe(); env.clean(); }
});

test('restricted session rejects resolved old me and cannot downgrade before a fresh login', async (t) => {
  const env = setup(t, () => me());
  try {
    client.setSession(proof());
    const old = await client.api('/me');
    client.setSession(proof({user: {...user, must_change_password: true}}));
    assert.throws(() => client.setSession(old), (error) => error.name === 'AbortError');
    await client.revalidateSession();
    assert.equal(client.session.user.must_change_password, true, 'a newer me cannot reopen the restricted lifetime');
    client.setSession(await client.api('/session', {method: 'POST', body: {username: user.username, password: 'changed-password'}}));
    assert.equal(client.session.user.must_change_password, false, 'only fresh login can restore management access');
  } finally { env.clean(); }
});

test('session payload requires an explicit boolean password-change flag', async (t) => {
  let flag;
  const env = setup(t, () => me(proof({user: {...user, must_change_password: flag}})));
  try {
    for (flag of [undefined, null, 0, 'false']) {
      await assert.rejects(client.revalidateSession(), (error) => error.status === 200);
      assert.equal(client.session.user, null);
    }
  } finally { env.clean(); }
});

test('restricted sessions never activate live channels or notification polling', async (t) => {
  const env = setup(t, () => assert.fail('restricted state must not request private data'));
  let dispose;
  try {
    client.setSession(proof({user: {...user, must_change_password: true}}));
    const {connectConsoleLive} = await import('../src/console/live.js');
    const {initNotifications} = await import('../src/console/notifications.js');
    dispose = initNotifications();
    const live = connectConsoleLive({signal: env.page.signal, onSnapshot() { assert.fail('restricted snapshot'); }});
    await flush();
    assert.equal(env.requests.length, 0);
    assert.equal(env.sockets.length, 0);
    live.close();
  } finally { dispose?.(); env.clean(); }
});

test('router fences private DOM and redirects restricted sessions before loading management pages', async (t) => {
  const env = setup(t, () => me(proof({user: {...user, must_change_password: true}})));
  const originals = new Map(['matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  globalThis.matchMedia = () => ({matches: true, addEventListener() {}});
  globalThis.requestAnimationFrame = () => 0;
  globalThis.history = {state: {}, replaceState(_state, _unused, target) { if (target) location.pathname = target.split('?')[0]; }, pushState(_state, _unused, target) { location.pathname = target; }};
  env.window.scrollTo = () => {};
  for (const id of ['view', 'sidebar', 'account', 'progress', 'toasts']) {
    const element = new TestElement('div'); element.id = id; env.body.append(element);
  }
  let stop;
  let privateLoads = 0;
  try {
    client.setSession(proof());
    const router = await import('../src/console/router.js?restricted-boundary');
    const content = (text) => { const element = new TestElement('h1'); element.append(text); return element; };
    stop = router.startRouter([
      {match: (path) => path === '/login', bare: true, page: async () => content('login')},
      {match: (path) => path === '/change-password', bare: true, page: async () => content('password form')},
      {match: () => true, page: async () => { privateLoads += 1; return content('private draft'); }}
    ]);
    await flush();
    assert.equal(privateLoads, 1);
    client.setSession(proof({user: {...user, must_change_password: true}}));
    assert.equal(document.getElementById('view').textContent, '', 'private DOM disappears synchronously');
    await flush();
    assert.equal(location.pathname, '/change-password');
    assert.equal(document.getElementById('view').textContent, 'password form');
    await router.go('/settings');
    assert.equal(location.pathname, '/change-password');
    assert.equal(privateLoads, 1, 'restricted navigation never calls a private page loader');
    await router.go('/login');
    assert.equal(location.pathname, '/change-password');
    globalThis.fetch = async () => response({code: 'unauthenticated'}, {status: 401});
    client.invalidateSession('logout'); await flush();
    await router.go('/change-password');
    assert.equal(location.pathname, '/login');
    globalThis.fetch = async () => me();
    client.setSession(await client.api('/session', {method: 'POST', body: {}}));
    await flush();
    await router.go('/change-password');
    assert.equal(location.pathname, '/overview');
  } finally {
    stop?.(); env.clean();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});

test('required password page focuses field errors, preserves CSRF, and clears secrets before sign-out', async (t) => {
  let failure = 'invalid_old_password';
  const env = setup(t, (request) => {
    assert.equal(request.url, '/api/v1/me/password');
    assert.equal(request.headers['X-CSRF-Token'], fixtureProof);
    return failure ? response({code: failure, message: 'untranslated backend detail'}, {status: 400}) : new Response(null, {status: 204});
  });
  const sidebar = new TestElement('div'); sidebar.id = 'sidebar';
  const mark = new TestElement('span'); mark.className = 'mark'; mark.cloneNode = () => new TestElement('span');
  sidebar.append(mark); env.body.append(sidebar);
  let unsubscribe;
  try {
    client.setSession(proof({user: {...user, must_change_password: true}}));
    const {changePasswordPage} = await import('../src/console/pages/change-password.js');
    const page = await changePasswordPage(); env.body.append(page);
    const form = page.querySelector('form');
    const controls = form.querySelectorAll('input');
    const fill = () => controls.forEach((control, index) => { control.value = index === 0 ? 'initial-password' : 'changed-password'; });
    for (const code of ['invalid_old_password', 'password_unchanged']) {
      failure = code; fill();
      form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
      assert.equal(document.activeElement, controls[code === 'invalid_old_password' ? 0 : 1]);
      assert.equal(form.querySelector('.form-error').hidden, false);
      assert.equal(form.textContent.includes('untranslated backend detail'), false);
      assert.equal(controls.every((control) => control.value === ''), true);
      assert.equal(client.session.user.must_change_password, true);
    }
    unsubscribe = client.onSession((_session, detail) => {
      if (detail.reason === 'password') assert.equal(controls.every((control) => control.value === ''), true);
    });
    failure = ''; fill();
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(client.session.user, null);
    assert.equal(controls.every((control) => control.value === ''), true);
  } finally { unsubscribe?.(); env.clean(); }
});

test('known expiry aborts private requests offline using server time despite a backwards wall clock', async (t) => {
  const env = setup(t, () => me(proof({expires_at: '2026-10-28T00:00:05Z'})));
  try {
    await client.revalidateSession({signal: env.page.signal});
    const signal = client.getSessionSignal();
    env.shiftWall(-86400000);
    globalThis.fetch = async () => { throw new Error('offline'); };
    await env.advance(5000);
    assert.equal(client.session.user, null);
    assert.equal(signal.aborted, true);
    assert.equal(client.session.csrf, '');
  } finally { env.clean(); }
});

test('session expiry conservatively includes the me round trip rather than extending an old HTTP Date', async (t) => {
  let release;
  const env = setup(t, () => new Promise((resolve) => { release = resolve; }));
  try {
    const pending = client.revalidateSession();
    await env.advance(3000);
    release(me(proof({expires_at: '2026-10-28T00:00:05Z'})));
    await pending;
    await env.advance(2000);
    assert.equal(client.session.user, null);
  } finally { env.clean(); }
});

test('locally expired proof cannot be resurrected by automatic me but a fresh login can establish it', async (t) => {
  let value;
  const env = setup(t, () => me(value));
  value = proof({expires_at: '2026-10-28T00:00:02Z'});
  try {
    await client.revalidateSession();
    await env.advance(2000);
    assert.equal(client.session.user, null);
    await assert.rejects(client.revalidateSession(), (error) => error.status === 401);
    assert.equal(client.session.user, null);
    globalThis.fetch = async () => me(proof({csrf_token: 'fresh-login', expires_at: '2026-10-28T00:10:00Z'}), 'Wed, 28 Oct 2026 00:00:02 GMT');
    client.setSession(await client.api('/session', {method: 'POST', body: {username: 'administrator', password: 'new-password'}, noRedirect: true}));
    assert.equal(client.session.csrf, 'fresh-login');
  } finally { env.clean(); }
});

test('management 401 invalidates its epoch while login failure remains a form error', async (t) => {
  const env = setup(t, () => response({message: 'Unauthorized'}, {status: 401}));
  try {
    client.setSession(proof());
    await assert.rejects(client.api('/nodes'), (error) => error.status === 401);
    assert.equal(client.session.user, null);
    const epoch = client.getSessionEpoch();
    location.pathname = '/login';
    await assert.rejects(client.api('/session', {method: 'POST', noRedirect: true, body: {username: 'bad', password: 'bad'} }), (error) => error.status === 401);
    assert.equal(client.getSessionEpoch(), epoch);
  } finally { env.clean(); }
});

test('expiry synchronously clears private drafts and body dialogs without a dirty discard prompt', async (t) => {
  let value;
  const env = setup(t, () => me(value));
  value = proof({expires_at: '2026-10-28T00:00:03Z'});
  const originals = new Map(['matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  globalThis.matchMedia = () => ({matches: true, addEventListener() {}});
  globalThis.requestAnimationFrame = () => 0;
  globalThis.history = {state: {}, replaceState(_state, _unused, target) { if (target) location.pathname = target.split('?')[0]; }, pushState(_state, _unused, target) { location.pathname = target; }};
  env.window.scrollTo = () => {};
  env.document.querySelectorAll = (selector) => env.body.querySelectorAll(selector);
  for (const id of ['view', 'sidebar', 'account', 'progress', 'toasts']) {
    const element = new TestElement('div'); element.id = id; env.body.append(element);
  }
  let stopRouter;
  try {
    const {startRouter} = await import('../src/console/router.js?session-expiry');
    const {modal} = await import('../src/console/ui/dialogs.js');
    client.setSession(value);
    const secret = new TestElement('form'); secret.dataset.dirty = 'true'; secret.append('private draft');
    const login = new TestElement('h1'); login.append('Sign in');
    stopRouter = startRouter([{match: (path) => path === '/login', bare: true, page: async () => login}, {match: () => true, page: async () => secret}]);
    await flush();
    await client.revalidateSession();
    assert.equal(env.body.querySelector('form'), secret, 'same proof must not replace the dirty page');
    modal('Private ticket', () => { const text = new TestElement('p'); text.append('private dialog token'); return text; });
    document.getElementById('toasts').append('private toast token');
    env.document.hidden = true; env.document.dispatchEvent(new Event('visibilitychange'));
    await env.advance(3000);
    assert.equal(env.body.textContent.includes('private draft'), false);
    assert.equal(env.body.textContent.includes('private dialog token'), false);
    assert.equal(env.body.querySelectorAll('dialog').length, 0);
    assert.equal(location.pathname, '/login');
    assert.equal(env.body.textContent.includes('private toast token'), false);
    assert.equal(env.body.querySelector('dialog'), null);
  } finally {
    stopRouter?.();
    await flush();
    env.clean();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});

test('console live restoration verifies me before creating a new private channel', async (t) => {
  let release;
  let hold = false;
  const env = setup(t, (request) => {
    if (request.url === '/api/v1/me') return hold ? new Promise((resolve) => { release = resolve; }) : me();
    throw new Error(`Unexpected request ${request.url}`);
  });
  try {
    client.setSession(proof());
    const {connectConsoleLive} = await import('../src/console/live.js');
    const snapshots = [];
    const live = connectConsoleLive({signal: env.page.signal, onSnapshot: (data) => snapshots.push(data)});
    await flush();
    assert.equal(env.sockets.length, 1);
    env.sockets[0].open(); env.sockets[0].message(fleet());
    env.document.hidden = true; env.document.dispatchEvent(new Event('visibilitychange'));
    assert.equal(env.sockets[0].closed, true);
    hold = true;
    env.document.hidden = false; env.document.dispatchEvent(new Event('visibilitychange'));
    await flush();
    assert.equal(env.sockets.length, 1, 'inner shared controller must not auto-resume ahead of me');
    release(me()); await flush();
    assert.equal(env.sockets.length, 2);
    env.sockets[0].message(fleet());
    assert.equal(snapshots.length, 1, 'old controller callbacks are fenced');
    live.close();
  } finally { env.clean(); }
});

test('console websocket handshake denial verifies me and a 401 never falls back', async (t) => {
  let rejected = false;
  const env = setup(t, (request) => request.url === '/api/v1/me' ? rejected ? response({message: 'Unauthorized'}, {status: 401}) : me() : assert.fail('no fallback after401'));
  try {
    client.setSession(proof());
    const {connectConsoleLive} = await import('../src/console/live.js');
    connectConsoleLive({signal: env.page.signal, onSnapshot() {}});
    await flush();
    const previous = env.requests.filter((request) => request.url === '/api/v1/me').length;
    rejected = true;
    env.sockets[0].fail(); await flush();
    assert.equal(env.requests.filter((request) => request.url === '/api/v1/me').length, previous + 1);
    assert.equal(client.session.user, null);
    assert.equal(env.requests.some((request) => request.url.includes('/live/stream')), false);
    assert.equal(env.sockets.length, 1);
  } finally { env.clean(); }
});

test('console fallback keeps an offline fleet snapshot until known expiry and tears down waiting HTTP', async (t) => {
  const sse = stream();
  let value;
  const env = setup(t, (request) => request.url === '/api/v1/me' ? me(value)
    : request.url.includes('/stream') ? sse.response : response(frame('snapshot', {update_interval_seconds: 60, data: fleet().data})));
  value = proof({expires_at: '2026-10-28T00:00:05Z'});
  try {
    client.setSession(value);
    const {connectConsoleLive} = await import('../src/console/live.js');
    const snapshots = [];
    connectConsoleLive({signal: env.page.signal, onSnapshot: (data) => snapshots.push(data)});
    await flush(); env.sockets[0].fail(); await flush(); sse.error(); await flush();
    assert.equal(snapshots.length, 1);
    assert.equal(snapshots[0].nodes[0].presence, 'offline');
    await env.advance(5000);
    assert.equal(client.session.user, null);
    await env.advance(60000);
    assert.equal(snapshots.length, 1);
    assert.equal(env.requests.filter((request) => request.url === '/api/v1/live').length, 1);
  } finally { env.clean(); }
});

test('logout clears the session and fences cached requests before its revoke response arrives', async (t) => {
  let releaseRevoke;
  let releaseNodes;
  const env = setup(t, (request) => new Promise((resolve) => { if (request.method === 'DELETE') releaseRevoke = resolve; else releaseNodes = resolve; }));
  try {
    client.setSession(proof());
    const old = client.allNodes();
    const rejected = assert.rejects(old, (error) => error.name === 'AbortError');
    const logout = client.signOut();
    assert.equal(client.session.user, null);
    assert.equal(env.requests[0].signal.aborted, true);
    releaseNodes(response({nodes: [{id: 'private-old'}], has_more: false}));
    releaseRevoke(new Response(null, {status: 204}));
    await Promise.all([logout, rejected]);
  } finally { env.clean(); }
});

test('successful account password change invalidates the shared session and removes open private dialogs', async (t) => {
  const env = setup(t, (request) => {
    if (request.url === '/api/v1/me/password') return new Response(null, {status: 204});
    if (request.url === '/api/v1/users') return response({users: [user]});
    if (request.url === '/api/v1/groups') return response({groups: []});
    if (request.url.startsWith('/api/v1/nodes?')) return response({nodes: [], has_more: false});
    return response({events: [], has_more: false});
  });
  const originals = new Map(['history', 'requestAnimationFrame', 'matchMedia'].map((key) => [key, globalThis[key]]));
  globalThis.history = {state: {}, replaceState() {}};
  globalThis.requestAnimationFrame = () => 0;
  globalThis.matchMedia = () => ({matches: true, addEventListener() {}});
  location.pathname = '/settings'; location.hash = '#account';
  try {
    client.setSession(proof());
    const signal = client.getSessionSignal();
    const {settingsPage} = await import('../src/console/pages/settings.js');
    const {modal} = await import('../src/console/ui/dialogs.js');
    const page = await settingsPage(); env.body.append(page);
    modal('Private token', () => { const element = new TestElement('p'); element.append('never keep this token'); return element; });
    const form = page.querySelector('form');
    form.querySelectorAll('input').forEach((input) => { input.value = 'new strong password'; });
    form.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.equal(client.session.user, null);
    assert.equal(signal.aborted, true);
    assert.equal(env.body.querySelectorAll('dialog').length, 0);
    assert.equal(env.body.textContent.includes('never keep this token'), false);
    assert.equal(form.querySelectorAll('input').every((input) => input.value === ''), true);
  } finally {
    env.clean();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});

test('notifications resume only after the shared me gate and keep one summary request per restoration', async (t) => {
  let release;
  const env = setup(t, (request) => request.url === '/api/v1/me' ? new Promise((resolve) => { release = resolve; })
    : response({unread_count: 3, latest_id: 'current'}));
  const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {onLine: true}});
  let dispose;
  try {
    client.setSession(proof());
    const entry = new TestElement('button'); entry.className = 'notification-entry';
    const count = new TestElement('span'); count.className = 'notification-count'; entry.append(count); env.body.append(entry);
    const {initNotifications} = await import('../src/console/notifications.js');
    dispose = initNotifications(); await flush();
    assert.equal(count.textContent, '3');
    assert.equal(env.requests.filter((request) => request.url.includes('/notifications/summary')).length, 1);
    env.document.hidden = true; env.document.dispatchEvent(new Event('visibilitychange'));
    env.document.hidden = false; env.document.dispatchEvent(new Event('visibilitychange'));
    env.document.dispatchEvent(new Event('visibilitychange')); await flush();
    assert.equal(env.requests.filter((request) => request.url === '/api/v1/me').length, 1);
    assert.equal(env.requests.filter((request) => request.url.includes('/notifications/summary')).length, 1);
    release(me()); await flush();
    assert.equal(env.requests.filter((request) => request.url.includes('/notifications/summary')).length, 2);
    assert.equal(count.textContent, '3');
    assert.equal(client.session.user.id, user.id);
  } finally {
    env.clean(); dispose?.();
    if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor); else delete globalThis.navigator;
  }
});

test('a late logout failure cannot invalidate a newly signed-in session', async (t) => {
  let reject;
  const env = setup(t, () => new Promise((_resolve, fail) => { reject = fail; }));
  try {
    client.setSession(proof());
    const logout = client.signOut();
    client.setSession(proof({csrf_token: 'new-proof'}));
    const signal = client.getSessionSignal();
    reject(new Error('old revoke network failure'));
    await logout;
    assert.equal(client.session.csrf, 'new-proof');
    assert.equal(signal.aborted, false);
  } finally { env.clean(); }
});

test('a previously resolved login response cannot replace a newer session epoch', async (t) => {
  const env = setup(t, () => me(proof({csrf_token: 'old-login'})));
  try {
    const old = await client.api('/session', {method: 'POST', noRedirect: true, body: {username: 'administrator', password: 'password'} });
    client.setSession(proof({csrf_token: 'new-login'}));
    assert.throws(() => client.setSession(old), (error) => error.name === 'AbortError');
    assert.equal(client.session.csrf, 'new-login');
  } finally { env.clean(); }
});

test('an unreadable management401 body still clears the private session immediately', async (t) => {
  const env = setup(t, () => ({status: 401, ok: false, headers: new Headers(), body: null,
    json() { return new Promise(() => {}); }}));
  try {
    client.setSession(proof());
    const request = client.api('/nodes');
    await assert.rejects(request, (error) => error.status === 401);
    assert.equal(client.session.user, null);
  } finally { env.clean(); }
});

test('a server-valid detail with 101 metadata tags reaches the live consumer without fallback', async (t) => {
  const env = setup(t, (request) => request.url === '/api/v1/me' ? me() : assert.fail(`Unexpected fallback ${request.url}`));
  try {
    client.setSession(proof());
    const {connectConsoleLive} = await import('../src/console/live.js');
    const snapshots = [];
    const states = [];
    connectConsoleLive({nodeID: user.id, signal: env.page.signal, onSnapshot: (data) => snapshots.push(data), onState: (state) => states.push(state)});
    await flush();
    const node = {...fleet().data.nodes[0], name: 'Many tags', notes: '', region: '', provider: '', group_id: null,
      tags: Array.from({length: 101}, (_, index) => `tag-${index}`), created_at: '2026-10-27T00:00:00Z'};
    const data = {node, latest: null, runtime: null};
    env.sockets[0].open(); env.sockets[0].message(frame('snapshot', {data}));
    await flush();
    assert.deepEqual(snapshots, [data]);
    assert.equal(states.at(-1).state, 'live');
    assert.equal(env.sockets[0].closed, undefined);
    assert.equal(env.requests.some((request) => request.url.includes('/live/stream')), false);
  } finally { env.clean(); }
});

async function actionPage(t, respond, expires_at = '2026-10-28T00:10:00Z', load) {
  const env = setup(t, respond);
  const originals = new Map(['matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  globalThis.matchMedia = () => ({matches: true, addEventListener() {}});
  globalThis.requestAnimationFrame = () => 0;
  globalThis.history = {state: {}, replaceState(_state, _unused, target) { if (target) location.pathname = target.split('?')[0]; }, pushState(_state, _unused, target) { location.pathname = target; }};
  env.window.scrollTo = () => {};
  for (const id of ['view', 'sidebar', 'account', 'progress', 'toasts']) {
    const element = new TestElement('div'); element.id = id; env.body.append(element);
  }
  client.setSession(proof({expires_at}));
  const routerURL = new URL(`../src/console/router.js?action-${encodeURIComponent(t.name)}`, import.meta.url);
  const {startRouter} = await import(routerURL.href);
  // Load the real action against this fixture's router instance, not a router
  // retained by an earlier fixture's document. No action behavior is replaced.
  const lifecycleURL = new URL('../src/console/pages/node-lifecycle.js', import.meta.url);
  const lifecycleSource = (await readFile(lifecycleURL, 'utf8')).replace(/from '(\.[^']+)'/g,
    (_match, path) => `from '${path === '../router.js' ? routerURL.href : new URL(path, lifecycleURL).href}'`);
  const {lifecycleActions} = await import(`data:text/javascript;base64,${Buffer.from(lifecycleSource).toString('base64')}`);
  const actions = lifecycleActions({id: user.id, name: 'Private node', status: 'online'});
  let loads = 0;
  const stop = startRouter([{match: (path) => path === '/login', bare: true, page: async () => {
    const heading = new TestElement('h1'); heading.append('Sign in'); return heading;
  }}, {match: () => true, page: async () => ++loads === 1 || !load ? actions : load(loads)}]);
  await flush();
  return {...env, get loads() { return loads; }, async disable() {
    actions.querySelectorAll('button').find((control) => control.textContent === 'Disable').click();
    await flush();
    const dialog = env.body.querySelector('dialog');
    dialog.querySelectorAll('button').find((control) => control.textContent === 'Disable node').click();
    dialog.dispatchEvent(new Event('animationend'));
    await flush();
  }, async remove() {
    actions.querySelectorAll('button').find((control) => control.textContent === 'Delete node').click();
    await flush();
    const dialog = env.body.querySelector('dialog');
    dialog.querySelector('input').value = 'Private node';
    dialog.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true}));
    dialog.dispatchEvent(new Event('animationend'));
    await flush();
  }, clean() {
    stop(); env.clean();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }};
}

test('an action cancelled by known expiry cannot recreate an error toast after the private view clears', async (t) => {
  let release;
  const env = await actionPage(t, () => new Promise((resolve) => { release = resolve; }), '2026-10-28T00:00:02Z');
  try {
    await env.disable();
    assert.equal(env.requests[0].url, '/api/v1/nodes/batch');
    await env.advance(2000);
    assert.equal(location.pathname, '/login');
    assert.equal(document.getElementById('view').textContent, 'Sign in');
    release(response({message: 'Late action failure'}, {status: 500})); await flush();
    assert.equal(document.getElementById('toasts').children.length, 0);
  } finally { env.clean(); }
});

test('an action initiating management401 clears private UI without adding a stale sign-in toast', async (t) => {
  let release;
  const env = await actionPage(t, () => new Promise((resolve) => { release = resolve; }));
  try {
    await env.disable();
    release(response({message: 'Unauthorized'}, {status: 401})); await flush();
    assert.equal(location.pathname, '/login');
    assert.equal(client.session.user, null);
    assert.equal(document.getElementById('view').textContent, 'Sign in');
    assert.equal(document.getElementById('toasts').children.length, 0);
  } finally { env.clean(); }
});

test('current action server failures and request timeouts remain visible', async (t) => {
  let timeout = false;
  const env = await actionPage(t, (request) => timeout ? new Promise((_resolve, reject) => {
    request.signal.addEventListener('abort', () => reject(request.signal.reason), {once: true});
  }) : response({message: 'Cannot disable this node.', request_id: 'action-failure'}, {status: 503}));
  try {
    await env.disable();
    assert.equal(document.getElementById('toasts').textContent.includes('Cannot disable this node.'), false, 'raw server wording must not reach the user');
    assert.equal(document.getElementById('toasts').textContent.includes('action-failure'), true);
    timeout = true;
    await env.disable(); await env.advance(15000);
    assert.equal(document.getElementById('toasts').textContent.includes(errorText({code: 'timeout'})), true);
    assert.equal(client.session.user.id, user.id);
    assert.equal(location.pathname, '/overview');
  } finally { env.clean(); }
});

test('a cancelled old action cannot add a toast to a replacement private session', async (t) => {
  let release;
  const env = await actionPage(t, () => new Promise((resolve) => { release = resolve; }));
  try {
    await env.disable();
    client.setSession(proof({csrf_token: 'replacement-action-session'}));
    await flush();
    release(response({message: 'Old session action failed'}, {status: 500})); await flush();
    assert.equal(document.getElementById('toasts').children.length, 0);
    assert.equal(client.session.csrf, 'replacement-action-session');
  } finally { env.clean(); }
});

for (const boundary of ['expiry', 'management401', 'replacement']) {
  test(`a successful lifecycle mutation awaiting refresh cannot report success after ${boundary}`, async (t) => {
    let release;
    const env = await actionPage(t, (request) => request.url === '/api/v1/nodes/batch'
      ? response({results: [{node_id: user.id, status: 'succeeded'}]})
      : response({message: 'Unauthorized'}, {status: 401}),
    boundary === 'expiry' ? '2026-10-28T00:00:02Z' : '2026-10-28T00:10:00Z',
    (count) => count === 2 ? new Promise((resolve) => { release = resolve; }) : (() => {
      const current = new TestElement('h1'); current.append('Replacement account'); return current;
    })());
    try {
      await env.disable();
      assert.equal(env.loads, 2, 'mutation succeeded and its router refresh is still loading');
      assert.equal(document.getElementById('toasts').children.length, 0);
      if (boundary === 'expiry') await env.advance(2000);
      else if (boundary === 'management401') await assert.rejects(client.api('/private-probe'), (error) => error.status === 401);
      else client.setSession(proof({user: {...user, id: '00000000-0000-4000-8000-000000000002', username: 'replacement'}, csrf_token: `replacement:${t.name}`}));
      await flush();
      const expected = boundary === 'replacement' ? 'Replacement account' : 'Sign in';
      assert.equal(document.getElementById('view').textContent, expected);
      const old = new TestElement('h1'); old.append('Private node late refresh');
      release(old); await flush();
      assert.equal(document.getElementById('view').textContent, expected);
      assert.equal(document.getElementById('toasts').children.length, 0, 'cancelled refresh must not continue to the private success toast');
    } finally { env.clean(); }
  });
}

test('a successful delete awaiting go cannot toast its private node name after known expiry', async (t) => {
  let release;
  const env = await actionPage(t, () => new Response(null, {status: 204}), '2026-10-28T00:00:02Z',
    () => new Promise((resolve) => { release = resolve; }));
  try {
    await env.remove();
    assert.equal(env.requests[0].method, 'DELETE');
    assert.equal(location.pathname, '/nodes');
    assert.equal(env.loads, 2, 'delete succeeded and its router navigation is still loading');
    await env.advance(2000);
    assert.equal(document.getElementById('view').textContent, 'Sign in');
    const old = new TestElement('h1'); old.append('Private node deleted');
    release(old); await flush();
    assert.equal(document.getElementById('view').textContent, 'Sign in');
    assert.equal(document.getElementById('toasts').children.length, 0);
  } finally { env.clean(); }
});

test('a current successful lifecycle mutation still reports success after its refresh completes', async (t) => {
  const env = await actionPage(t, () => response({results: [{node_id: user.id, status: 'succeeded'}]}));
  try {
    await env.disable();
    assert.equal(env.loads, 2);
    assert.equal(document.getElementById('toasts').textContent.includes('Private node disabled.'), true);
    assert.equal(client.session.user.id, user.id);
  } finally { env.clean(); }
});

test('login credential rejection still renders its inline error after private-session toast filtering', async (t) => {
  const env = setup(t, () => response({message: 'Unauthorized', code: 'unauthenticated'}, {status: 401}));
  const originals = new Map(['matchMedia', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  globalThis.matchMedia = () => ({matches: true, addEventListener() {}});
  globalThis.requestAnimationFrame = () => 0;
  location.pathname = '/login';
  const sidebar = new TestElement('div'); sidebar.id = 'sidebar';
  const mark = new TestElement('span'); mark.className = 'mark'; mark.cloneNode = () => new TestElement('span');
  sidebar.append(mark); env.body.append(sidebar);
  try {
    const {loginPage} = await import('../src/console/pages/login.js');
    const page = await loginPage(); env.body.append(page);
    const form = page.querySelector('form');
    form.querySelectorAll('input').forEach((control) => { control.value = 'incorrect-credentials'; });
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    const error = form.querySelector('.form-error');
    assert.equal(error.hidden, false);
    assert.ok(error.textContent);
    assert.equal(form.querySelectorAll('input').find((control) => control.name === 'password').value, '');
    assert.equal(location.pathname, '/login');
    assert.equal(client.session.user, null);
  } finally {
    env.clean();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});
