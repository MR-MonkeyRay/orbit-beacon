import test from 'node:test';
import assert from 'node:assert/strict';
import {invalidate, setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, elements, flush, installDOM} from './console-dom.js';
import {validResponse, validService} from '../src/public/validate.js';

const id = '00000000-0000-4000-8000-000000000001';
const service = (extra = {}) => ({id, name: 'Health', type: 'http', target: 'https://internal.example/health', interval_seconds: 60, timeout_seconds: 10,
  failure_threshold: 2, latency_threshold_ms: 0, display_order: 0, public: true, notify: true, enabled: true, latest: {status: 'up', latency_ms: 12}, availability_24h: 99.9, ...extra});
const admin = () => setSession({csrf_token: 'csrf', user: {id, username: 'admin', must_change_password: false}, expires_at: ''});
const control = (root, name) => elements(root).find((element) => element.name === name);

test('service editor switches target placeholders and submits typed payloads including false and zero', async () => {
  const dom = installDOM(); const originalFetch = globalThis.fetch; setNavigationSignal(null); admin();
  const sent = []; let saved = 0;
  globalThis.fetch = async (url, options) => { sent.push({url: String(url), method: options.method, body: JSON.parse(options.body)}); return new Response(JSON.stringify(service())); };
  try {
    const {serviceEditor, servicePayload} = await import('../src/console/pages/services.js');
    const dialog = serviceEditor(null, {signal: new AbortController().signal, saved: async () => { saved++; }});
    const root = dom.body;
    assert.equal(control(root, 'target').placeholder, 'https://example.com/health');
    const type = control(root, 'type'); type.value = 'tcp'; type.dispatchEvent(new Event('change'));
    assert.equal(control(root, 'target').placeholder, 'example.com:443');
    type.value = 'icmp'; type.dispatchEvent(new Event('change'));
    assert.equal(control(root, 'target').placeholder, 'example.com');
    control(root, 'name').value = ' Ping '; control(root, 'target').value = ' example.com ';
    const fields = Object.fromEntries(['name', 'type', 'target', 'interval_seconds', 'timeout_seconds', 'failure_threshold', 'latency_threshold_ms', 'display_order', 'public', 'notify', 'enabled'].map((name) => [name, control(root, name)]));
    assert.equal(String(control(root, 'failure_threshold').getAttribute('min') ?? control(root, 'failure_threshold').min), '1'); assert.equal(String(control(root, 'failure_threshold').getAttribute('max') ?? control(root, 'failure_threshold').max), '10');
    assert.deepEqual(servicePayload(fields), {name: 'Ping', type: 'icmp', target: 'example.com', interval_seconds: 60, timeout_seconds: 10, failure_threshold: 2, latency_threshold_ms: 0, display_order: 0, public: false, notify: false, enabled: true});
    for (const name of ['public', 'notify', 'enabled']) assert.equal(control(root, name).getAttribute('role'), 'switch');
    root.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(sent[0].url, '/api/v1/services'); assert.equal(sent[0].method, 'POST'); assert.equal(saved, 1);
    // 关闭动画以 260ms 计时器收尾，等它真正移除，否则下面会找到上一个表单。
    dialog.close('done'); await dialog.closed;
    const edit = serviceEditor(service(), {saved: async () => { saved++; }});
    control(root, 'enabled').checked = false; control(root, 'display_order').value = '-3';
    root.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    // 编辑只提交改动的字段。
    assert.equal(sent[1].method, 'PATCH'); assert.deepEqual(sent[1].body, {enabled: false, display_order: -3});
    edit.close('done');
  } finally { setSession({user: null, csrf_token: '', expires_at: ''}); dom.restore(); globalThis.fetch = originalFetch; }
});

