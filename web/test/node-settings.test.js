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

async function setup({hash = '#settings', presence = 'online', lastSeen = '2026-10-28T10:00:00Z', patchStatus = 200, latest = null, expiresOn = null, metadata = {}, publicEnabled = true, publicPatchStatus = 200} = {}) {
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
    expires_on: expiresOn, last_seen: lastSeen, group_id: null, region: '', provider: '', notes: 'Saved notes', tags: [], created_at: '2026-10-27T10:00:00Z', ...metadata};
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
    else if (url === `${path}/public-settings`) {
      status = options.method === 'PATCH' ? publicPatchStatus : 200;
      data = status !== 200 ? {message: 'Public update rejected'} : options.method === 'PATCH' ? JSON.parse(options.body) : {enabled: publicEnabled, fields: ['cpu', 'memory', 'load', 'uptime', 'availability']};
    }
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

test('node has six separate tabs and Manage loads commands without runtime or enrollment lists', async () => {
  const env = await setup({hash: '#manage'});
  try {
    const tabs = elements(env.root).filter((element) => element.getAttribute('role') === 'tab');
    assert.deepEqual(tabs.map((tab) => tab.id), ['tab-monitor', 'tab-traffic', 'tab-probes', 'tab-public', 'tab-manage', 'tab-settings']);
    assert.equal(env.root.querySelector('#tab-manage').getAttribute('aria-selected'), 'true');
    assert.equal(env.root.querySelectorAll('h2').map((item) => item.textContent).join('|'), [copy('nodes.commands'), copy('nodes.install'), copy('nodes.dangerZone')].join('|'));
    assert.equal(env.root.querySelector('.node-manage').querySelectorAll('.node-action-row').length, 3);
    assert.equal(env.root.querySelector('header').querySelectorAll('button').length, 1, 'the sticky header holds enable/disable only');
    assert.equal(env.root.querySelector('.node-summary').querySelectorAll('button').length, 1, 'copy ID sits in the summary');
    assert.equal(env.root.querySelector('[role="menu"]'), null);
    assert.equal(env.requests.some((request) => request.url.endsWith('/runtime') || request.url.startsWith('/api/v1/enrollments')), false);
    assert.equal(env.root.querySelector('.node-summary').querySelectorAll('time').at(-1).getAttribute('datetime'), env.node.last_seen);
    assert.equal(formFor(env.root, 'notes'), undefined);
    assert.equal(formFor(env.root, 'cpu'), undefined);
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

for (const hash of ['monitor', 'traffic', 'probes', 'public', 'manage', 'settings']) {
  test(`#${hash} directly selects its node tab`, async () => {
    const env = await setup({hash: `#${hash}`});
    try {
      assert.equal(env.root.querySelector(`#tab-${hash}`).getAttribute('aria-selected'), 'true');
      assert.ok(env.root.querySelector(`#panel-${hash}`));
      assert.equal(env.root.querySelector('[data-section-link]'), null);
    } finally { env.cleanup(); }
  });
}

for (const hash of ['details', 'agent', 'settings-details', 'settings-public', 'settings-agent']) {
  test(`removed #${hash} alias falls back to monitoring`, async () => {
    const env = await setup({hash: `#${hash}`});
    try { assert.equal(env.root.querySelector('#tab-monitor').getAttribute('aria-selected'), 'true'); }
    finally { env.cleanup(); }
  });
}

test('the sticky header keeps one row and live metadata never replaces a settings draft', async () => {
  const env = await setup({hash: '#settings'});
  try {
    const head = env.root.querySelector('header');
    assert.deepEqual(head.children.map((child) => child.className), ['detail-head', 'detail-actions']);
    const back = head.querySelector('a');
    assert.equal(back.className, 'back');
    assert.equal(back.textContent, copy('nodes.allNodes'));
    assert.equal(back.title, copy('nodes.allNodes'));
    const summary = env.root.querySelector('.node-summary');
    assert.equal(head.querySelector('.node-summary'), null);
    assert.equal(summary.parentElement, head.parentElement);
    assert.equal(summary.querySelector('.node-id').querySelector('.mono').textContent, id.slice(0, 8));
    const details = formFor(env.root, 'notes');
    const draft = edit(details, 'notes', 'Private draft'); draft.focus();
    env.send({node: {...env.node, geo_location: 'California, US', geo_country: 'us', remote_ip: '203.0.113.5', provider: 'Provider', tags: ['edge'], last_seen: '2026-10-28T10:01:00Z'}, latest: null});
    assert.ok(summary.textContent.includes('🇺🇸 California, US'));
    assert.ok(summary.querySelector('.node-auto'));
    assert.equal(summary.textContent.includes('·'), false);
    assert.equal(summary.querySelectorAll('.tag').length, 1);
    assert.ok(summary.textContent.includes('203.0.113.5'));
    assert.deepEqual(summary.querySelectorAll('time').map((time) => time.getAttribute('datetime')), ['2026-10-28T10:01:00Z']);
    assert.equal(formFor(env.root, 'notes'), details);
    assert.equal(draft.value, 'Private draft');
    assert.equal(env.document.activeElement, draft);
    assert.equal(details.dataset.dirty, 'true');
    edit(details, 'region', 'Osaka');
    edit(details, 'name', 'Saved new name');
    details.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.ok(summary.textContent.includes('Osaka'));
    assert.equal(summary.querySelector('.node-auto'), null);
    assert.equal(head.querySelector('h1').textContent, 'Saved new name');
    assert.equal(details.dataset.dirty, 'false');
  } finally { env.cleanup(); }
});

test('one settings form saves every metadata field and discard restores all groups', async () => {
  const env = await setup({hash: '#settings'});
  try {
    const form = formFor(env.root, 'notes');
    assert.equal(env.root.querySelectorAll('form').length, 1);
    assert.equal(form.querySelectorAll('.node-settings-card').length, 4);
    assert.equal(form.querySelectorAll('.form-actions').length, 1);
    assert.equal(form.querySelector('.form-actions').classList.contains('node-save-bar'), true);
    assert.equal(control(form, 'region').getAttribute('placeholder'), copy('nodes.autoDetect'));
    assert.deepEqual(control(form, 'billing_cycle').options.map((option) => option.value), ['', 'month', 'quarter', 'half_year', 'year', 'once']);
    const values = {name: 'Edge', group_id: '', region: 'Tokyo', provider: 'Cloud', display_order: '7', expires_on: '2028-02-29', price_amount: '12.50', price_currency: 'USD', billing_cycle: 'year', auto_renew: true, bandwidth: '1 Gbps', ipv4: true, ipv6: false, network_route: 'Direct', plan_extra: 'Premium', notes: 'Private'};
    for (const [key, value] of Object.entries(values)) edit(form, key, value);
    form.querySelector('[data-tags]').setValue(['edge', 'prod']);
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    const patch = env.requests.find((request) => request.url === path && request.options.method === 'PATCH');
    assert.deepEqual(JSON.parse(patch.options.body), {...values, group_id: null, display_order: 7, tags: ['edge', 'prod']});
    assert.equal(form.dataset.dirty, 'false');
    edit(form, 'auto_renew', false); edit(form, 'price_amount', '99'); edit(form, 'notes', 'Discard');
    form.querySelectorAll('button').find((item) => item.textContent === copy('controls.discard')).click();
    assert.equal(control(form, 'auto_renew').checked, true);
    assert.equal(control(form, 'price_amount').value, '12.50');
    assert.equal(control(form, 'notes').value, 'Private');
    assert.equal(form.dataset.dirty, 'false');
  } finally { env.cleanup(); }
});

test('status-page groups save the whitelist, disable fields when off, and update both publication badges', async () => {
  const env = await setup({hash: '#public'});
  try {
    const form = formFor(env.root, 'enabled');
    assert.equal(form.querySelectorAll('fieldset').length, 3);
    const fields = ['region', 'provider', 'cpu', 'memory', 'load', 'uptime', 'availability', 'traffic', 'expires_on', 'price', 'billing_cycle', 'bandwidth', 'ip_version', 'network_route', 'plan_extra'];
    assert.deepEqual(elements(form).filter((item) => item.name && item.name !== 'enabled').map((item) => item.name), fields);
    assert.equal(form.querySelector('.callout').hidden, true);
    assert.equal(env.root.querySelector('.node-public-badge').textContent, copy('nodes.published'));
    assert.ok(env.root.querySelector('#tab-public').getAttribute('aria-label').includes(copy('nodes.published')));
    assert.equal(env.root.querySelector('#panel-public').querySelector('a').getAttribute('href'), '/');
    const enabled = edit(form, 'enabled', false); enabled.dispatchEvent(new Event('change'));
    assert.ok(fields.every((name) => control(form, name).disabled));
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    const patches = () => env.requests.filter((request) => request.url === `${path}/public-settings` && request.options.method === 'PATCH');
    assert.deepEqual(JSON.parse(patches()[0].options.body), {enabled: false, fields: ['cpu', 'memory', 'load', 'uptime', 'availability']});
    assert.equal(env.root.querySelector('.node-public-badge').textContent, copy('nodes.notPublished'));
    assert.ok(env.root.querySelector('#tab-public').getAttribute('aria-label').includes(copy('nodes.notPublished')));
    edit(form, 'enabled', true).dispatchEvent(new Event('change'));
    for (const key of fields) edit(form, key, ['region', 'price', 'traffic'].includes(key));
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.deepEqual(JSON.parse(patches()[1].options.body), {enabled: true, fields: ['region', 'traffic', 'price']});
    edit(form, 'enabled', false).dispatchEvent(new Event('change'));
    form.querySelectorAll('button').find((item) => item.textContent === copy('controls.discard')).click();
    assert.equal(enabled.checked, true);
    assert.ok(fields.every((name) => !control(form, name).disabled));
    env.send({node: {...env.node, status: 'disabled', presence: 'disabled'}, latest: null});
    assert.equal(form.querySelector('.callout').hidden, false);
    env.send({node: {...env.node, status: 'unknown', presence: 'pending'}, latest: null});
    assert.equal(form.querySelector('.callout').hidden, false);
    env.send({node: {...env.node, status: 'unknown', presence: 'online'}, latest: null});
    assert.equal(form.querySelector('.callout').hidden, true);
  } finally { env.cleanup(); }
});

test('public header badge selects the Status page tab using tabSet dirty protection', async () => {
  const env = await setup({hash: '#settings'});
  try {
    const form = formFor(env.root, 'notes'); edit(form, 'notes', 'Draft');
    env.root.querySelector('.node-public-badge').click(); await flush();
    assert.equal(env.root.querySelector('#tab-settings').getAttribute('aria-selected'), 'true');
    const dialog = env.body.querySelector('dialog');
    assert.ok(dialog);
    dialog.querySelectorAll('button').find((item) => item.textContent === copy('controls.discard')).click();
    dialog.dispatchEvent(new Event('animationend')); await flush();
    assert.equal(env.root.querySelector('#tab-public').getAttribute('aria-selected'), 'true');
    assert.equal(location.hash, '#public');
    assert.equal(env.document.activeElement, env.root.querySelector('#tab-public'));
  } finally { env.cleanup(); }
});


test('detected location is only a placeholder; saving an empty region preserves auto-detection', async () => {
  const env = await setup({metadata: {geo_location: 'California, US', geo_country: 'US'}});
  try {
    const form = formFor(env.root, 'notes');
    const region = control(form, 'region');
    assert.equal(region.value, '');
    assert.equal(region.getAttribute('placeholder'), copy('nodes.autoLocation', {location: 'California, US'}));
    edit(form, 'notes', 'Keep auto');
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(JSON.parse(env.requests.find((request) => request.options.method === 'PATCH').options.body).region, '');
  } finally { env.cleanup(); }
});

test('initially hidden public fields stay selected but disabled; rejected saves preserve the draft and badges', async () => {
  const env = await setup({hash: '#public', publicEnabled: false, publicPatchStatus: 400});
  try {
    const form = formFor(env.root, 'enabled');
    assert.equal(control(form, 'cpu').checked, true);
    assert.equal(control(form, 'cpu').disabled, true);
    edit(form, 'enabled', true).dispatchEvent(new Event('change'));
    assert.equal(control(form, 'cpu').disabled, false);
    edit(form, 'cpu', false);
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(form.dataset.dirty, 'true');
    assert.equal(control(form, 'enabled').checked, true);
    assert.equal(control(form, 'cpu').checked, false);
    assert.equal(form.querySelector('#public-save').disabled, false);
    assert.equal(env.root.querySelector('.node-public-badge').textContent, copy('nodes.notPublished'));
    assert.ok(toastHost.querySelector('.error'));
    form.querySelectorAll('button').find((item) => item.textContent === copy('controls.discard')).click();
    assert.equal(control(form, 'enabled').checked, false);
    assert.equal(control(form, 'cpu').checked, true);
    assert.equal(control(form, 'cpu').disabled, true);
    assert.equal(form.dataset.dirty, 'false');
  } finally { env.cleanup(); }
});

test('short header ID copies the complete UUID', async () => {
  const env = await setup();
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  let copied;
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {clipboard: {writeText: async (text) => { copied = text; }}}});
  try {
    env.root.querySelector('.node-id').querySelector('button').click(); await flush();
    assert.equal(copied, id);
  } finally {
    if (previous) Object.defineProperty(globalThis, 'navigator', previous); else delete globalThis.navigator;
    env.cleanup();
  }
});
