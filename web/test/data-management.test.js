import test from 'node:test';
import assert from 'node:assert/strict';
import {getNavigationSignal, setNavigationSignal, setSession} from '../src/console/api.js';
import {dataManagementPage} from '../src/console/pages/data-management.js';
import {copy, elements, flush, installDOM} from './console-dom.js';
import {liveHarness, response} from './live-harness.js';

const before = '2026-10-08T08:00:00Z';
const snapshot = () => ({before, databases: [
  {domain: 'main', driver: 'sqlite', size_bytes: 524288, reusable_bytes: 8192, compact_supported: true},
  {domain: 'history', driver: 'mysql', size_bytes: 1048576, reusable_bytes: null, compact_supported: false}
], categories: [
  {id: 'metric_history', domain: 'history', rows: 3000, eligible_rows: 2400},
  {id: 'audit_events', domain: 'main', rows: 3500, eligible_rows: 1100}
]});
const find = (root, predicate) => elements(root).find(predicate);
const action = (root, key) => find(root, (element) => element.tagName === 'button' && element.textContent === copy(key));
const checkbox = (root, id) => find(root, (element) => element.type === 'checkbox' && element.value === id);
const compactAction = (root, domain) => find(root, (element) => element.tagName === 'button'
  && element.getAttribute('aria-label') === `${copy('data.management.compact')}: ${copy(`data.management.domain.${domain}`)}`);

function setup(data = snapshot) {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const navigation = getNavigationSignal();
  const controller = new AbortController();
  const requests = [];
  let fail = false;
  let postError = false;
  let malformed = false;
  let resolveGet;
  let delayed = false;
  setNavigationSignal(controller.signal);
  setSession({user: {id: 'admin', username: 'admin', must_change_password: false}, csrf_token: 'csrf', expires_at: '2099-01-01T00:00:00Z'});
  globalThis.fetch = async (url, options = {}) => {
    const method = options.method || 'GET';
    requests.push({url, method, signal: options.signal, body: options.body && JSON.parse(options.body)});
    if (method === 'POST' && postError) throw new TypeError('network down');
    if (method === 'GET' && delayed) await new Promise((resolve) => { resolveGet = resolve; });
    const value = method === 'GET' ? {...data(), ...(malformed ? {databases: {}} : {})} : {before, results: [{id: 'audit_events', deleted_rows: 1100, has_more: true}]};
    return new Response(JSON.stringify(fail && method === 'GET' ? {code: 'internal_error'} : value), {status: fail && method === 'GET' ? 500 : 200, headers: {'Content-Type': 'application/json'}});
  };
  return {dom, requests, controller, fail: () => { fail = true; }, unknown: () => { postError = true; },
    malformed: () => { malformed = true; },
    delay: () => { delayed = true; }, release: () => { delayed = false; resolveGet?.(); },
    restore() { controller.abort(); setNavigationSignal(navigation); setSession({user: null, csrf_token: '', expires_at: ''}); globalThis.fetch = originalFetch; dom.restore(); }};
}

async function mount(state) {
  const page = await dataManagementPage();
  state.dom.body.append(page.element);
  page.attach();
  return page;
}

function selectAudit(page) {
  const audit = checkbox(page.element, 'audit_events');
  audit.checked = true;
  audit.dispatchEvent(new Event('change'));
}
function type(dialog, value) {
  const input = find(dialog, (element) => element.name === 'confirmation');
  input.value = value;
  input.dispatchEvent(new Event('input'));
}

test('fresh selection and exact CLEAN gate bind only selected categories and preview cutoff', async () => {
  const state = setup();
  let page;
  try {
    state.delay();
    page = await mount(state);
    assert.equal(action(page.element, 'data.management.clean').disabled, true);
    state.release();
    await flush();
    assert.equal(action(page.element, 'data.management.clean').disabled, true);
    assert.ok(elements(page.element).filter((element) => element.type === 'checkbox').every((element) => !element.checked));
    selectAudit(page);
    assert.equal(action(page.element, 'data.management.clean').disabled, false);
    action(page.element, 'data.management.clean').click();
    const dialog = state.dom.body.querySelector('dialog');
    assert.ok(dialog.open);
    type(dialog, 'CLEAN ');
    assert.equal(action(dialog, 'data.management.clean').disabled, true);
    dialog.querySelector('form').dispatchEvent(new Event('submit'));
    await flush();
    assert.equal(state.requests.filter((request) => request.method === 'POST').length, 0);
    type(dialog, 'CLEAN');
    assert.equal(action(dialog, 'data.management.clean').disabled, false);
    dialog.querySelector('form').dispatchEvent(new Event('submit'));
    await flush();
    const posts = state.requests.filter((request) => request.method === 'POST');
    assert.equal(posts.length, 1);
    assert.deepEqual(posts[0].body, {categories: ['audit_events'], keep_days: 30, before, confirmation: 'CLEAN'});
  } finally { page?.detach(); state.restore(); }
});