test('services table, permission hint, history ranges and empty add action render', async () => {
  const dom = installDOM(); const originalFetch = globalThis.fetch; const originalMatch = globalThis.matchMedia;
  globalThis.matchMedia = () => ({matches: true}); const navigation = new AbortController(); setNavigationSignal(navigation.signal); admin();
  const ranges = []; let empty = false;
  globalThis.fetch = async (url) => {
    if (String(url).includes('/history')) { ranges.push(new URL(String(url), 'https://example.com').searchParams.get('range')); return new Response(JSON.stringify({range: ranges.at(-1), step_seconds: 300, buckets: []})); }
    return new Response(JSON.stringify({services: empty ? [] : [service({type: 'icmp', latest: {status: 'down', error: 'permission_denied', latency_ms: null}})]}));
  };
  try {
    const {servicesPage, serviceDetail} = await import('../src/console/pages/services.js');
    const page = await servicesPage(); dom.body.append(page);
    assert.ok(page.querySelector('table')); assert.ok(page.textContent.includes('net.ipv4.ping_group_range')); assert.ok(page.textContent.includes('Health'));
    const dialog = serviceDetail(service(), navigation.signal); await flush();
    assert.deepEqual(ranges, ['24h']);
    const range = elements(dom.body).find((e) => e.tagName === 'button' && e.textContent === copy('services.range.7d'));
    range.click(); await flush(); assert.deepEqual(ranges, ['24h', '7d']);
    assert.ok(dom.body.textContent.includes(copy('services.noHistory'))); dialog.close('done'); await flush();
    navigation.abort(); page.remove();
    empty = true; const next = new AbortController(); setNavigationSignal(next.signal); const blank = await servicesPage(); dom.body.append(blank);
    assert.ok(blank.textContent.includes(copy('services.empty'))); assert.equal(blank.querySelector('table'), null);
    elements(blank).find((e) => e.tagName === 'button' && e.textContent === copy('services.add')).click();
    assert.ok(dom.body.querySelector('form')); next.abort();
    const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs();
  } finally { navigation.abort(); setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''}); dom.restore(); globalThis.fetch = originalFetch; globalThis.matchMedia = originalMatch; }
});

const groupA = {id: '10000000-0000-4000-8000-00000000000a', name: 'Edge', display_order: 2};
const groupB = {id: '10000000-0000-4000-8000-00000000000b', name: 'Spare', display_order: 1};
const sid = (n) => `20000000-0000-4000-8000-00000000000${n}`;
const fixture = () => [
  service({id: sid(1), name: 'API', group_id: groupA.id}),
  service({id: sid(2), name: 'CDN', group_id: groupA.id, target: 'https://cdn.example', latest: {status: 'down', error: 'timeout', latency_ms: null}}),
  service({id: sid(3), name: 'Old', group_id: groupA.id, enabled: false, latest: null}),
  service({id: sid(4), name: 'Mail', group_id: null, type: 'tcp', target: 'mail.example:25', latest: {status: 'up', latency_ms: 3}})
];
const json = (body, status = 200) => new Response(JSON.stringify(body), {status, headers: {'content-type': 'application/json'}});

async function groupedPage({groupsFail = false, storage = new Map(), services = fixture(), groups = [groupA, groupB]} = {}) {
  const dom = installDOM(); const originalFetch = globalThis.fetch;
  globalThis.localStorage = {getItem: (key) => storage.get(key) ?? null, setItem: (key, value) => { storage.set(key, String(value)); }};
  const navigation = new AbortController(); setNavigationSignal(navigation.signal); admin(); invalidate('serviceGroups');
  // gates 中按路径挂起的 Promise 让测试控制响应时机。
  const sent = []; const state = {groups, services, stats: null, batchFails: [], gates: {}};
  globalThis.fetch = async (url, options = {}) => {
    const path = new URL(String(url), 'https://example.com').pathname; const method = options.method || 'GET';
    const body = options.body ? JSON.parse(options.body) : undefined;
    sent.push({path, method, body});
    if (method !== 'GET' && state.gates[path]) await state.gates[path];
    if (path === '/api/v1/service-groups') return groupsFail ? json({error: {code: 'internal', message: 'boom'}}, 500) : json({service_groups: state.groups});
    if (path === '/api/v1/service-groups/order') { state.groups = body.ids.map((id) => state.groups.find((group) => group.id === id)); return json({service_groups: state.groups}); }
    if (path === '/api/v1/services' && method === 'POST') return json({...body, id: sid(9)}, 201);
    if (path === '/api/v1/services') return json({services: state.services});
    if (path === '/api/v1/services/batch') return json({results: body.service_ids.map((id) => state.batchFails?.includes(id)
      ? {service_id: id, status: 'failed', error: {code: 'not_found', message: 'Service not found'}} : {service_id: id, status: 'succeeded'})});
    const target = state.services.find((item) => path.endsWith(item.id));
    if (method === 'GET') return json({...target, stats: state.stats});
    // PATCH 生效到夹具，保存后的重新读取才能看到分组变化。
    if (method === 'PATCH') Object.assign(target, body);
    return json(target);
  };
  const {servicesPage} = await import('../src/console/pages/services.js');
  const page = await servicesPage(); dom.body.append(page);
  const restore = () => { navigation.abort(); setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''}); invalidate('serviceGroups'); dom.restore(); globalThis.fetch = originalFetch; };
  return {dom, page, sent, state, storage, restore};
}
const sections = (page) => page.querySelectorAll('tbody').map((body) => body.dataset.group);
const section = (page, key) => page.querySelectorAll('tbody').find((body) => body.dataset.group === key);
const lastDialog = (dom) => dom.body.querySelectorAll('dialog').at(-1);
const textButton = (root, text) => elements(root).find((e) => e.tagName === 'button' && e.textContent.startsWith(text));
const submit = async (dom) => { lastDialog(dom).querySelector('form').dispatchEvent(new Event('submit', {cancelable: true})); await flush(); await flush(); };

