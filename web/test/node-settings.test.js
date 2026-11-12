import test from 'node:test';
import assert from 'node:assert/strict';
import {invalidate, setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, elements, flush, installDOM, TestElement} from './console-dom.js';

const id = '00000000-0000-4000-8000-000000000001';
const path = `/api/v1/nodes/${id}`;
const toastHost = new TestElement('div');
toastHost.id = 'toasts';
const control = (root, name) => elements(root).find((element) => element.name === name);
const formFor = (root, name) => root.querySelectorAll('form').find((form) => control(form, name));
const factValues = (root) => Object.fromEntries(root.querySelectorAll('dt').map((label) => [label.textContent, label.parentElement.querySelector('dd').textContent]));
const edit = (form, name, value) => {
  const input = control(form, name);
  if (input.type === 'checkbox') input.checked = value; else input.value = value;
  input.dispatchEvent(new Event('input'));
  // 小型控制台测试工具不模拟 DOM 事件传播。
  form.dispatchEvent(new Event('input'));
  return input;
};

async function setup({hash = '#details', presence = 'online', lastSeen = '2026-10-28T10:00:00Z', patchStatus = 200, latest = null, expiresOn = null} = {}) {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window', 'WebSocket', 'matchMedia', 'requestAnimationFrame', 'history', 'performance', 'setTimeout', 'clearTimeout'].map((key) => [key, globalThis[key]]));
  const originalNow = Date.now;
  globalThis.window = new EventTarget();
  globalThis.matchMedia = () => ({matches: false, addEventListener() {}});
  globalThis.requestAnimationFrame = () => 0;
  let now = 0;
  const clock = Date.parse('2026-10-28T10:00:00Z');
  Date.now = () => clock + now;
  const sockets = [];
  globalThis.WebSocket = class extends EventTarget {
    constructor() {
      super(); sockets.push(this);
      queueMicrotask(() => {
        if (this.closed) return;
        this.dispatchEvent(new Event('open'));
        this.message({type: 'snapshot', server_time: new Date(clock + now).toISOString(), update_interval_seconds: 10,
          data: {node, latest: latest ? {...latest, system: latest.system || null} : null}});
      });
    }
    message(value) { this.dispatchEvent(Object.assign(new Event('message'), {data: JSON.stringify(value)})); }
    close() { this.closed = true; }
  };
  let timerID = 0;
  const deadlines = new Map();
  globalThis.performance = {now: () => now};
  globalThis.setTimeout = (callback, delay) => { deadlines.set(++timerID, {callback, at: now + delay, delay}); return timerID; };
  globalThis.clearTimeout = (key) => deadlines.delete(key);
  location.pathname = `/nodes/${id}`;
  location.hash = hash;
  location.href = `https://example.test/nodes/${id}`;
  globalThis.history = {state: null, replaceState(_state, _unused, url) { location.hash = url.includes('#') ? `#${url.split('#')[1]}` : ''; }};
  toastHost.replaceChildren();
  dom.body.append(toastHost);
  const page = new AbortController();
  setNavigationSignal(page.signal);
  invalidate('groups', 'nodes');
  const userSession = {csrf_token: 'csrf', user: {id, username: 'administrator', must_change_password: false}, expires_at: '2026-10-29T10:00:00Z'};
  setSession(userSession);
  const requests = [];
  const node = {id, name: 'Internal name', status: 'unknown', stopped: false, presence, agent_version: 'test-agent',
    expires_on: expiresOn, last_seen: lastSeen, group_id: null, region: '', provider: '', notes: 'Saved notes', tags: [], created_at: '2026-10-27T10:00:00Z'};
  const traffic = {billing_day: 1, quota_bytes: '0', cycle: {}, daily: [], updated_at: null};
  globalThis.fetch = async (url, options) => {
    requests.push({url, options});
    let data;
    let status = 200;
    if (url === '/api/v1/me') data = userSession;
    else if (url === path) data = options.method === 'PATCH' ? Object.assign(node, JSON.parse(options.body)) : node;
    else if (url === `${path}/commands`) data = {commands: []};
    else if (url === `${path}/traffic-settings`) {
      status = options.method === 'PATCH' ? patchStatus : 200;
      data = status === 200 ? options.method === 'PATCH' ? {billing_day: 2, quota_bytes: '1000000000', billing_mode: 'total'} : {billing_day: 1, quota_bytes: '0'}
        : {message: 'Billing update rejected'};
    } else if (url === '/api/v1/groups') data = {groups: []};
    else if (url === `${path}/public-settings`) data = options.method === 'PATCH' ? JSON.parse(options.body) : {enabled: true, fields: ['cpu', 'memory', 'load', 'uptime', 'availability']};
    else if (url === `${path}/latest`) { data = latest || {message: 'No sample'}; status = latest ? 200 : 404; }
    else if (url === `${path}/traffic?days=30`) data = traffic;
    else if (url === `${path}/probes`) data = {probes: []};
    else if (url.startsWith(`${path}/history?`)) { data = {message: 'No history'}; status = 503; }
    else { data = {message: `Unexpected request ${url}`}; status = 404; }
    return new Response(JSON.stringify(data), {status});
  };
  try {
    const {nodePage} = await import('../src/console/pages/node.js');
    const root = await nodePage();
    dom.body.append(root);
    dom.mutations();
    await flush();
    return {...dom, root, page, requests, node, deadlines, send(data) { sockets.at(-1).message({type: 'snapshot', server_time: new Date(clock + now).toISOString(), update_interval_seconds: 10, data}); }, advance(milliseconds) {
      now += milliseconds;
      for (const socket of sockets) if (!socket.closed) socket.message({type: 'heartbeat', server_time: new Date(clock + now).toISOString(), update_interval_seconds: 10});
      for (const [key, timer] of [...deadlines]) if (timer.at <= now) { deadlines.delete(key); timer.callback(); }
    }, cleanup() {
      page.abort();
      setNavigationSignal(null);
      setSession({user: null, csrf_token: '', expires_at: ''});
      Date.now = originalNow;
      for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
      dom.restore();
    }};
  } catch (error) {
    page.abort(); setNavigationSignal(null);
    setSession({user: null, csrf_token: '', expires_at: ''});
    Date.now = originalNow;
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    dom.restore();
    throw error;
  }
}

