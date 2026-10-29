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
  // Keep a failed implementation's ignored browser promise from leaking into
  // another test; navigation's own result must still expose unexpected errors.
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