test('monitor groups follow server order, list ungrouped last and summarise enabled members', async () => {
  const env = await groupedPage();
  try {
    assert.deepEqual(sections(env.page), [groupA.id, groupB.id, 'none']);
    const edge = section(env.page, groupA.id);
    assert.equal(edge.querySelector('.service-group-name').textContent, 'Edge');
    assert.equal(edge.querySelector('.count').textContent, '3');
    // 有启用的成员故障即为故障；停用的成员不参与汇总。
    assert.equal(edge.querySelector('.service-group-head').querySelector('.badge').textContent, copy('services.status.down'));
    const spare = section(env.page, groupB.id);
    assert.equal(spare.querySelector('.badge'), null);
    assert.equal(spare.querySelector('.service-group-empty').textContent, copy('services.empty'));
    assert.equal(section(env.page, 'none').querySelector('.service-group-name').textContent, copy('nodes.ungrouped'));
    const {groupStatus} = await import('../src/console/pages/services.js');
    assert.equal(groupStatus([]), null);
    assert.equal(groupStatus([service({enabled: false})]), 'disabled');
    assert.equal(groupStatus([service(), service({latest: null})]), 'unknown');
    assert.equal(groupStatus([service(), service({enabled: false, latest: {status: 'down'}})]), 'up');
  } finally { env.restore(); }
});

test('collapsing a monitor group hides its rows, keeps focus and survives a reload', async () => {
  const storage = new Map();
  let env = await groupedPage({storage});
  try {
    const toggle = env.page.querySelector(`#service-group-${groupA.id}`);
    assert.equal(toggle.getAttribute('aria-expanded'), 'true');
    toggle.focus(); toggle.click();
    const collapsed = env.page.querySelector(`#service-group-${groupA.id}`);
    assert.equal(collapsed.getAttribute('aria-expanded'), 'false');
    assert.equal(section(env.page, groupA.id).querySelectorAll('tr').length, 1);
    assert.equal(document.activeElement, collapsed);
    assert.deepEqual(JSON.parse(storage.get('orbitbeacon-service-groups-collapsed')), [groupA.id]);
  } finally { env.restore(); }
  env = await groupedPage({storage});
  try {
    assert.equal(env.page.querySelector(`#service-group-${groupA.id}`).getAttribute('aria-expanded'), 'false');
    assert.equal(section(env.page, 'none').querySelectorAll('tr').length, 2);
  } finally { env.restore(); }
});

