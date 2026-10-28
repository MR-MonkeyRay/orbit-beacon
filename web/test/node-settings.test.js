import test from 'node:test';
import assert from 'node:assert/strict';
import {invalidate, setNavigationSignal, setSession} from '../src/console/api.js';
import {elements, flush, installDOM, TestElement} from './console-dom.js';

const id = '00000000-0000-4000-8000-000000000001';
const path = `/api/v1/nodes/${id}`;
const toastHost = new TestElement('div');
toastHost.id = 'toasts';
const runtime = {report_interval_seconds: 10, disk_interval_seconds: 60, interfaces: [], disk_paths: [], probe_targets: []};
const control = (root, name) => elements(root).find((element) => element.name === name);
const formFor = (root, name) => root.querySelectorAll('form').find((form) => control(form, name));
const factValues = (root) => Object.fromEntries(root.querySelectorAll('dt').map((label) => [label.textContent, label.parentElement.querySelector('dd').textContent]));
const edit = (form, name, value) => {
  const input = control(form, name);
  input.value = value;
  input.dispatchEvent(new Event('input'));
  // The small console harness does not simulate DOM event propagation.
  form.dispatchEvent(new Event('input'));
  return input;
};

async function setup({hash = '#details', observed = runtime, presence = 'online', lastSeen = '2026-10-28T10:00:00Z', patchStatus = 200, latest = null} = {}) {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window', 'matchMedia', 'requestAnimationFrame', 'history', 'performance', 'setTimeout', 'clearTimeout'].map((key) => [key, globalThis[key]]));
  globalThis.window = new EventTarget();
  globalThis.matchMedia = () => ({matches: false, addEventListener() {}});
  globalThis.requestAnimationFrame = () => 0;
  let now = 0;
  let timerID = 0;
  const deadlines = new Map();
  globalThis.performance = {now: () => now};
  globalThis.setTimeout = (callback, delay) => { deadlines.set(++timerID, {callback, at: now + delay, delay}); return timerID; };
  globalThis.clearTimeout = (key) => deadlines.delete(key);
  location.pathname = `/nodes/${id}`;
  location.hash = hash;
  globalThis.history = {state: null, replaceState(_state, _unused, url) { location.hash = url.includes('#') ? `#${url.split('#')[1]}` : ''; }};
  toastHost.replaceChildren();
  dom.body.append(toastHost);
  const page = new AbortController();
  setNavigationSignal(page.signal);
  invalidate('groups', 'nodes');
  setSession({csrf_token: 'csrf', user: {id, username: 'administrator'}, expires_at: ''});
  const requests = [];
  const node = {id, name: 'Internal name', status: 'enrolled', presence, agent_version: 'test-agent',
    last_seen: lastSeen, group_id: null, region: '', provider: '', notes: 'Saved notes', tags: [], created_at: '2026-10-27T10:00:00Z'};
  const traffic = {billing_day: 1, quota_bytes: '0', cycle: {}, daily: [], updated_at: null};
  globalThis.fetch = async (url, options) => {
    requests.push({url, options});
    let data;
    let status = 200;
    if (url === path) data = node;
    else if (url === `${path}/runtime`) data = {runtime: observed};
    else if (url === `${path}/traffic-settings`) {
      status = options.method === 'PATCH' ? patchStatus : 200;
      data = status === 200 ? options.method === 'PATCH' ? {billing_day: 2, quota_bytes: '1000000000'} : {billing_day: 1, quota_bytes: '0'}
        : {message: 'Billing update rejected'};
    } else if (url === '/api/v1/groups') data = {groups: []};
    else if (url === `${path}/public-settings`) data = {enabled: true, name: 'Saved public name', region: '', provider: ''};
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
    return {...dom, root, page, requests, node, deadlines, advance(milliseconds) {
      now += milliseconds;
      for (const [key, timer] of [...deadlines]) if (timer.at <= now) { deadlines.delete(key); timer.callback(); }
    }, cleanup() {
      page.abort();
      setNavigationSignal(null);
      for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
      dom.restore();
    }};
  } catch (error) {
    page.abort(); setNavigationSignal(null);
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    dom.restore();
    throw error;
  }
}

