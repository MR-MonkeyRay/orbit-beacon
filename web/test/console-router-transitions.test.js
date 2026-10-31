import test from 'node:test';
import assert from 'node:assert/strict';
import {setImmediate as nextTurn} from 'node:timers/promises';
import * as client from '../src/console/api.js';
import {reducedMotion} from '../src/shared/dom.js';
import {flush, TestElement} from './console-dom.js';
import {liveHarness, response} from './live-harness.js';

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((accept, fail) => { resolve = accept; reject = fail; });
  // 防止失败实现中被忽略的浏览器 promise 泄漏到
  // 其他测试；导航自身的结果仍须暴露意外错误。
  promise.catch(() => {});
  return {promise, resolve, reject};
}

async function routing(t, {hidden = false} = {}) {
	const env = liveHarness(t, () => response({user: {id: 'routing-user', username: 'administrator', must_change_password: false},
	  csrf_token: 'routing-proof', expires_at: '2026-10-28T00:10:00Z'}, {headers: {Date: 'Wed, 28 Oct 2026 00:00:00 GMT'}}));
  const originals = new Map(['matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  const motion = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => false});
  globalThis.matchMedia = () => ({matches: false, addEventListener() {}});
  globalThis.requestAnimationFrame = () => 0;
  location.origin = 'https://example.test';
  location.pathname = '/initial'; location.search = ''; location.hash = '';
  const setURL = (target) => { if (target) { const url = new URL(target, location.href); location.pathname = url.pathname; location.search = url.search; location.hash = url.hash; } };
  globalThis.history = {state: {}, replaceState(_state, _unused, target) { setURL(target); }, pushState(_state, _unused, target) { setURL(target); }};
  env.window.scrollTo = () => {};
  env.document.hidden = hidden;
  for (const id of ['view', 'sidebar', 'account', 'progress', 'toasts']) {
    const element = new TestElement('div'); element.id = id; env.body.append(element);
  }
  const transitions = [];
  env.document.startViewTransition = (update) => {
    if (env.document.hidden) throw new DOMException('Transition was aborted because of invalid state.', 'InvalidStateError');
    const ready = deferred();
    const done = deferred();
    const entry = {ready: ready.promise, updateCallbackDone: done.promise,
      skipTransition() { ready.reject(new DOMException('Transition skipped.', 'AbortError')); },
      begin() { ready.resolve(); }, fail(error) { ready.reject(error); }};
    Promise.resolve().then(update).then(done.resolve, (error) => { done.reject(error); ready.reject(error); });
    transitions.push(entry);
    return entry;
  };
  client.setNavigationSignal(null);
  client.setSession({user: {id: 'routing-user', username: 'administrator', must_change_password: false}, csrf_token: 'routing-proof', expires_at: '2026-10-28T00:10:00Z'});
  const router = await import(`../src/console/router.js?transition-${encodeURIComponent(t.name)}`);
  const stop = router.startRouter([{match: () => true, page: async () => {
    const heading = new TestElement('h1'); heading.append(location.pathname);
    const content = new TestElement('section'); content.append(heading); return content;
  }}]);
  await flush();
  transitions[0]?.begin();
  await flush();
  return {...env, ...router, transitions, clean() {
    stop();
    client.setSession({user: null, csrf_token: '', expires_at: ''});
    client.setNavigationSignal(null);
    Object.defineProperty(reducedMotion, 'matches', motion);
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }};
}

for (const target of ['/?from=login#status', '/public.html']) {
  for (const replace of [false, true]) {
    test(`root navigation ${target} uses full-page ${replace ? 'replace' : 'assign'} instead of the console fallback`, async (t) => {
      const env = await routing(t, {hidden: true});
      const navigated = [];
      location.assign = (url) => navigated.push(['assign', url]);
      location.replace = (url) => navigated.push(['replace', url]);
      try {
        await env.go(target, {replace});
        assert.deepEqual(navigated, [[replace ? 'replace' : 'assign', target]]);
        assert.equal(document.getElementById('view').textContent, '/initial');
      } finally { env.clean(); }
    });
  }
}

test('hidden initial routing and hidden navigation update content without starting ineligible transitions', async (t) => {
  const env = await routing(t, {hidden: true});
  try {
    assert.equal(document.getElementById('view').textContent, '/initial');
    await env.go('/hidden-next');
    assert.equal(document.getElementById('view').textContent, '/hidden-next');
  } finally { env.clean(); }
});

test('rapid routing rejects the superseded navigation without masking the newest content', async (t) => {
  const env = await routing(t);
  try {
    const first = env.go('/first'); await flush();
    const cancelled = assert.rejects(first, (error) => error.name === 'AbortError');
    const firstTransition = env.transitions.at(-1);
    const second = env.go('/second'); await flush();
    env.transitions.at(-1).begin();
    await Promise.all([cancelled, second]);
    firstTransition.begin(); await flush();
    assert.equal(location.pathname, '/second');
    assert.equal(document.getElementById('view').textContent, '/second');
  } finally { env.clean(); }
});

test('hiding during an unfinished transition accepts the browser visibility cancellation and still commits content', async (t) => {
  const env = await routing(t);
  try {
    const pending = env.go('/before-hide'); await flush();
    env.document.hidden = true;
    env.transitions.at(-1).fail(new DOMException('Transition was aborted because of invalid state.', 'InvalidStateError'));
    env.document.dispatchEvent(new Event('visibilitychange'));
    await pending;
    assert.equal(document.getElementById('view').textContent, '/before-hide');
    await env.go('/while-hidden');
    assert.equal(document.getElementById('view').textContent, '/while-hidden');
  } finally { env.clean(); }
});

for (const name of ['InvalidStateError', 'AbortError']) {
  test(`visible routing exposes an unexpected ${name} instead of treating it as intentional cancellation`, async (t) => {
    const env = await routing(t);
    try {
      const error = new DOMException('Unexpected animation configuration failure.', name);
      const pending = env.go('/broken-animation');
      const rejected = assert.rejects(pending, (value) => value === error);
      await flush(); env.transitions.at(-1).fail(error);
      await rejected;
      assert.equal(document.getElementById('view').textContent, '/broken-animation');
    } finally { env.clean(); }
  });
}

test('routing exposes a genuine transition DOM-update failure', async (t) => {
  const env = await routing(t);
  const view = document.getElementById('view');
  const replace = view.replaceChildren;
  try {
    const error = new Error('DOM update failed.');
    view.replaceChildren = () => { throw error; };
    await assert.rejects(env.go('/broken-content'), (value) => value === error);
  } finally { view.replaceChildren = replace; env.clean(); }
});

test('rapid link and popstate navigation consume only their expected cancellation', async (t) => {
  const env = await routing(t);
  try {
    const follow = (path) => {
      const anchor = {target: '', href: new URL(path, location.href).href};
      const event = new Event('click', {cancelable: true});
      Object.defineProperties(event, {button: {value: 0}, target: {value: {closest: () => anchor}}});
      env.document.dispatchEvent(event);
    };
    follow('/first-link'); await flush();
    follow('/second-link'); await flush();
    env.transitions.at(-1).begin(); await flush();
    location.pathname = '/first-popstate';
    env.window.dispatchEvent(Object.assign(new Event('popstate'), {state: {scroll: 0}})); await flush();
    location.pathname = '/second-popstate';
    env.window.dispatchEvent(Object.assign(new Event('popstate'), {state: {scroll: 0}})); await flush();
    env.transitions.at(-1).begin(); await flush();
    await nextTurn();
    assert.equal(document.getElementById('view').textContent, '/second-popstate');
  } finally { env.clean(); }
});

test('root navigation honors dirty-form cancellation before any full-page request', async (t) => {
  const env = await routing(t, {hidden: true});
  const navigated = [];
  location.assign = (target) => navigated.push(target);
  const form = new TestElement('form'); form.dataset.dirty = 'true';
  document.getElementById('view').append(form);
  try {
    const cancelled = env.go('/');
    const rejection = assert.rejects(cancelled, (error) => error.name === 'AbortError');
    await flush();
    assert.deepEqual(navigated, []);
    document.querySelector('dialog').close('cancel'); await rejection;
    assert.equal(form.isConnected, true);
    const accepted = env.go('/public.html'); await flush();
    document.querySelector('dialog').close('confirm'); await accepted;
    assert.deepEqual(navigated, ['/public.html']);
    const approvedUnload = new Event('beforeunload', {cancelable: true});
    env.window.dispatchEvent(approvedUnload);
    assert.equal(approvedUnload.defaultPrevented, false, 'confirmed navigation must not prompt twice');
    const unapprovedUnload = new Event('beforeunload', {cancelable: true});
    env.window.dispatchEvent(unapprovedUnload);
    assert.equal(unapprovedUnload.defaultPrevented, true, 'the unload permission is used once');
  } finally { env.clean(); }
});

test('locale reload confirms dirty content once, rejects busy forms, and consumes unload approval once', async (t) => {
  const env = await routing(t, {hidden: true});
  const form = new TestElement('form'); form.dataset.dirty = 'true';
  document.getElementById('view').append(form);
  const busy = new TestElement('button'); busy.classList.add('busy'); form.append(busy);
  let reloads = 0;
  location.reload = () => { reloads += 1; };
  try {
    assert.equal(await env.confirmLocaleChange(), false);
    assert.equal(document.querySelector('dialog'), null);
    busy.classList.remove('busy');
    const cancelled = env.confirmLocaleChange(); await flush();
    document.querySelector('dialog').close('cancel');
    assert.equal(await cancelled, false);
    assert.equal(form.isConnected, true);
    const accepted = env.confirmLocaleChange(); await flush();
    document.querySelector('dialog').close('confirm');
    assert.equal(await accepted, true);
    env.reloadAfterConfirmation();
    assert.equal(reloads, 1);
    const approved = new Event('beforeunload', {cancelable: true}); env.window.dispatchEvent(approved);
    assert.equal(approved.defaultPrevented, false);
    const next = new Event('beforeunload', {cancelable: true}); env.window.dispatchEvent(next);
    assert.equal(next.defaultPrevented, true);
  } finally { env.clean(); }
});

test('locale confirmation includes dialog drafts and uses one prompt for a dirty view plus dialog', async (t) => {
  const env = await routing(t, {hidden: true});
  const {modal} = await import('../src/console/ui/dialogs.js');
  const input = new TestElement('input');
  const dialog = modal('Private form', () => input);
  input.value = 'unsaved secret';
  const form = new TestElement('form'); form.dataset.dirty = 'true';
  document.getElementById('view').append(form);
  try {
    const pending = env.confirmLocaleChange(); await flush();
    const dialogs = env.body.querySelectorAll('dialog');
    assert.equal(dialogs.length, 2, 'the existing form gets only one discard confirmation');
    dialogs.at(-1).close('cancel');
    assert.equal(await pending, false);
    assert.equal(input.value, 'unsaved secret');
    form.dataset.dirty = 'false';
    const dialogOnly = env.confirmLocaleChange(); await flush();
    env.body.querySelectorAll('dialog').at(-1).close('confirm');
    assert.equal(await dialogOnly, true);
    dialog.close('done');
  } finally { env.clean(); }
});