test('status filter and search show matching monitors with their groups expanded', async () => {
  const storage = new Map([['orbitbeacon-service-groups-collapsed', JSON.stringify([groupA.id])]]);
  const env = await groupedPage({storage});
  try {
    const down = textButton(env.page, copy('services.status.down'));
    assert.equal(down.querySelector('.count').textContent, '1');
    down.click();
    assert.deepEqual(sections(env.page), [groupA.id]);
    const toggle = env.page.querySelector(`#service-group-${groupA.id}`);
    assert.equal(toggle.disabled, true); assert.equal(toggle.getAttribute('aria-expanded'), 'true');
    assert.deepEqual(section(env.page, groupA.id).querySelectorAll('.service-name').map((name) => name.textContent), ['CDN']);
    textButton(env.page, copy('nodes.all')).click();
    const search = env.page.querySelector('#service-search');
    search.value = 'MAIL.example'; search.dispatchEvent(new Event('input'));
    assert.deepEqual(sections(env.page), ['none']);
    search.value = 'nothing'; search.dispatchEvent(new Event('input'));
    assert.equal(env.page.querySelector('tbody'), null);
    assert.ok(env.page.textContent.includes(copy('services.noMatch')));
    search.value = ''; search.dispatchEvent(new Event('input'));
    assert.equal(env.page.querySelector(`#service-group-${groupA.id}`).getAttribute('aria-expanded'), 'false');
  } finally { env.restore(); }
});

test('the monitor editor moves a monitor between groups and clears it with null', async () => {
  const env = await groupedPage();
  try {
    env.page.querySelector(`#service-edit-${sid(1)}`).click(); await flush();
    const group = control(lastDialog(env.dom), 'group_id');
    assert.equal(group.value, groupA.id);
    assert.deepEqual(group.options.map((option) => option.value), ['', groupA.id, groupB.id, '__new']);
    group.value = ''; group.dispatchEvent(new Event('change'));
    await submit(env.dom);
    const first = env.sent.find((call) => call.method === 'PATCH');
    assert.equal(first.path, `/api/v1/services/${sid(1)}`); assert.equal(first.body.group_id, null);
    assert.ok(section(env.page, 'none').querySelectorAll('.service-name').some((name) => name.textContent === 'API'));
    assert.equal(section(env.page, groupA.id).querySelector('.count').textContent, '2');
    env.page.querySelector(`#service-edit-${sid(4)}`).click(); await flush();
    const next = control(lastDialog(env.dom), 'group_id');
    assert.equal(next.value, '');
    next.value = groupB.id; next.dispatchEvent(new Event('change'));
    await submit(env.dom);
    assert.equal(env.sent.filter((call) => call.method === 'PATCH').at(-1).body.group_id, groupB.id);
    assert.deepEqual(section(env.page, groupB.id).querySelectorAll('.service-name').map((name) => name.textContent), ['Mail']);
    // 未改动分组的保存不带 group_id，不会覆盖别处的移动。
    env.page.querySelector(`#service-edit-${sid(2)}`).click(); await flush();
    control(lastDialog(env.dom), 'name').value = 'CDN edge';
    await submit(env.dom);
    const unchanged = env.sent.filter((call) => call.method === 'PATCH').at(-1);
    assert.deepEqual(unchanged.body, {name: 'CDN edge'});
  } finally { const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs(); env.restore(); }
});

test('a failed group read keeps monitors in one list and leaves the group untouched on save', async () => {
  const env = await groupedPage({groupsFail: true});
  try {
    assert.deepEqual(sections(env.page), [undefined]);
    assert.equal(env.page.querySelector('.service-group-toggle'), null);
    assert.equal(env.page.querySelectorAll('.service-name').length, 4);
    env.page.querySelector(`#service-edit-${sid(1)}`).click(); await flush();
    assert.equal(control(lastDialog(env.dom), 'group_id'), undefined);
    control(lastDialog(env.dom), 'name').value = 'API v2';
    await submit(env.dom);
    assert.deepEqual(env.sent.find((call) => call.method === 'PATCH').body, {name: 'API v2'});
  } finally { const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs(); env.restore(); }
});

test('managing monitor groups counts members and reorders through the service-groups endpoint', async () => {
  const env = await groupedPage();
  try {
    textButton(env.page, copy('nodes.manageGroups')).click(); await flush();
    const dialog = lastDialog(env.dom);
    assert.equal(dialog.querySelector('h2').textContent, copy('nodes.manageGroups'));
    assert.ok(dialog.textContent.includes(copy('services.count', {count: 3})));
    assert.ok(dialog.textContent.includes(copy('services.count', {count: 0})));
    elements(dialog).find((e) => e.getAttribute?.('aria-label') === copy('nodes.moveDownNamed', {name: 'Edge'})).click();
    await flush(); await flush(); await flush();
    assert.deepEqual(env.sent.find((call) => call.method === 'PUT'), {path: '/api/v1/service-groups/order', method: 'PUT', body: {ids: [groupB.id, groupA.id]}});
    assert.deepEqual(sections(env.page), [groupB.id, groupA.id, 'none']);
  } finally { const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs(); env.restore(); }
});