test('node Agent section keeps commands without fetching enrollment keys or runtime', async () => {
  const env = await setup({hash: '#agent'});
  try {
    const tabs = elements(env.root).filter((element) => element.getAttribute('role') === 'tab');
    assert.equal(tabs.length, 4);
    assert.deepEqual(tabs.map((tab) => tab.id), ['tab-monitor', 'tab-traffic', 'tab-probes', 'tab-settings']);
    assert.deepEqual(tabs.map((tab) => tab.querySelector('use').getAttribute('href')), ['#i-monitor', '#i-cloud', '#i-check', '#i-sliders']);
    assert.equal(env.root.querySelector('#tab-settings').getAttribute('aria-selected'), 'true');
    assert.equal(env.root.querySelector('#tab-config'), null);
    assert.equal(env.requests.some((request) => request.url?.endsWith('/runtime') || request === `${path}/runtime`), false);
    assert.equal(env.root.querySelector('#node-agent').parentElement.querySelectorAll('dl').length, 0);
    assert.equal(env.root.querySelector('#node-enrollment'), null);
    assert.equal(env.requests.some((request) => request.url.startsWith('/api/v1/enrollments')), false);
    assert.equal(env.root.querySelector('.node-summary').querySelectorAll('time').at(-1).getAttribute('datetime'), env.node.last_seen);
    assert.ok(formFor(env.root, 'notes'));
    assert.ok(formFor(env.root, 'cpu'));
  } finally { env.cleanup(); }
});

test('removed #config hash does not alias the local agent settings tab', async () => {
  const env = await setup({hash: '#config'});
  try {
    assert.equal(env.root.querySelector('#tab-monitor').getAttribute('aria-selected'), 'true');
    assert.equal(env.root.querySelector('#tab-config'), null);
  } finally { env.cleanup(); }
});