test('failed refresh retains usage but disables destructive actions', async () => {
  const state = setup();
  let page;
  try {
    page = await mount(state);
    await flush();
    selectAudit(page);
    state.fail();
    action(page.element, 'data.management.refresh').click();
    await flush();
    assert.equal(action(page.element, 'data.management.clean').disabled, true);
    assert.ok(page.element.querySelector('.data-stale'));
    assert.ok(checkbox(page.element, 'audit_events').checked);
    assert.ok(elements(page.element).filter((element) => element.textContent === copy('data.management.compact') && element.tagName === 'button').every((element) => element.disabled));
  } finally { page?.detach(); state.restore(); }
});

test('malformed successful refresh retains valid statistics without rearming destructive controls', async () => {
  const state = setup();
  let page;
  try {
    page = await mount(state);
    await flush();
    selectAudit(page);
    assert.equal(action(page.element, 'data.management.clean').disabled, false);
    const usage = page.element.querySelector('.grid-2').textContent;
    state.malformed();
    action(page.element, 'data.management.refresh').click();
    await flush();
    assert.equal(action(page.element, 'data.management.clean').disabled, true);
    assert.ok(page.element.querySelector('.data-stale'));
    assert.equal(page.element.querySelector('.grid-2').textContent, usage);
    assert.ok(checkbox(page.element, 'audit_events').checked);
    const compact = action(page.element, 'data.management.compact');
    assert.equal(compact.disabled, true);
    assert.equal(compact.getAttribute('aria-label'), `${copy('data.management.compact')}: ${copy('data.management.domain.main')}`);
    action(page.element, 'data.management.clean').click();
    compact.click();
    assert.ok(!state.dom.body.querySelector('dialog'));
    assert.equal(state.requests.filter((request) => request.method === 'POST').length, 0);
  } finally { page?.detach(); state.restore(); }
});

test('unknown POST permanently disables its original submit and detach aborts requests and closes dialogs', async () => {
  const state = setup();
  let page;
  try {
    page = await mount(state);
    await flush();
    selectAudit(page);
    action(page.element, 'data.management.clean').click();
    const dialog = state.dom.body.querySelector('dialog');
    type(dialog, 'CLEAN');
    state.unknown();
    dialog.querySelector('form').dispatchEvent(new Event('submit'));
    await flush();
    type(dialog, 'CLEAN');
    assert.equal(action(dialog, 'data.management.clean').disabled, true);
    dialog.querySelector('form').dispatchEvent(new Event('submit'));
    await flush();
    assert.equal(state.requests.filter((request) => request.method === 'POST').length, 1);
    page.detach();
    await new Promise((resolve) => setTimeout(resolve, 280));
    assert.ok(!state.dom.body.querySelector('dialog'));
    state.delay();
    page.attach();
    const pending = state.requests.at(-1);
    page.detach();
    assert.equal(pending.signal.aborted, true);
    state.release();
    await flush();
  } finally { page?.detach(); state.restore(); }
});

test('period changes disable cleanup until fresh counts and SQLite compaction requires exact COMPACT', async () => {
  const state = setup();
  let page;
  try {
    page = await mount(state);
    await flush();
    selectAudit(page);
    state.delay();
    const days = find(page.element, (element) => element.name === 'keep_days');
    days.value = '90';
    days.dispatchEvent(new Event('change'));
    assert.equal(action(page.element, 'data.management.clean').disabled, true);
    state.release();
    await flush();
    assert.equal(action(page.element, 'data.management.clean').disabled, false);
    assert.equal(compactAction(page.element, 'history'), undefined);
    const compact = compactAction(page.element, 'main');
    assert.equal(compact.disabled, false);
    compact.click();
    const dialog = state.dom.body.querySelector('dialog');
    type(dialog, 'compact');
    assert.equal(action(dialog, 'data.management.compact').disabled, true);
    dialog.querySelector('form').dispatchEvent(new Event('submit'));
    await flush();
    assert.equal(state.requests.filter((request) => request.method === 'POST').length, 0);
    type(dialog, 'COMPACT ');
    assert.equal(action(dialog, 'data.management.compact').disabled, true);
    type(dialog, 'COMPACT');
    assert.equal(action(dialog, 'data.management.compact').disabled, false);
    dialog.querySelector('form').dispatchEvent(new Event('submit'));
    await flush();
    assert.ok(dialog.textContent.includes(copy('data.management.compacted')));
    assert.ok(page.element.textContent.includes(copy('data.management.compacted')));
    assert.equal(action(dialog, 'data.management.compact'), undefined);
    action(dialog, 'controls.dialog.close').click();
    await new Promise((resolve) => setTimeout(resolve, 280));
    assert.equal(compactAction(page.element, 'main').disabled, false);
  } finally {
    page?.detach();
    await new Promise((resolve) => setTimeout(resolve, 280));
    state.restore();
  }
});