test('monitor groups stay visible before any monitor exists', async () => {
  const env = await groupedPage({services: []});
  try {
    assert.deepEqual(sections(env.page), [groupA.id, groupB.id]);
    assert.equal(env.page.textContent.includes(copy('services.noMatch')), false);
    assert.equal(section(env.page, groupA.id).querySelector('.service-group-empty').textContent, copy('services.empty'));
  } finally { env.restore(); }
});

test('the group manager moves the clicked group even after the list changed underneath it', async () => {
  const dom = installDOM(); const originalFetch = globalThis.fetch; setNavigationSignal(null); admin();
  const sent = [];
  globalThis.fetch = async (url, options = {}) => { sent.push({url: String(url), body: options.body ? JSON.parse(options.body) : undefined}); return json({service_groups: []}); };
  try {
    const {SERVICE_GROUPS, manageGroupsDialog} = await import('../src/console/lib/groups.js');
    let list = [groupA, groupB];
    const dialog = manageGroupsDialog({kind: SERVICE_GROUPS, groups: () => list, counts: () => new Map(), changed: async () => {}});
    // 页面轮询在对话框打开期间插入了新分组，排在最前。
    const groupC = {id: '10000000-0000-4000-8000-00000000000c', name: 'New', display_order: 3};
    list = [groupC, groupA, groupB];
    elements(dom.body).find((e) => e.getAttribute?.('aria-label') === copy('nodes.moveDownNamed', {name: 'Edge'})).click();
    await flush(); await flush();
    assert.equal(sent[0].url, '/api/v1/service-groups/order');
    assert.deepEqual(sent[0].body.ids, [groupC.id, groupB.id, groupA.id]);
    dialog.close('done');
  } finally { const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs(); setSession({user: null, csrf_token: '', expires_at: ''}); dom.restore(); globalThis.fetch = originalFetch; }
});

const checkboxOf = (root, id) => root.querySelector(`#${id}`);
const tick = (control, checked = true) => { control.checked = checked; control.dispatchEvent(new Event('change')); };

test('selected monitors run batch actions and only failures are reported', async () => {
  const env = await groupedPage();
  try {
    const bar = env.page.querySelector('.bulk-actions');
    assert.equal(bar.hidden, true);
    tick(checkboxOf(env.page, `service-select-${sid(1)}`)); tick(checkboxOf(env.page, `service-select-${sid(4)}`));
    assert.equal(bar.hidden, false);
    assert.equal(bar.querySelector('.selection-count').textContent, copy('services.selected', {count: 2}));
    const groupBox = checkboxOf(env.page, `service-group-select-${groupA.id}`);
    assert.equal(groupBox.indeterminate, true);
    env.state.batchFails = [sid(4)];
    textButton(bar, copy('services.batch.pause')).click(); await flush(); await flush(); await flush();
    const call = env.sent.find((item) => item.path === '/api/v1/services/batch');
    assert.deepEqual(call.body, {service_ids: [sid(1), sid(4)], action: 'pause'});
    const report = lastDialog(env.dom);
    assert.equal(report.querySelector('h2').textContent, copy('services.batch.failed', {count: 1}));
    assert.ok(report.textContent.includes('Mail')); assert.equal(report.textContent.includes('API'), false);
    assert.equal(env.page.querySelector('.bulk-actions').hidden, true);
    assert.equal(checkboxOf(env.page, `service-select-${sid(1)}`).checked, false);
    // 操作栏隐藏后焦点不能留在被隐藏的按钮上；失败列表关闭后回到“全选”。
    textButton(report, copy('nodes.close')).click(); report.dispatchEvent(new Event('animationend')); await flush();
    assert.equal(document.activeElement, checkboxOf(env.page, 'service-select-all'));
  } finally { const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs(); env.restore(); }
});