test('billing saves exact decimal units, stays clean after submit, and preserves focus and traffic tab', async () => {
  const env = await setup({hash: '#traffic'});
  try {
    const billing = formFor(env.root, 'billing_day');
    assert.ok(billing, 'Traffic has a separate billing form');
    // 流量面板标题与标签页同名已省略；计费面板保留自己的标题。
    const headings = env.root.querySelectorAll('h2').map((heading) => heading.textContent);
    assert.ok(headings.includes(copy('nodes.trafficBilling')));
    assert.ok(!headings.includes(copy('nodes.traffic')));
    assert.deepEqual(control(billing, 'quota_unit').options.map((option) => option.value), ['bytes', 'KB', 'MB', 'GB', 'TB']);
    assert.equal(control(billing, 'billing_day').getAttribute('min'), '1');
    assert.equal(control(billing, 'billing_day').getAttribute('max'), '28');
    edit(billing, 'billing_day', '2');
    edit(billing, 'quota_value', '01');
    const unit = edit(billing, 'quota_unit', 'GB');
    unit.dispatchEvent(new Event('change'));
    const quota = control(billing, 'quota_value'); quota.focus();
    const trafficBefore = env.requests.filter((request) => request.url === `${path}/traffic?days=30`).length;
    const requestsBefore = env.requests.length;
    billing.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    const patch = env.requests.find((request) => request.url === `${path}/traffic-settings` && request.options.method === 'PATCH');
    assert.deepEqual(JSON.parse(patch.options.body), {billing_day: 2, quota_bytes: '1000000000', billing_mode: 'total'});
    assert.equal(billing.dataset.dirty, 'false');
    assert.equal(billing.querySelector('#billing-save').disabled, true);
    assert.equal(env.document.activeElement, quota);
    assert.equal(env.root.querySelector('#tab-traffic').getAttribute('aria-selected'), 'true');
    assert.equal(location.hash, '#traffic');
    assert.equal(env.requests.filter((request) => request.url === `${path}/traffic?days=30`).length, trafficBefore + 1);
    assert.deepEqual(env.requests.slice(requestsBefore).map((request) => request.url), [`${path}/traffic-settings`, `${path}/traffic?days=30`]);
    edit(billing, 'quota_value', '2000000000');
    assert.equal(billing.dataset.dirty, 'true');
    billing.querySelectorAll('button').find((button) => button.textContent === copy('controls.discard')).click();
    assert.equal(billing.dataset.dirty, 'false');
  } finally { env.cleanup(); }
});

test('rejected billing saves preserve the draft and do not refresh traffic', async () => {
  const env = await setup({hash: '#traffic', patchStatus: 400});
  try {
    const billing = formFor(env.root, 'billing_day');
    assert.ok(billing, 'Traffic has a separate billing form');
    edit(billing, 'billing_day', '2');
    const before = env.requests.filter((request) => request.url === `${path}/traffic?days=30`).length;
    billing.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.equal(billing.dataset.dirty, 'true');
    assert.equal(control(billing, 'billing_day').value, '2');
    assert.equal(billing.querySelector('#billing-save').disabled, false);
    assert.equal(env.requests.filter((request) => request.url === `${path}/traffic?days=30`).length, before);
    assert.ok(toastHost.querySelector('.error'));
  } finally { env.cleanup(); }
});

test('billing revisits use the saved response and honor tab-change discard', async () => {
  const env = await setup({hash: '#traffic'});
  try {
    let billing = formFor(env.root, 'billing_day');
    edit(billing, 'billing_day', '2');
    billing.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    edit(billing, 'quota_value', '2000000000');
    env.root.querySelector('#tab-settings').click();
    await flush();
    const dialog = env.body.querySelector('dialog');
    assert.ok(dialog, 'leaving a dirty billing form asks before discarding');
    dialog.querySelectorAll('button').find((button) => button.textContent === copy('controls.discard')).click();
    dialog.dispatchEvent(new Event('animationend'));
    await flush();
    assert.equal(env.root.querySelector('#tab-settings').getAttribute('aria-selected'), 'true');
    env.root.querySelector('#tab-traffic').click();
    await flush();
    billing = formFor(env.root, 'billing_day');
    assert.equal(control(billing, 'billing_day').value, '2');
    assert.equal(billing.dataset.dirty, 'false');
    assert.equal(billing.querySelector('#billing-save').disabled, true);
  } finally { env.cleanup(); }
});

test('latest deadline patches preserve numeric nodes on monitor and session boundaries clear samples', async () => {
  const received = '2026-10-28T10:00:00Z';
  const latest = {received_at: received, sampled_at: received, server_time: received, seq: 1,
    metrics: {cpu_percent: 25, uptime_seconds: 3600, metric_status: {cpu_percent: 'ok', uptime_seconds: 'ok'},
      sources: {cpu: {age_ms: 0, source_interval_ms: 10000}, uptime: {age_ms: 0, source_interval_ms: 10000}}}};
  const env = await setup({hash: '', latest});
  try {
    assert.equal(env.root.querySelector('#tab-monitor').getAttribute('aria-selected'), 'true');
    assert.equal(formFor(env.root, 'notes'), undefined);
    const cpu = env.root.querySelectorAll('dt').find((label) => label.textContent === copy('nodes.cpu')).parentElement.querySelector('.sample-value');
    const before = env.requests.length;
    env.advance(45000);
    assert.equal(cpu.classList.contains('metric-stale'), false);
    env.advance(1);
    assert.equal(cpu.classList.contains('metric-stale'), true);
    assert.equal(cpu.textContent, '25%');
    assert.equal(env.requests.length, before);
    assert.equal(env.root.querySelector('.sample-value'), cpu);
    setSession({csrf_token: '', user: null, expires_at: ''});
    assert.equal(env.root.querySelectorAll('.sample-value').length, 0);
    assert.equal(env.deadlines.size, 0);
  } finally { env.cleanup(); }
});