test('node tabs show reported local settings through #agent without the removed config route', async () => {
  const env = await setup({hash: '#agent'});
  try {
    const tabs = elements(env.root).filter((element) => element.getAttribute('role') === 'tab');
    assert.equal(tabs.length, 4);
    assert.deepEqual(tabs.map((tab) => tab.id), ['tab-details', 'tab-public', 'tab-agent', 'tab-enrollment']);
    assert.equal(env.root.querySelector('#tab-agent').getAttribute('aria-selected'), 'true');
    assert.equal(env.root.querySelector('#tab-config'), null);
    const values = factValues(env.root);
    assert.deepEqual(Object.fromEntries(Object.entries(values).slice(0, 6)), {
      'Report interval': '10 seconds', 'Disk interval': '60 seconds', 'Network interfaces': 'Default route',
      'Disk paths': 'Agent default', 'Service probes': 'None configured', 'Agent version': 'test-agent'
    });
    assert.equal(env.root.querySelector('time').getAttribute('datetime'), env.node.last_seen);
    assert.equal(env.root.querySelectorAll('input,select,textarea').length, 0);
  } finally { env.cleanup(); }
});

test('agent settings distinguishes no report and offline last observations', async () => {
  for (const options of [
    {observed: null, presence: 'never', lastSeen: null},
    {presence: 'offline'},
    {presence: 'offline', lastSeen: null}
  ]) {
    const env = await setup({hash: '#agent', ...options});
    try {
      if (options.observed === null) {
        assert.equal(env.root.querySelectorAll('dl,input,select,textarea').length, 0);
      } else if (options.lastSeen === null) {
        assert.equal(env.root.querySelectorAll('time').length, 0);
        assert.equal(factValues(env.root)['Report interval'], '10 seconds');
      } else {
        assert.equal(env.root.querySelector('time').getAttribute('datetime'), env.node.last_seen);
      }
    } finally { env.cleanup(); }
  }
});

test('agent settings displays reported selections as inert text without editable controls', async () => {
  const observed = {report_interval_seconds: 5, disk_interval_seconds: 120, interfaces: ['eth0', 'wg0'],
    disk_paths: ['/', '/var/lib'], probe_targets: ['https://example.test/<img>']};
  const env = await setup({hash: '#agent', observed});
  try {
    const values = factValues(env.root);
    assert.equal(values['Report interval'], '5 seconds');
    assert.equal(values['Disk interval'], '120 seconds');
    assert.equal(values['Network interfaces'], 'eth0\nwg0');
    assert.equal(values['Disk paths'], '/\n/var/lib');
    assert.equal(values['Service probes'], observed.probe_targets[0]);
    const settings = env.root.querySelector('#panel-agent');
    assert.equal(settings.querySelectorAll('input,select,textarea,button,a,img').length, 0);
  } finally { env.cleanup(); }
});

test('removed #config hash does not alias the local agent settings tab', async () => {
  const env = await setup({hash: '#config'});
  try {
    assert.equal(env.root.querySelector('#tab-details').getAttribute('aria-selected'), 'true');
    assert.equal(env.root.querySelector('#tab-config'), null);
  } finally { env.cleanup(); }
});

test('billing saves exact decimal units, stays clean after submit, and preserves Notes, focus and tab', async () => {
  const env = await setup();
  try {
    const billing = formFor(env.root, 'billing_day');
    assert.ok(billing, 'Details has a separate traffic billing form');
    const headings = env.root.querySelectorAll('h2').map((heading) => heading.textContent);
    assert.equal(headings[headings.indexOf('Traffic') + 1], 'Traffic billing');
    assert.deepEqual(control(billing, 'quota_unit').options.map((option) => option.value), ['bytes', 'KB', 'MB', 'GB', 'TB']);
    assert.equal(control(billing, 'billing_day').getAttribute('min'), '1');
    assert.equal(control(billing, 'billing_day').getAttribute('max'), '28');
    const details = formFor(env.root, 'notes');
    const notes = edit(details, 'notes', 'Unsaved notes');
    edit(billing, 'billing_day', '2');
    edit(billing, 'quota_value', '01');
    const unit = edit(billing, 'quota_unit', 'GB');
    unit.dispatchEvent(new Event('change'));
    notes.focus();
    const trafficBefore = env.requests.filter((request) => request.url === `${path}/traffic?days=30`).length;
    const requestsBefore = env.requests.length;
    billing.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    const patch = env.requests.find((request) => request.url === `${path}/traffic-settings` && request.options.method === 'PATCH');
    assert.deepEqual(JSON.parse(patch.options.body), {billing_day: 2, quota_bytes: '1000000000'});
    assert.equal(billing.dataset.dirty, 'false');
    assert.equal(billing.querySelector('#billing-save').disabled, true);
    assert.equal(details.dataset.dirty, 'true');
    assert.equal(notes.value, 'Unsaved notes');
    assert.equal(env.document.activeElement, notes);
    assert.equal(env.root.querySelector('#tab-details').getAttribute('aria-selected'), 'true');
    assert.equal(location.hash, '#details');
    assert.equal(env.requests.filter((request) => request.url === `${path}/traffic?days=30`).length, trafficBefore + 1);
    assert.deepEqual(env.requests.slice(requestsBefore).map((request) => request.url), [`${path}/traffic-settings`, `${path}/traffic?days=30`]);
    edit(billing, 'quota_value', '2000000000');
    assert.equal(billing.dataset.dirty, 'true');
    billing.querySelectorAll('button').find((button) => button.textContent === 'Discard changes').click();
    assert.equal(billing.dataset.dirty, 'false');
    assert.equal(details.dataset.dirty, 'true');
  } finally { env.cleanup(); }
});