test('group and filtered select-all pick only matching monitors, collapsed groups included', async () => {
  const storage = new Map([['orbitbeacon-service-groups-collapsed', JSON.stringify([groupA.id])]]);
  const env = await groupedPage({storage});
  try {
    tick(checkboxOf(env.page, `service-group-select-${groupA.id}`));
    assert.equal(env.page.querySelector('.selection-count').textContent, copy('services.selected', {count: 3}));
    assert.equal(checkboxOf(env.page, `service-group-select-${groupA.id}`).checked, true);
    tick(checkboxOf(env.page, `service-group-select-${groupA.id}`), false);
    textButton(env.page, copy('services.status.down')).click();
    tick(checkboxOf(env.page, 'service-select-all'));
    assert.equal(env.page.querySelector('.selection-count').textContent, copy('services.selected', {count: 1}));
    textButton(env.page, copy('nodes.all')).click();
    assert.equal(checkboxOf(env.page, 'service-select-all').indeterminate, true);
    assert.equal(checkboxOf(env.page, `service-group-select-${groupB.id}`).disabled, true);
  } finally { env.restore(); }
});

test('batch move asks for a group, sends null for ungrouped, and delete needs confirmation', async () => {
  const env = await groupedPage();
  try {
    tick(checkboxOf(env.page, `service-select-${sid(1)}`));
    textButton(env.page.querySelector('.bulk-actions'), copy('services.batch.move')).click(); await flush();
    let dialog = lastDialog(env.dom);
    const target = control(dialog, 'group_id');
    assert.deepEqual(target.options.map((option) => option.value), ['', groupA.id, groupB.id, '__new']);
    dialog.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    // 关闭动画结束后才发请求。
    dialog.dispatchEvent(new Event('animationend')); await flush(); await flush(); await flush();
    assert.deepEqual(env.sent.find((item) => item.path === '/api/v1/services/batch').body, {service_ids: [sid(1)], action: 'move', group_id: null});
    tick(checkboxOf(env.page, `service-select-${sid(2)}`));
    textButton(env.page.querySelector('.bulk-actions'), copy('services.batch.delete')).click(); await flush();
    dialog = lastDialog(env.dom);
    assert.equal(dialog.querySelector('h2').textContent, copy('services.batch.deleteTitle', {count: 1}));
    textButton(dialog, copy('controls.cancel')).click(); dialog.dispatchEvent(new Event('animationend')); await flush(); await flush();
    assert.equal(env.sent.filter((item) => item.path === '/api/v1/services/batch').length, 1);
    assert.equal(env.page.querySelector('.bulk-actions').hidden, false);
  } finally { const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs(); env.restore(); }
});

test('row menu pauses or resumes a monitor and clones it into a new one', async () => {
  const env = await groupedPage();
  try {
    const menuOf = (id) => env.page.querySelector(`#service-menu-${id}`).parentElement;
    const items = (id) => menuOf(id).querySelectorAll('.menu-item');
    assert.deepEqual(items(sid(3)).map((item) => item.textContent), [copy('services.resume'), copy('services.clone'), copy('services.delete')]);
    items(sid(1))[0].click(); await flush(); await flush();
    assert.deepEqual(env.sent.find((item) => item.method === 'PATCH').body, {enabled: false});
    assert.equal(items(sid(1))[0].textContent, copy('services.resume'));
    items(sid(2))[1].click(); await flush();
    const dialog = lastDialog(env.dom);
    assert.equal(control(dialog, 'name').value, `CDN${copy('services.cloneSuffix')}`);
    assert.equal(control(dialog, 'target').value, 'https://cdn.example');
    assert.equal(control(dialog, 'group_id').value, groupA.id);
    await submit(env.dom);
    const created = env.sent.find((item) => item.method === 'POST' && item.path === '/api/v1/services');
    assert.equal(created.body.name, `CDN${copy('services.cloneSuffix')}`); assert.equal(created.body.group_id, groupA.id);
    assert.equal(Object.hasOwn(created.body, 'id'), false);
    const {cloneName} = await import('../src/console/pages/services.js');
    assert.equal([...cloneName('名'.repeat(100))].length, 100);
  } finally { const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs(); env.restore(); }
});

// 挂起某路径的写请求，返回放行函数。
const gate = (env, path) => {
  let open; env.state.gates[path] = new Promise((done) => { open = done; });
  return () => { delete env.state.gates[path]; open(); };
};