test('MySQL capability enables compaction and an unknown result cannot be retried', async () => {
  const state = setup(() => {
    const data = snapshot();
    data.databases[1].compact_supported = true;
    return data;
  });
  let page;
  try {
    page = await mount(state);
    await flush();
    const compact = compactAction(page.element, 'history');
    assert.equal(compact.disabled, false);
    compact.click();
    const dialog = state.dom.body.querySelector('dialog');
    type(dialog, 'compact');
    assert.equal(action(dialog, 'data.management.compact').disabled, true);
    type(dialog, 'COMPACT ');
    assert.equal(action(dialog, 'data.management.compact').disabled, true);
    type(dialog, 'COMPACT');
    assert.equal(action(dialog, 'data.management.compact').disabled, false);
    state.unknown();
    dialog.querySelector('form').dispatchEvent(new Event('submit'));
    await flush();
    assert.ok(dialog.textContent.includes(copy('data.management.unknown')));
    assert.ok(page.element.querySelector('.data-stale'));
    type(dialog, 'COMPACT');
    assert.equal(action(dialog, 'data.management.compact').disabled, true);
    dialog.querySelector('form').dispatchEvent(new Event('submit'));
    await flush();
    assert.equal(state.requests.filter((request) => request.method === 'POST').length, 1);
    action(dialog, 'controls.dialog.close').click();
    await new Promise((resolve) => setTimeout(resolve, 280));
    assert.equal(compactAction(page.element, 'history').disabled, true);
    assert.equal(compactAction(page.element, 'main').disabled, true);
    assert.equal(action(page.element, 'data.management.clean').disabled, true);
    compactAction(page.element, 'history').click();
    assert.ok(!state.dom.body.querySelector('dialog'));
  } finally { page?.detach(); state.restore(); }
});

for (const kind of ['cleanup', 'compact']) {
  test(`${kind} waits for its deadline before showing an unknown result and forbidding retries`, async (t) => {
    const previousNavigation = getNavigationSignal();
    const navigation = new AbortController();
    const env = liveHarness(t, (request) => {
      if (request.method === 'POST') return new Promise((_resolve, reject) => {
        request.signal.addEventListener('abort', () => reject(request.signal.reason), {once: true});
      });
      const data = snapshot();
      data.databases[1].compact_supported = true;
      return response(data);
    });
    let page;
    setNavigationSignal(navigation.signal);
    setSession({user: {id: 'admin', username: 'admin', must_change_password: false}, csrf_token: 'csrf', expires_at: '2099-01-01T00:00:00Z'});
    try {
      page = await mount({dom: env});
      await flush();
      if (kind === 'cleanup') {
        selectAudit(page);
        action(page.element, 'data.management.clean').click();
      } else compactAction(page.element, 'history').click();
      const dialog = env.body.querySelector('dialog');
      const confirmation = kind === 'cleanup' ? 'CLEAN' : 'COMPACT';
      const key = kind === 'cleanup' ? 'data.management.clean' : 'data.management.compact';
      type(dialog, confirmation);
      dialog.querySelector('form').dispatchEvent(new Event('submit'));
      await env.advance(14999);
      assert.equal(dialog.querySelector('.form-error').hidden, true);
      assert.equal(action(dialog, key).disabled, true);
      await env.advance(1);
      if (kind === 'compact') {
        assert.equal(dialog.querySelector('.form-error').hidden, true);
        assert.equal(env.requests.at(-1).signal.aborted, false);
        await env.advance(31 * 60 * 1000 - 15001);
        assert.equal(dialog.querySelector('.form-error').hidden, true);
        await env.advance(1);
      }
      assert.equal(dialog.querySelector('.form-error').hidden, false);
      assert.ok(page.element.querySelector('.data-stale'));
      type(dialog, confirmation);
      assert.equal(action(dialog, key).disabled, true);
      dialog.querySelector('form').dispatchEvent(new Event('submit'));
      await flush();
      assert.equal(env.requests.filter((request) => request.method === 'POST').length, 1);
    } finally {
      page?.detach();
      await env.advance(280);
      navigation.abort();
      setNavigationSignal(previousNavigation);
      setSession({user: null, csrf_token: '', expires_at: ''});
    }
  });
}

test('embedded storage cleanup retains its controls without a second page heading', async () => {
  const state = setup();
  let page;
  try {
    page = dataManagementPage({embedded: true});
    state.dom.body.append(page.element); page.attach(); await flush();
    assert.equal(page.element.querySelector('h1'), null);
    assert.ok(action(page.element, 'data.management.refresh'));
    assert.ok(compactAction(page.element, 'main'));
    assert.ok(action(page.element, 'data.management.clean'));
  } finally { page?.detach(); state.restore(); }
});