test('rejected billing saves preserve the draft and do not refresh traffic', async () => {
  const env = await setup({patchStatus: 400});
  try {
    const billing = formFor(env.root, 'billing_day');
    assert.ok(billing, 'Details has a separate traffic billing form');
    edit(billing, 'billing_day', '2');
    const before = env.requests.filter((request) => request.url === `${path}/traffic?days=30`).length;
    billing.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.equal(billing.dataset.dirty, 'true');
    assert.equal(control(billing, 'billing_day').value, '2');
    assert.equal(billing.querySelector('#billing-save').disabled, false);
    assert.equal(env.requests.filter((request) => request.url === `${path}/traffic?days=30`).length, before);
    assert.match(toastHost.textContent, /Billing update rejected/);
  } finally { env.cleanup(); }
});

test('billing revisits use the saved response and honor tab-change discard', async () => {
  const env = await setup();
  try {
    let billing = formFor(env.root, 'billing_day');
    edit(billing, 'billing_day', '2');
    billing.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    edit(billing, 'quota_value', '2000000000');
    env.root.querySelector('#tab-agent').click();
    await flush();
    const dialog = env.body.querySelector('dialog');
    assert.ok(dialog, 'leaving a dirty billing form asks before discarding');
    dialog.querySelectorAll('button').find((button) => button.textContent === 'Discard changes').click();
    dialog.dispatchEvent(new Event('animationend'));
    await flush();
    assert.equal(env.root.querySelector('#tab-agent').getAttribute('aria-selected'), 'true');
    env.root.querySelector('#tab-details').click();
    await flush();
    billing = formFor(env.root, 'billing_day');
    assert.equal(control(billing, 'billing_day').value, '2');
    assert.equal(billing.dataset.dirty, 'false');
    assert.equal(billing.querySelector('#billing-save').disabled, true);
  } finally { env.cleanup(); }
});

test('latest deadline patches leave dirty Notes, focus, current tab, and numeric nodes intact', async () => {
  const received = '2026-10-28T10:00:00Z';
  const latest = {received_at: received, sampled_at: received, server_time: received, seq: 1,
    metrics: {cpu_percent: 25, uptime_seconds: 3600, metric_status: {cpu_percent: 'ok', uptime_seconds: 'ok'},
      sources: {cpu: {age_ms: 0, source_interval_ms: 10000}, uptime: {age_ms: 0, source_interval_ms: 10000}}}};
  const env = await setup({latest});
  try {
    const details = formFor(env.root, 'notes');
    const notes = edit(details, 'notes', 'Do not replace this draft');
    notes.focus();
    const cpu = env.root.querySelectorAll('dt').find((label) => label.textContent === 'CPU').parentElement.querySelector('.sample-value');
    const before = env.requests.length;
    env.advance(45000);
    assert.equal(cpu.classList.contains('metric-stale'), false);
    env.advance(1);
    assert.equal(cpu.classList.contains('metric-stale'), true);
    assert.equal(cpu.textContent, '25%');
    assert.equal(env.requests.length, before);
    assert.equal(formFor(env.root, 'notes'), details);
    assert.equal(control(details, 'notes'), notes);
    assert.equal(notes.value, 'Do not replace this draft');
    assert.equal(details.dataset.dirty, 'true');
    assert.equal(env.document.activeElement, notes);
    assert.equal(env.root.querySelector('#tab-details').getAttribute('aria-selected'), 'true');
    setSession({csrf_token: '', user: null, expires_at: ''});
    assert.equal(env.root.querySelectorAll('.sample-value').length, 0);
    assert.equal(env.deadlines.size, 0);
  } finally { env.cleanup(); }
});