test('a pending batch keeps selections made meanwhile and row actions on one monitor do not overlap', async () => {
  const env = await groupedPage();
  try {
    const release = gate(env, '/api/v1/services/batch');
    tick(checkboxOf(env.page, `service-select-${sid(1)}`));
    textButton(env.page.querySelector('.bulk-actions'), copy('services.batch.pause')).click(); await flush();
    tick(checkboxOf(env.page, `service-select-${sid(4)}`));
    release(); await flush(); await flush(); await flush();
    assert.deepEqual(env.sent.find((item) => item.path === '/api/v1/services/batch').body.service_ids, [sid(1)]);
    assert.equal(checkboxOf(env.page, `service-select-${sid(1)}`).checked, false);
    assert.equal(checkboxOf(env.page, `service-select-${sid(4)}`).checked, true);
    assert.equal(env.page.querySelector('.selection-count').textContent, copy('services.selected', {count: 1}));
    const open = gate(env, `/api/v1/services/${sid(4)}`);
    const pause = () => env.page.querySelector(`#service-menu-${sid(4)}`).parentElement.querySelectorAll('.menu-item')[0];
    pause().click(); await flush(); pause().click(); await flush();
    open(); await flush(); await flush();
    assert.equal(env.sent.filter((item) => item.method === 'PATCH').length, 1);
  } finally { const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs(); env.restore(); }
});

test('saving an edit refocuses the re-rendered edit button and an unchanged edit sends nothing', async () => {
  const env = await groupedPage();
  try {
    const edit = env.page.querySelector(`#service-edit-${sid(2)}`);
    edit.focus(); edit.click(); await flush();
    const dialog = lastDialog(env.dom);
    control(dialog, 'name').value = 'CDN 2';
    await submit(env.dom);
    dialog.dispatchEvent(new Event('animationend')); await flush();
    const fresh = env.page.querySelector(`#service-edit-${sid(2)}`);
    assert.notEqual(fresh, edit);
    assert.equal(document.activeElement, fresh);
    fresh.click(); await flush();
    const before = env.sent.length;
    await submit(env.dom);
    assert.equal(env.sent.length, before);
  } finally { const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs(); env.restore(); }
});

test('the 24-hour strip shows one bar per hour and the detail shows window statistics', async () => {
  const hours = [...Array(22).fill(null), 100, 50];
  const env = await groupedPage({services: [service({id: sid(1), name: 'API', recent_hours: hours}), service({id: sid(2), name: 'Old'})]});
  try {
    const strip = env.page.querySelector('.service-strip');
    const bars = strip.querySelectorAll('.availability-day');
    assert.equal(bars.length, 24);
    assert.ok(bars[0].classList.contains('availability-neutral'));
    assert.ok(bars[22].classList.contains('availability-normal')); assert.ok(bars[23].classList.contains('availability-critical'));
    assert.equal(env.page.querySelectorAll('.service-strip').length, 1);
    env.state.stats = {availability_24h: 99.5, availability_7d: null, availability_30d: 98, avg_latency_ms_24h: 12.5};
    const {serviceDetail} = await import('../src/console/pages/services.js');
    serviceDetail(env.state.services[0], new AbortController().signal); await flush(); await flush();
    const tiles = lastDialog(env.dom).querySelectorAll('.stat-tile').map((tile) => tile.querySelector('dd').textContent);
    assert.equal(tiles.length, 4);
    assert.equal(tiles[1], copy('common.unavailable')); assert.ok(tiles[0].startsWith('99.5')); assert.ok(tiles[3].includes('12.5'));
  } finally { const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs(); env.restore(); }
});

test('a silenced monitor group shows its silence end in the header, and without groups in the toolbar', async () => {
  const until = '2030-01-01T02:00:00Z';
  const {formatTimestamp} = await import('../src/shared/format.js');
  const text = copy('services.silencedUntil', {time: formatTimestamp(new Date(until))});
  const services = fixture().map((item) => ({...item, silenced_until: item.group_id === groupA.id ? until : null}));
  let env = await groupedPage({services});
  try {
    const badgeOf = (key) => section(env.page, key).querySelector('.service-group-head').querySelectorAll('.badge').find((badge) => badge.textContent === text);
    assert.ok(badgeOf(groupA.id)?.classList.contains('disabled'));
    assert.equal(badgeOf('none'), undefined);
    assert.equal(badgeOf(groupB.id), undefined);
    assert.equal(env.page.querySelector('.services-silence').textContent, '');
  } finally { env.restore(); }
  // 没有分组时没有组头，范围为全部的静默显示在工具栏。
  env = await groupedPage({groups: [], services: fixture().map((item) => ({...item, group_id: null, silenced_until: until}))});
  try {
    assert.equal(env.page.querySelector('.service-group-head'), null);
    assert.equal(env.page.querySelector('.services-silence').textContent, text);
  } finally { env.restore(); }
});