test('expiry date appears before notes and clears with explicit null', async () => {
  const env = await setup({expiresOn: '2028-02-29'});
  try {
    const details = formFor(env.root, 'notes');
    const expiry = control(details, 'expires_on');
    assert.equal(expiry.type, 'date');
    assert.equal(expiry.value, '2028-02-29');
    assert.equal(factValues(env.root)[copy('nodes.expiresOn')], undefined);
    const inputs = elements(details).filter((element) => element.name);
    assert.ok(inputs.indexOf(expiry) < inputs.indexOf(control(details, 'notes')));
    edit(details, 'expires_on', '');
    details.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    const patch = env.requests.find((request) => request.url === path && request.options.method === 'PATCH');
    assert.equal(JSON.parse(patch.options.body).expires_on, null);
    assert.equal(env.node.expires_on, null);
  } finally { env.cleanup(); }
});

test('unset expiry uses the existing not-set label', async () => {
  const env = await setup();
  try {
    assert.equal(control(formFor(env.root, 'notes'), 'expires_on').value, '');
    assert.equal(factValues(env.root)[copy('nodes.expiresOn')], undefined);
  } finally { env.cleanup(); }
});

for (const [hash, section] of Object.entries({details: 'details', public: 'public', agent: 'agent',
  'settings-details': 'details', 'settings-public': 'public', 'settings-agent': 'agent'})) {
  test(`#${hash} opens settings and targets ${section}`, async () => {
    const env = await setup({hash: `#${hash}`});
    try {
      assert.equal(env.root.querySelector('#tab-settings').getAttribute('aria-selected'), 'true');
      const target = env.root.querySelector('[data-autofocus]');
      assert.equal(target.id, `node-${section}`);
      await env.root.lastElementChild.selectTab(hash);
      assert.equal(document.activeElement, target);
      assert.equal(target.scrolledIntoView.block, 'start');
      assert.equal(location.hash, `#${hash}`);
    } finally { env.cleanup(); }
  });
}

test('header summary updates on live metadata and detail saves without losing the public draft', async () => {
  const env = await setup({hash: '#settings'});
  try {
    env.send({node: {...env.node, region: 'Tokyo', provider: 'Provider', tags: ['edge'], last_seen: '2026-10-28T10:01:00Z'}, latest: null});
    const summary = env.root.querySelector('.node-summary');
    assert.match(summary.textContent, /Tokyo·Provider·edge/);
    assert.equal(summary.querySelectorAll('.tag').length, 1);
    assert.deepEqual(summary.querySelectorAll('time').map((time) => time.getAttribute('datetime')), [env.node.created_at, '2026-10-28T10:01:00Z']);
    assert.equal(factValues(env.root)[copy('nodes.region')], undefined);
    assert.equal(factValues(env.root)[copy('nodes.lastSeen')], undefined);
    const publicForm = formFor(env.root, 'cpu');
    const publicName = edit(publicForm, 'cpu', false);
    const details = formFor(env.root, 'notes');
    edit(details, 'region', 'Osaka');
    edit(details, 'name', 'Saved new name');
    details.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.match(summary.textContent, /Osaka/);
    assert.equal(env.root.querySelector('h1').textContent, 'Saved new name');
    assert.equal(formFor(env.root, 'cpu'), publicForm);
    assert.equal(publicName.checked, false);
    assert.equal(publicForm.dataset.dirty, 'true');
    assert.equal(details.dataset.dirty, 'false');
    assert.equal(location.hash, '#settings');
  } finally { env.cleanup(); }
});

test('saving and discarding details preserve a neighboring public draft', async () => {
  const env = await setup({hash: '#settings'});
  try {
    const details = formFor(env.root, 'notes');
    const publicForm = formFor(env.root, 'cpu');
    edit(publicForm, 'cpu', false);
    edit(details, 'notes', 'Discard me');
    details.querySelectorAll('button').find((control) => control.textContent === copy('controls.discard')).click();
    assert.equal(control(details, 'notes').value, env.node.notes);
    assert.equal(details.dataset.dirty, 'false');
    assert.equal(control(publicForm, 'cpu').checked, false);
    assert.equal(publicForm.dataset.dirty, 'true');
    publicForm.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.equal(publicForm.dataset.dirty, 'false');
    edit(publicForm, 'cpu', true);
    publicForm.querySelectorAll('button').find((control) => control.textContent === copy('controls.discard')).click();
    assert.equal(control(publicForm, 'cpu').checked, false);
    assert.equal(publicForm.dataset.dirty, 'false');
  } finally { env.cleanup(); }
});