test('public services strictly validate 30 nullable percentages and reject target/error leakage', () => {
  const entry = {name: 'Health', type: 'tcp', status: 'up', latency_ms: 3, availability: Array(30).fill(null)};
  const response = {services: [entry], nodes: [], has_more: false, next_offset: null, generated_at: '2026-11-11T12:00:00Z'};
  assert.equal(validResponse(response), true);
  for (const bad of [{...entry, target: 'secret'}, {...entry, error: 'timeout'}, {...entry, latency_ms: Infinity}, {...entry, latency_ms: -1}, {...entry, status: 'disabled'}, {...entry, type: 'udp'}, {...entry, availability: Array(29).fill(100)}, {...entry, availability: Array(30)}, {...entry, availability: Array(30).fill(101)}, {...entry, name: ''}, {...entry, name: 'Bad\nname'}]) assert.equal(validService(bad), false);
  assert.equal(validResponse({...response, services: undefined}), false);
});

test('public services reuse daily bars, update statuses, and hide when empty', async () => {
  const dom = installDOM();
  try {
    const {serviceSection} = await import('../src/public/services.js'); const section = serviceSection(); dom.body.append(section);
    assert.equal(section.hidden, true);
    section.update([{name: '<Health>', type: 'http', status: 'up', latency_ms: 4.5, availability: Array(30).fill(100)}], '2026-11-11T12:00:00Z');
    assert.equal(section.hidden, false); assert.equal(section.querySelector('strong').textContent, '<Health>'); assert.equal(section.querySelectorAll('.availability-day').length, 30);
    assert.ok(section.textContent.includes('4.5 ms')); assert.ok(section.querySelector('.availability').getAttribute('aria-label').includes('100%')); assert.ok(!section.querySelector('.availability').getAttribute('aria-label').includes(copy('public.availabilityNoData'))); assert.equal(section.querySelector('.dot').getAttribute('aria-label'), copy('public.serviceStatus.up'));
    section.update([{name: 'Health', type: 'http', status: 'down', latency_ms: null, availability: Array(30).fill(null)}], '2026-11-11T12:00:00Z');
    assert.equal(section.querySelector('.dot').getAttribute('aria-label'), copy('public.serviceStatus.down'));
    section.update([], '2026-11-11T12:00:00Z'); assert.equal(section.hidden, true);
  } finally { dom.restore(); }
});

test('service inbox details render the immutable event and link services without an alert lookup', async () => {
  const dom = installDOM(); setNavigationSignal(null);
  const record = {id, source_id: id, kind: 'service', transition: 'certificate_expiring', read: true, created_at: '2026-11-11T12:00:00Z',
    details: {name: 'Health', target: 'https://internal.example', error: '', latency_ms: 12, tls_expires_at: '2026-11-20T00:00:00Z'}};
  const calls = [];
  const {createNotificationInbox} = await import('../src/console/ui/notification-inbox.js');
  const inbox = createNotificationInbox({api: async (path) => {
    calls.push(path);
    if (path.startsWith('/notifications?')) return {notifications: [record], has_more: false, read_through: id};
    return record;
  }, isActive: () => true, isStale: () => false, failureText: () => '', refreshSummary: () => {}, openTaskDetails: () => { throw new Error('service is not a task'); }});
  try {
    await flush();
    const row = document.querySelector('.notification-row');
    elements(row).find((e) => e.tagName === 'button' && e.textContent.includes('Health')).click(); await flush();
    assert.ok(dom.body.textContent.includes(copy('services.certificateExpiry')));
    assert.ok(dom.body.textContent.includes(copy('services.event.certificate_expiring')));
    assert.equal(document.querySelector('.notification-source').querySelector('a').getAttribute('href'), '/services');
    assert.equal(calls.some((path) => path.startsWith('/alerts/')), false);
  } finally { inbox.close(); await inbox.closed; dom.restore(); }
});
