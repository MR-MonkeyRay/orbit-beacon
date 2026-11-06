import test from 'node:test';
import assert from 'node:assert/strict';
import {invalidate, setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, copyPattern, elements, flush} from './console-dom.js';
import {frame, liveHarness, response, stream} from './live-harness.js';

const id = (number) => `00000000-0000-4000-8000-${String(number).padStart(12, '0')}`;
const reported = '2026-10-28T00:00:00Z';
const runtime = {report_interval_seconds: 10, disk_interval_seconds: 60, interfaces: [], disk_paths: [], probe_targets: [], remote_commands: ['upgrade', 'restart', 'stop'], disabled_features: []};
const node = (number) => ({id: id(number), name: `Node ${String(number).padStart(3, '0')}`, status: 'unknown', stopped: false, presence: 'online',
  last_seen: reported, agent_version: '1.0', group_id: null, region: '', provider: '', notes: 'Saved notes', tags: [], created_at: reported});
const latest = (seq = 1) => ({received_at: reported, sampled_at: reported, seq, runtime,
  system: {agent_version: '1.0'}, metrics: {cpu_percent: seq, uptime_seconds: 3600,
    metric_status: {cpu_percent: 'ok', uptime_seconds: 'ok'},
    sources: {cpu: {age_ms: 0, source_interval_ms: 10000}, uptime: {age_ms: 0, source_interval_ms: 10000}}}});
const compact = (value) => Object.fromEntries(['id', 'status', 'presence', 'stopped', 'last_seen', 'agent_version'].map((key) => [key, value[key]]));
const control = (root, name) => elements(root).find((element) => element.name === name);
const formFor = (root, name) => root.querySelectorAll('form').find((form) => control(form, name));
const button = (root, label) => root.querySelectorAll('button').find((item) => item.textContent === label);
const edit = (form, name, value) => {
  const input = control(form, name); input.value = value; input.dispatchEvent(new Event('input')); form.dispatchEvent(new Event('input')); return input;
};
const sampleFact = (root, label) => root.querySelectorAll('dt').find((item) => item.textContent === label)?.parentElement.querySelector('dd');

async function setup(t, {page = 'nodes', hash = '#monitor', count = 2, search = '', sample = latest(), mutate, commands = []} = {}) {
  const original = new Map(['requestAnimationFrame', 'cancelAnimationFrame', 'history', 'matchMedia'].map((key) => [key, globalThis[key]]));
  let scope;
  t.after(() => {
    scope?.abort(); setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''});
    for (const [key, value] of original) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  });
  let inventory = Array.from({length: count}, (_, index) => node(index + 1));
  let serverFleet = inventory.map(compact);
  let nextInventory;
  let nextFleet;
  const fallback = stream();
  const session = {user: {id: id(999), username: 'administrator', must_change_password: false}, csrf_token: 'csrf', expires_at: '2026-10-29T00:00:00Z'};
  const env = liveHarness(t, (request) => {
    const url = new URL(request.url, 'https://example.test'); const path = url.pathname;
    if (mutate && request.method && request.method !== 'GET') return mutate(request);
    if (path === '/api/v1/me') return response(session);
    if (path === '/api/v1/live') {
      const data = frame('snapshot', {data: {nodes: serverFleet}});
      if (!nextFleet) return response(data);
      const pending = nextFleet; nextFleet = null;
      return new Promise((resolve) => { pending.release = () => resolve(response(data)); });
    }
    if (path === '/api/v1/live/stream') return fallback.response;
    if (path === '/api/v1/nodes/batch') return response({results: JSON.parse(request.body).node_ids.map((node_id) => ({node_id, status: 'succeeded'}))});
    if (path === '/api/v1/nodes') {
      const data = {nodes: inventory.map((item) => ({...item})), has_more: false, next_offset: null};
      if (!nextInventory) return response(data);
      const pending = nextInventory; nextInventory = null;
      return new Promise((resolve) => { pending.release = () => resolve(response(data)); });
    }
    if (path === '/api/v1/groups') return response({groups: [{id: id(700), name: 'Production'}]});
    if (path === '/api/v1/users') return response({users: []});
    if (path === '/api/v1/audit') return response({events: []});
    if (path === '/api/v1/enrollments') return response({enrollments: [{id: id(500), node_id: id(1), expires_at: '2026-10-29T00:00:00Z', uses: 0, max_uses: 1, revoked_at: null}], has_more: false});
    if (path === '/api/v1/alerts/summary') return response({firing: {total: 0}, newest: []});
    if (path === `/api/v1/nodes/${id(1)}`) return response(inventory[0]);
    if (path.endsWith('/commands')) return response({commands});
    if (path.endsWith('/runtime')) return response({runtime});
    if (path.endsWith('/traffic-settings')) return response({billing_day: 1, quota_bytes: '0'});
    if (path.endsWith('/public-settings')) return response({enabled: true, name: 'Saved public name', region: '', provider: ''});
    if (path.endsWith('/latest')) return sample ? response({...sample, server_time: reported}) : response({message: 'No sample'}, {status: 404});
    if (path.endsWith('/traffic')) return response({billing_day: 1, quota_bytes: '0', cycle: {}, daily: [], updated_at: null});
    if (path.endsWith('/probes')) return response({probes: []});
    return response({message: `Unexpected request ${request.url}`}, {status: 503});
  });
  globalThis.requestAnimationFrame = (callback) => { callback(); return 0; };
  globalThis.cancelAnimationFrame = () => {};
  globalThis.matchMedia = () => ({matches: false, addEventListener() {}});
  env.window.scrollTo = () => {};
  Object.assign(location, {pathname: page === 'node' ? `/nodes/${id(1)}` : `/${page}`, search, hash, href: `https://example.test/${page}`});
  globalThis.history = {state: null, replaceState(_state, _unused, value) {
    const url = new URL(value, 'https://example.test'); Object.assign(location, {pathname: url.pathname, search: url.search, hash: url.hash});
  }};
  const toastHost = document.createElement('div'); toastHost.id = 'toasts'; env.body.append(toastHost);
  scope = new AbortController(); setNavigationSignal(scope.signal); invalidate('nodes', 'groups', 'users'); setSession(session);
  const module = await import(`../src/console/pages/${page}.js`);
  const root = await module[`${page}Page`](); env.body.append(root); env.mutations(); await flush();
  const socket = env.sockets[0];
  assert.ok(socket, 'page establishes its one authenticated live subscription'); socket.open();
  const send = (data, seconds = 0) => {
    serverFleet = data.nodes;
    const current = env.sockets.at(-1);
    if (current.readyState === 0) current.open();
    current.message(frame('snapshot', {server_time: new Date(Date.parse(reported) + seconds * 1000).toISOString(), data}));
  };
  return {...env, root, scope, socket, send, fallback, inventory,
    replaceInventory(value) { inventory = value; serverFleet = value.map(compact); },
    setServerFleet(value) { serverFleet = value.map(compact); },
    deferInventory() { nextInventory = {}; return nextInventory; },
    deferFleet() { nextFleet = {}; return nextFleet; }};
}

test('nodes ten typed live updates preserve query, caret, page, row identity and selected IDs until explicit re-filter', async (t) => {
  const env = await setup(t, {count: 55, search: '?q=Node&sort=status&page=1'});
  const search = env.root.querySelector('#node-search'); search.focus(); search.setSelectionRange(1, 3);
  const rows = [...env.root.querySelector('tbody').children];
  const boxes = env.root.querySelectorAll('input[data-node-id]');
  boxes[0].checked = true; boxes[0].dispatchEvent(new Event('change'));
  boxes[1].checked = true; boxes[1].dispatchEvent(new Event('change'));
  const query = location.search;
  for (let index = 1; index <= 10; index++) {
    env.send({nodes: env.inventory.map((item) => compact({...item, presence: item.id === boxes[0].dataset.nodeId ? 'offline' : 'online', agent_version: `1.${index}`}))}, index * 10);
    assert.equal(env.document.activeElement, search); assert.equal(search.value, 'Node');
    assert.equal(search.selectionStart, 1); assert.equal(search.selectionEnd, 3); assert.equal(location.search, query);
    assert.deepEqual(env.root.querySelector('tbody').children, rows); assert.equal(boxes[0].checked, true); assert.equal(boxes[1].checked, true);
  }
  assert.match(rows[0].textContent, copyPattern('common.status.offline'));
  assert.equal(button(env.root, copy('nodes.applyFilters')).parentElement.hidden, false);
  assert.equal(env.sockets.length, 1); assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.live'));
  button(env.root, copy('nodes.applyFilters')).click();
  assert.notDeepEqual(env.root.querySelector('tbody').children, rows);
});

test('nodes deletion tombstones only its selected ID and confirmation keeps frozen batch targets', async (t) => {
  const env = await setup(t);
  const boxes = env.root.querySelectorAll('input[data-node-id]');
  for (const box of boxes) { box.checked = true; box.dispatchEvent(new Event('change')); }
  button(env.root, copy('nodes.disable')).click(); await flush();
  const row = boxes[0].parentElement.parentElement.parentElement;
  env.send({nodes: [compact(env.inventory[1]), compact(node(3))]});
  assert.equal(boxes[0].disabled, true); assert.equal(boxes[0].checked, false); assert.equal(boxes[1].checked, true);
  assert.match(row.textContent, copyPattern('controls.deleted')); assert.doesNotMatch(row.textContent, copyPattern('common.status.offline'));
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, false);
  const dialog = env.body.querySelector('dialog'); button(dialog, copy('nodes.disableNodes')).click(); dialog.dispatchEvent(new Event('animationend')); await flush();
  const batch = env.requests.find((request) => request.url === '/api/v1/nodes/batch');
  assert.deepEqual(JSON.parse(batch.body).node_ids, [id(1), id(2)]);
});

test('nodes unknown inventory refresh is explicit and keeps filters while clamping the page', async (t) => {
  const env = await setup(t, {count: 55, search: '?q=Node&page=1'});
  const before = env.requests.filter((request) => request.url.startsWith('/api/v1/nodes?')).length;
  const replacement = [node(56)]; env.replaceInventory(replacement); env.send({nodes: replacement.map(compact)});
  assert.equal(env.requests.filter((request) => request.url.startsWith('/api/v1/nodes?')).length, before);
  button(env.root, copy('nodes.refreshList')).click(); await flush();
  assert.equal(env.requests.filter((request) => request.url.startsWith('/api/v1/nodes?')).length, before + 1);
  assert.equal(env.root.querySelector('#node-search').value, 'Node'); assert.equal(new URLSearchParams(location.search).get('q'), 'Node');
  assert.equal(new URLSearchParams(location.search).has('page'), false); assert.match(env.root.querySelector('tbody').textContent, /Node 056/);
  assert.doesNotMatch(env.root.querySelector('tbody').textContent, copyPattern('controls.deleted'));
});

test('nodes refresh overlays the newest complete fleet after a deferred metadata response', async (t) => {
  const env = await setup(t, {search: '?q=Node&sort=status'});
  const search = env.root.querySelector('#node-search'); search.focus(); search.setSelectionRange(1, 3);
  const boxes = env.root.querySelectorAll('input[data-node-id]');
  for (const box of boxes) { box.checked = true; box.dispatchEvent(new Event('change')); }
  env.send({nodes: [...env.inventory, node(3)].map(compact)});
  const pending = env.deferInventory(); button(env.root, copy('nodes.refreshList')).click(); await flush();
  assert.equal(typeof pending.release, 'function');
  const survivor = {...node(2), presence: 'offline', last_seen: '2026-10-28T00:00:10Z'};
  env.send({nodes: [compact(survivor), compact(node(4))]}, 10);
  pending.release(); await flush();
  const deletedBox = env.root.querySelectorAll('input[data-node-id]').find((box) => box.dataset.nodeId === id(1));
  const survivorBox = env.root.querySelectorAll('input[data-node-id]').find((box) => box.dataset.nodeId === id(2));
  assert.equal(deletedBox.disabled, true); assert.equal(deletedBox.checked, false);
  assert.match(deletedBox.parentElement.parentElement.parentElement.textContent, copyPattern('controls.deleted'));
  assert.equal(deletedBox.parentElement.parentElement.parentElement.querySelector('a').hasAttribute('href'), false);
  assert.equal(survivorBox.checked, true); assert.match(survivorBox.parentElement.parentElement.parentElement.textContent, copyPattern('common.status.offline'));
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, false, 'new inventory is not acknowledged by an older response');
  assert.equal(elements(env.root).find((item) => item.textContent === copy('nodes.selectionRemoved')).hidden, false);
  assert.equal(env.document.activeElement, search); assert.equal(search.value, 'Node');
  assert.equal(search.selectionStart, 1); assert.equal(search.selectionEnd, 3);
  assert.equal(location.search, '?q=Node&sort=status');
  await env.advance(10000); env.socket.message(frame('heartbeat', {server_time: '2026-10-28T00:00:20Z'}));
  assert.equal(deletedBox.disabled, true, 'a quiet stream cannot leave a resurrected actionable row');
  env.replaceInventory([survivor, node(4)]); button(env.root, copy('nodes.refreshList')).click(); await flush();
  assert.equal(env.root.querySelectorAll('input[data-node-id]').some((box) => box.dataset.nodeId === id(1)), false);
  assert.match(env.root.querySelector('tbody').textContent, /Node 004/);
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, true);
});

test('nodes metadata refresh accepts newer HTTP inventory absent from an older fleet', async (t) => {
  const env = await setup(t);
  env.send({nodes: [compact(node(2)), compact(node(3))]});
  env.replaceInventory([node(2), node(3), node(4)]);
  button(env.root, copy('nodes.refreshList')).click(); await flush();
  const boxes = env.root.querySelectorAll('input[data-node-id]');
  assert.deepEqual(boxes.map((box) => box.dataset.nodeId), [id(2), id(3), id(4)]);
  assert.equal(boxes.every((box) => !box.disabled), true);
  assert.doesNotMatch(env.root.querySelector('tbody').textContent, copyPattern('controls.deleted'));
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, true);
});

test('nodes post-batch reload overlays fleet changes received while metadata is pending', async (t) => {
  const env = await setup(t);
  for (const box of env.root.querySelectorAll('input[data-node-id]')) { box.checked = true; box.dispatchEvent(new Event('change')); }
  button(env.root, copy('nodes.disable')).click(); await flush();
  const pending = env.deferInventory(); const dialog = env.body.querySelector('dialog');
  button(dialog, copy('nodes.disableNodes')).click(); dialog.dispatchEvent(new Event('animationend')); await flush();
  assert.equal(typeof pending.release, 'function');
  env.send({nodes: [compact({...node(2), status: 'disabled', presence: 'disabled'}), compact(node(3))]}, 10);
  pending.release(); await flush();
  const boxes = env.root.querySelectorAll('input[data-node-id]');
  const removed = boxes.find((box) => box.dataset.nodeId === id(1));
  assert.equal(removed.disabled, true); assert.match(removed.parentElement.parentElement.parentElement.textContent, copyPattern('controls.deleted'));
  assert.match(boxes.find((box) => box.dataset.nodeId === id(2)).parentElement.parentElement.parentElement.textContent, copyPattern('common.status.disabled'));
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, false);
  assert.deepEqual(JSON.parse(env.requests.find((request) => request.url === '/api/v1/nodes/batch').body).node_ids, [id(1), id(2)]);
});
for (const delayed of ['metadata', 'fleet']) {
  test(`nodes concurrent manual and batch reloads fence late ${delayed}, keep one channel and confirm post-mutation state`, async (t) => {
    let env;
    const afterBatch = [{...node(1), name: 'Node 001 after batch'}, {...node(2), name: 'Node 002 after batch', status: 'disabled', presence: 'disabled'}];
    env = await setup(t, {mutate: (request) => {
      env.replaceInventory(afterBatch);
      return response({results: JSON.parse(request.body).node_ids.map((node_id) => ({node_id, status: 'succeeded'}))});
    }});
    const selected = env.root.querySelectorAll('input[data-node-id]').find((box) => box.dataset.nodeId === id(2));
    selected.checked = true; selected.dispatchEvent(new Event('change'));
    env.send({nodes: [...env.inventory, node(3)].map(compact)});
    const before = env.requests.filter((request) => request.url.startsWith('/api/v1/nodes?')).length;
    const older = delayed === 'metadata' ? env.deferInventory() : env.deferFleet();
    button(env.root, copy('nodes.refreshList')).click(); await flush();
    assert.equal(typeof older.release, 'function');
    const newer = env.deferInventory();
    button(env.root, copy('nodes.disable')).click(); await flush();
    const dialog = env.body.querySelector('dialog');
    button(dialog, copy('nodes.disableNodes')).click(); dialog.dispatchEvent(new Event('animationend')); await flush();
    assert.equal(env.requests.filter((request) => request.url === '/api/v1/nodes/batch').length, 1);
    const newerStarted = typeof newer.release === 'function';
    if (newerStarted) { newer.release(); await flush(); }
    older.release(); await flush();
    if (!newerStarted) {
      assert.equal(typeof newer.release, 'function', 'the batch must fetch metadata after the older reload, not join its pre-mutation result');
      newer.release(); await flush();
    }
    assert.equal(env.requests.filter((request) => request.url.startsWith('/api/v1/nodes?')).length, before + 2,
      'the queued post-mutation reload invalidates any metadata cached by its predecessor');
    const rows = env.root.querySelector('tbody').children;
    assert.match(rows.find((row) => row.textContent.includes('Node 002')).textContent, new RegExp(`Node 002 after batch.*${copyPattern('common.status.disabled').source}`));
    assert.match(rows.find((row) => row.textContent.includes('Node 001')).textContent, /Node 001 after batch/);
    let active = 0;
    for (const event of env.order) {
      if (event === 'ws:start') active++;
      if (event === 'ws:close') active--;
      assert.ok(active >= 0 && active <= 1, 'every reload must close its owned channel before another starts');
    }
    assert.equal(env.sockets.filter((socket) => !socket.closed).length, 1);
    const current = env.sockets.at(-1); current.open();
    current.message(frame('snapshot', {data: {nodes: afterBatch.map(compact)}}));
    assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.live'));
    env.scope.abort();
    assert.equal(env.sockets.every((socket) => socket.closed), true, 'navigation closes the final reload-owned channel');
    const channels = env.sockets.length;
    await env.advance(60000);
    assert.equal(env.sockets.length, channels, 'navigation leaves no reconnect timer');
  });
}

test('nodes unrelated batch keeps an existing deleted tombstone and explanation until explicit refresh', async (t) => {
  let env;
  env = await setup(t, {mutate: (request) => {
    env.replaceInventory([{...node(2), status: 'disabled', presence: 'disabled'}]);
    return response({results: JSON.parse(request.body).node_ids.map((node_id) => ({node_id, status: 'succeeded'}))});
  }});
  for (const box of env.root.querySelectorAll('input[data-node-id]')) { box.checked = true; box.dispatchEvent(new Event('change')); }
  env.send({nodes: [compact(node(2))]});
  const explanation = elements(env.root).find((item) => item.textContent === copy('nodes.selectionRemoved'));
  assert.equal(explanation.hidden, false);
  button(env.root, copy('nodes.disable')).click(); await flush();
  const dialog = env.body.querySelector('dialog');
  button(dialog, copy('nodes.disableNodes')).click(); dialog.dispatchEvent(new Event('animationend')); await flush();
  await flush();
  const boxes = env.root.querySelectorAll('input[data-node-id]');
  const removed = boxes.find((box) => box.dataset.nodeId === id(1));
  assert.ok(removed, 'a batch is not an acknowledgement of an unrelated deletion');
  assert.equal(removed.disabled, true); assert.equal(removed.checked, false);
  const row = removed.parentElement.parentElement.parentElement;
  assert.match(row.textContent, copyPattern('controls.deleted')); assert.equal(row.querySelector('a').hasAttribute('href'), false);
  assert.match(boxes.find((box) => box.dataset.nodeId === id(2)).parentElement.parentElement.parentElement.textContent, copyPattern('common.status.disabled'));
  assert.equal(explanation.hidden, false);
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, false);
  assert.deepEqual(JSON.parse(env.requests.find((request) => request.url === '/api/v1/nodes/batch').body).node_ids, [id(2)]);
  const results = env.body.querySelectorAll('dialog').find((item) => item.textContent.includes(copy('nodes.batchResults', {action: copy('nodes.disable')})));
  button(results, copy('nodes.close')).click(); results.dispatchEvent(new Event('animationend')); await flush();
  button(env.root, copy('nodes.refreshList')).click(); await flush();
  assert.equal(env.root.querySelectorAll('input[data-node-id]').some((box) => box.dataset.nodeId === id(1)), false);
  assert.equal(explanation.hidden, true);
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, true);
});


test('nodes live presence counts patch text without hiding focused segments or rebuilding filtered rows', async (t) => {
  const env = await setup(t, {search: '?q=Node&status=online&sort=status'});
  env.send({nodes: [compact(node(1)), compact({...node(2), presence: 'offline'})]});
  button(env.root, copy('nodes.applyFilters')).click();
  const segments = env.root.querySelector('.segmented').querySelectorAll('button');
  const online = segments.find((item) => item.textContent.startsWith(copy('common.status.online')));
  const offline = segments.find((item) => item.textContent.startsWith(copy('common.status.offline')));
  assert.equal(offline.hidden, false); offline.focus();
  const rows = [...env.root.querySelector('tbody').children];
  env.send({nodes: env.inventory.map(compact)}, 10);
  assert.equal(online.lastElementChild.textContent, '2'); assert.equal(offline.lastElementChild.textContent, '0');
  assert.equal(offline.hidden, false); assert.equal(env.document.activeElement, offline);
  assert.deepEqual(env.root.querySelector('tbody').children, rows);
  assert.equal(button(env.root, copy('nodes.applyFilters')).parentElement.hidden, false);
  assert.equal(location.search, '?q=Node&status=online&sort=status');
});

for (const page of ['nodes', 'overview']) {
  test(`${page} preserves a new HTTP addition when an older fleet frame arrives during refresh`, async (t) => {
    const env = await setup(t, {page});
    env.send({nodes: [...env.inventory, node(3)].map(compact)});
    env.replaceInventory([node(2), node(3), node(4)]);
    const pending = env.deferInventory();
    button(env.root, copy('nodes.refreshList')).click(); await flush();
    env.send({nodes: [compact(node(2)), compact(node(3))]}, 10);
    env.setServerFleet([node(2), node(3), node(4)]);
    pending.release(); await flush();
    if (page === 'nodes') {
      const addition = env.root.querySelectorAll('input[data-node-id]').find((box) => box.dataset.nodeId === id(4));
      assert.equal(addition.disabled, false);
      assert.doesNotMatch(addition.parentElement.parentElement.parentElement.textContent, copyPattern('controls.deleted'));
    } else {
      const addition = env.root.querySelectorAll('li').find((item) => item.textContent.includes('Node 004'));
      assert.equal(addition.querySelector('a').getAttribute('href'), `/nodes/${id(4)}`);
      assert.doesNotMatch(addition.textContent, copyPattern('controls.deleted'));
    }
  });
}

test('a newly nonzero live presence filter becomes available without an implicit table rebuild', async (t) => {
  const env = await setup(t);
  const offline = env.root.querySelector('.segmented').querySelectorAll('button').find((item) => item.textContent.startsWith(copy('common.status.offline')));
  assert.equal(offline.hidden, true);
  const rows = [...env.root.querySelector('tbody').children];
  env.send({nodes: [compact(node(1)), compact({...node(2), presence: 'offline'})]}, 10);
  assert.equal(offline.hidden, false);
  assert.deepEqual(env.root.querySelector('tbody').children, rows);
  offline.click();
  assert.equal(env.root.querySelectorAll('input[data-node-id]').length, 1);
  assert.match(env.root.querySelector('tbody').textContent, /Node 002/);
  assert.equal(new URLSearchParams(location.search).get('status'), 'offline');
});

for (const page of ['nodes', 'overview']) {
  test(`${page} confirms fresh-only metadata against a causal fleet after concurrent creation and deletion`, async (t) => {
    const env = await setup(t, {page});
    env.send({nodes: [...env.inventory, node(3)].map(compact)});
    env.replaceInventory([node(2), node(3), node(4)]);
    const pending = env.deferInventory();
    button(env.root, copy('nodes.refreshList')).click(); await flush();
    env.send({nodes: [compact(node(2)), compact(node(3))]}, 10);
    pending.release(); await flush();
    if (page === 'nodes') {
      const removed = env.root.querySelectorAll('input[data-node-id]').find((box) => box.dataset.nodeId === id(4));
      assert.equal(removed.disabled, true);
      assert.match(removed.parentElement.parentElement.parentElement.textContent, copyPattern('controls.deleted'));
    } else {
      const removed = env.root.querySelectorAll('li').find((item) => item.textContent.includes('Node 004'));
      assert.equal(removed.querySelector('a').hasAttribute('href'), false);
      assert.match(removed.textContent, copyPattern('controls.deleted'));
    }
  });
}

for (const page of ['nodes', 'overview']) {
  test(`${page} last-seen timestamps remain absolute through quiet heartbeats`, async (t) => {
    const env = await setup(t, {page});
    const times = () => page === 'nodes'
      ? env.root.querySelectorAll('tbody').flatMap((body) => body.querySelectorAll('tr').map((row) => row.children[6].querySelector('time')))
      : env.root.querySelectorAll('section').find((item) => item.querySelector('h2')?.textContent === copy('nodes.newest')).querySelectorAll('time');
    assert.equal(times().length, env.inventory.length);
    const assertObservedTime = (time, iso) => {
      assert.ok(Math.abs(Date.parse(time.textContent) - Date.parse(iso)) < 1000, 'visible last-seen text identifies the observation time');
      assert.equal(time.getAttribute('datetime'), iso);
    };
    for (const time of times()) assertObservedTime(time, reported);
    const seen = '2026-10-28T00:00:05Z';
    env.send({nodes: env.inventory.map((item) => compact({...item, last_seen: seen}))}, 5);
    const before = times().map((time) => time.textContent);
    for (const time of times()) assertObservedTime(time, seen);
    for (let index = 1; index <= 20; index++) {
      await env.advance(10000); env.socket.message(frame('heartbeat', {server_time: new Date(Date.parse(reported) + (5 + index * 10) * 1000).toISOString()}));
    }
    assert.deepEqual(times().map((time) => time.textContent), before);
    assert.equal(times().every((time) => time.getAttribute('datetime') === seen && time.getAttribute('title').includes(seen)), true);
    assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.live')); assert.equal(env.sockets.length, 1);
  });
}

for (const draft of [{hash: '#details', name: 'notes'}, {hash: '#public', name: 'public_name'}]) {
  test(`detail ten typed live updates preserve ${draft.name} draft, dirty state, focus and tab`, async (t) => {
    const env = await setup(t, {page: 'node', hash: draft.hash});
    const form = formFor(env.root, draft.name); const input = edit(form, draft.name, 'Unsaved text must survive'); input.focus(); input.setSelectionRange(2, 9);
    const panel = env.root.querySelector(`#panel-${draft.hash.slice(1)}`);
    assert.equal(sampleFact(env.root, copy('nodes.cpu')), undefined);
    const requestsBefore = env.requests.length;
    for (let index = 1; index <= 10; index++) {
      env.send({node: {...env.inventory[0], presence: index % 2 ? 'offline' : 'online', notes: 'Changed elsewhere', agent_version: `1.${index}`}, latest: latest(index), runtime: {...runtime, report_interval_seconds: 5}}, index * 10);
      assert.equal(formFor(env.root, draft.name), form); assert.equal(input.value, 'Unsaved text must survive'); assert.equal(form.dataset.dirty, 'true');
      assert.equal(env.document.activeElement, input); assert.equal(input.selectionStart, 2); assert.equal(input.selectionEnd, 9);
      assert.equal(location.hash, draft.hash); assert.equal(env.root.querySelector(`#panel-${draft.hash.slice(1)}`), panel);

    }
    assert.equal(env.sockets.length, 1); assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.live'));
    assert.equal(env.requests.length, requestsBefore, 'live samples do not duplicate independent panel reads');

    env.send({node: null, latest: null, runtime: null}, 101);
    assert.equal(env.root.querySelector('.node-deleted-notice').hidden, false); assert.equal(input.value, 'Unsaved text must survive');
    assert.equal(form.dataset.dirty, 'true'); assert.equal(input.readOnly, true);
    assert.equal(form.querySelectorAll('button').every((item) => item.disabled), true);
    assert.equal(env.root.querySelector('#tab-enrollment').disabled, true);
  });
}

test('detail missing sample is not deletion and lazy Agent settings use the newest runtime without another subscription', async (t) => {
  const env = await setup(t, {page: 'node'});
  env.send({node: {...env.inventory[0], status: 'disabled', presence: 'disabled'}, latest: null, runtime: null});
  assert.match(env.root.textContent, copyPattern('nodes.sampleEmpty')); assert.equal(env.root.querySelector('.node-deleted-notice').hidden, true);
  assert.equal(control(env.root, 'notes'), undefined);
  env.send({node: {...env.inventory[0], presence: 'offline'}, latest: latest(3), runtime: {...runtime, report_interval_seconds: 5, interfaces: ['eth1']}});
  env.root.querySelector('#tab-agent').click(); await flush(); env.mutations();
  assert.equal(sampleFact(env.root, copy('nodes.reportInterval')).textContent, copy('nodes.seconds', {count: 5})); assert.equal(sampleFact(env.root, copy('nodes.interfaces')).textContent, 'eth1');
  const settings = env.root.querySelector('#panel-agent');
  env.send({node: env.inventory[0], latest: latest(4), runtime: {...runtime, report_interval_seconds: 15}});
  assert.equal(env.root.querySelector('#panel-agent'), settings); assert.equal(sampleFact(settings, copy('nodes.reportInterval')).textContent, copy('nodes.seconds', {count: 15}));
  env.root.querySelector('#tab-details').click(); await flush(); env.mutations();
  env.send({node: env.inventory[0], latest: latest(5), runtime: {...runtime, report_interval_seconds: 20}});
  env.root.querySelector('#tab-agent').click(); await flush(); env.mutations();
  assert.equal(sampleFact(env.root, copy('nodes.reportInterval')).textContent, copy('nodes.seconds', {count: 20}));
  assert.equal(env.sockets.length, 1);
});

test('detail heartbeat changes only freshness and navigation closes the one live subscription', async (t) => {
  const env = await setup(t, {page: 'node'}); env.send({node: env.inventory[0], latest: latest(), runtime});
  const value = sampleFact(env.root, copy('nodes.cpu')).querySelector('.sample-value');
  const input = env.root.querySelector('#tab-monitor'); input.focus();
  await env.advance(10000); env.socket.message(frame('heartbeat', {server_time: '2026-10-28T00:00:10Z'}));
  await env.advance(10000); env.socket.message(frame('heartbeat', {server_time: '2026-10-28T00:00:20Z'}));
  await env.advance(10000); env.socket.message(frame('heartbeat', {server_time: '2026-10-28T00:00:30Z'}));
  await env.advance(15000); assert.equal(value.classList.contains('metric-stale'), false);
  await env.advance(1); assert.equal(value.classList.contains('metric-stale'), true); assert.equal(value.textContent, '1%');
  env.socket.message(frame('heartbeat', {server_time: '2026-10-28T00:01:00Z'}));
  assert.equal(sampleFact(env.root, copy('nodes.cpu')).querySelector('.sample-value'), value); assert.equal(env.document.activeElement, input);
  env.socket.end(); await flush(); assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.reconnecting')); assert.equal(value.textContent, '1%');
  env.fallback.frame(frame('snapshot', {data: {node: env.inventory[0], latest: latest(), runtime}})); await flush();
  assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.live'));
  env.document.hidden = true; env.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.stale'));
  env.document.hidden = false; env.document.dispatchEvent(new Event('visibilitychange')); await flush();
  assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.reconnecting'));
  env.scope.abort(); assert.equal(env.socket.closed, true);
});

test('overview patches only fleet and existing newest readonly fields until explicit inventory refresh', async (t) => {
  const env = await setup(t, {page: 'overview'});
  const section = (title) => env.root.querySelectorAll('section').find((item) => item.querySelector('h2')?.textContent === title);
  const activity = section(copy('nodes.recentActivity')); const newest = section(copy('nodes.newest')); const rows = [...newest.querySelector('ul').children];
  const groups = section(copy('nodes.groups'));
  env.send({nodes: [compact({...env.inventory[0], presence: 'offline'}), compact(env.inventory[1])]});
  assert.equal(section(copy('nodes.recentActivity')), activity); assert.equal(section(copy('nodes.newest')), newest); assert.deepEqual(newest.querySelector('ul').children, rows);
  assert.match(rows[0].textContent, copyPattern('common.status.offline')); assert.match(section(copy('nodes.fleet')).textContent, copyPattern('common.status.offline'));
  env.send({nodes: [compact(env.inventory[1]), compact(node(3))]});
  assert.match(env.root.textContent, new RegExp(`${copyPattern('nodes.inventoryChanged').source}${copyPattern('nodes.refreshList').source}`)); assert.equal(section(copy('nodes.recentActivity')), activity);
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, false);
  env.replaceInventory([node(2), node(3)]); button(env.root, copy('nodes.refreshList')).click(); await flush();
  assert.equal(section(copy('nodes.recentActivity')), activity); assert.equal(section(copy('nodes.groups')), groups);
  assert.equal(section(copy('nodes.newest')), newest); assert.match(newest.textContent, /Node 003/);
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, true);
  assert.equal(env.sockets.filter((socket) => !socket.closed).length, 1);
});

test('overview keeps the group name when live frames omit it', async (t) => {
  const env = await setup(t, {page: 'overview'});
  env.send({nodes: [compact(env.inventory[0]), compact(env.inventory[1]), compact(node(3))]});
  const grouped = {...node(3), group_id: id(700)};
  env.replaceInventory([node(1), node(2), grouped]); button(env.root, copy('nodes.refreshList')).click(); await flush();
  env.send({nodes: [compact(node(1)), compact(node(2)), compact(grouped)]}, 10);
  const newest = env.root.querySelectorAll('section').find((item) => item.querySelector('h2')?.textContent === copy('nodes.newest'));
  const row = newest.querySelectorAll('li').find((item) => item.textContent.includes('Node 003'));
  assert.match(row.textContent, /Production/);
});

test('overview deferred refresh keeps newer fleet status, deleted links and unknown-inventory notice', async (t) => {
  const env = await setup(t, {page: 'overview'});
  const section = (title) => env.root.querySelectorAll('section').find((item) => item.querySelector('h2')?.textContent === title);
  const activity = section(copy('nodes.recentActivity')); const groups = section(copy('nodes.groups')); const newest = section(copy('nodes.newest'));
  env.send({nodes: [...env.inventory, node(3)].map(compact)});
  const pending = env.deferInventory(); button(env.root, copy('nodes.refreshList')).click(); await flush();
  assert.equal(typeof pending.release, 'function');
  const survivor = {...node(2), presence: 'offline', last_seen: '2026-10-28T00:00:10Z'};
  env.send({nodes: [compact(survivor), compact(node(4))]}, 10);
  pending.release(); await flush();
  const removed = newest.querySelectorAll('li').find((item) => item.textContent.includes('Node 001'));
  assert.match(removed.textContent, copyPattern('controls.deleted')); assert.equal(removed.querySelector('a').hasAttribute('href'), false);
  assert.match(newest.querySelectorAll('li').find((item) => item.textContent.includes('Node 002')).textContent, copyPattern('common.status.offline'));
  assert.equal(section(copy('nodes.fleet')).querySelector('.num').textContent, '2');
  assert.equal(section(copy('nodes.fleet')).querySelector('.fleet-bar').getAttribute('aria-label'), new Intl.ListFormat('en').format([`1 ${copy('common.status.online').toLowerCase()}`, `1 ${copy('common.status.offline').toLowerCase()}`]));
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, false);
  assert.equal(section(copy('nodes.recentActivity')), activity); assert.equal(section(copy('nodes.groups')), groups);
  env.replaceInventory([survivor, node(4)]); button(env.root, copy('nodes.refreshList')).click(); await flush();
  assert.equal(section(copy('nodes.newest')), newest); assert.match(newest.textContent, /Node 004/); assert.doesNotMatch(newest.textContent, /Node 001/);
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, true);
});

for (const draft of [{name: 'notes', value: 'Pending metadata draft', hash: '#details', result: node(1)},
  {name: 'public_name', value: 'Pending public draft', hash: '#public', result: {enabled: true, name: 'Saved'}},
  {name: 'billing_day', value: '2', hash: '#traffic', result: {billing_day: 2, quota_bytes: '0'}}]) {
  test(`detail deletion fences pending ${draft.name} save continuation and keeps its dirty draft`, async (t) => {
    let release;
    const env = await setup(t, {page: 'node', hash: draft.hash, mutate: () => new Promise((resolve) => { release = resolve; })});
    const form = formFor(env.root, draft.name); const input = edit(form, draft.name, draft.value); input.focus();
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush(); assert.equal(typeof release, 'function');
    const before = env.requests.length;
    env.send({node: null, latest: null, runtime: null});
    release(response(draft.result)); await flush();
    assert.equal(formFor(env.root, draft.name), form); assert.equal(input.value, draft.value); assert.equal(form.dataset.dirty, 'true');
    assert.equal(env.document.activeElement, input); assert.equal(input.readOnly, true);
    assert.equal(form.querySelectorAll('button').every((item) => item.disabled), true);
    assert.equal(env.requests.length, before, 'late save does not refresh the deleted detail or its independent panels');
  });
}

test('detail deletion fences an already open lifecycle confirmation without changing frozen list batch behavior', async (t) => {
  const env = await setup(t, {page: 'node'});
  button(env.root, copy('nodes.disable')).click(); await flush();
  const dialog = env.body.querySelector('dialog');
  env.send({node: null, latest: null, runtime: null});
  button(dialog, copy('nodes.disableNode')).click(); dialog.dispatchEvent(new Event('animationend')); await flush();
  assert.equal(env.requests.some((request) => request.url === '/api/v1/nodes/batch'), false);
  assert.equal(button(env.root, copy('nodes.disable')).disabled, true);
});

test('detail deletion fences an already open enrollment revocation confirmation', async (t) => {
  const env = await setup(t, {page: 'node', hash: '#enrollment'}); await flush();
  button(env.root, copy('nodes.enrollment.revoke')).click(); await flush();
  const dialog = env.body.querySelector('dialog'); env.send({node: null, latest: null, runtime: null});
  button(dialog, copy('nodes.enrollment.revokeConfirm')).click(); dialog.dispatchEvent(new Event('animationend')); await flush();
  assert.equal(env.requests.some((request) => request.method === 'DELETE'), false);
  assert.equal(button(env.root, copy('nodes.enrollment.issue')).disabled, true);
});

test('detail live capability arrays reject unknown, repeated, missing and null values', async (t) => {
  for (const [field, values] of [
    ['remote_commands', ['shell']], ['remote_commands', ['restart', 'restart']],
    ['disabled_features', ['cpu']], ['disabled_features', ['disk', 'disk']],
    ['remote_commands', null], ['disabled_features', null],
    ['remote_commands', undefined], ['disabled_features', undefined]
  ]) {
    await t.test(`${field}=${JSON.stringify(values)}`, async (t) => {
      const env = await setup(t, {page: 'node', hash: '#agent'});
      env.send({node: env.inventory[0], latest: latest(), runtime});
      env.send({node: env.inventory[0], latest: latest(), runtime: {...runtime, [field]: values, report_interval_seconds: 5}});
      assert.equal(sampleFact(env.root, copy('nodes.reportInterval')).textContent, copy('nodes.seconds', {count: 10}));
    });
  }
});

test('detail live accepts empty capability arrays as reported', async (t) => {
  const env = await setup(t, {page: 'node', hash: '#agent'});
  env.send({node: env.inventory[0], latest: latest(), runtime: {...runtime, remote_commands: [], disabled_features: []}});
  assert.equal(sampleFact(env.root, copy('nodes.remoteCommands')).textContent, copy('nodes.none'));
  assert.equal(sampleFact(env.root, copy('nodes.disabledFeatures')).textContent, copy('nodes.none'));
});

for (const page of ['nodes', 'overview', 'node']) test(`${page} uses Stopped badge and separates stopped from offline`, async (t) => {
  const env = await setup(t, {page, search: page === 'nodes' ? '?sort=status' : ''});
  const stopped = {...env.inventory[0], stopped: true, presence: 'offline'};
  const offline = {...env.inventory[1], presence: 'offline'};
  env.send(page === 'node' ? {node: stopped, latest: null, runtime} : {nodes: [compact(stopped), compact(offline)]});
  const badges = env.root.querySelectorAll('.badge');
  const stoppedBadge = badges.find((badge) => badge.textContent === copy('common.status.stopped'));
  assert.ok(stoppedBadge); assert.ok(stoppedBadge.classList.contains('disabled'));
  if (page === 'nodes') {
    const filters = env.root.querySelector('.segmented').querySelectorAll('button');
    const stoppedFilter = filters.find((item) => item.textContent.startsWith(copy('common.status.stopped')));
    const offlineFilter = filters.find((item) => item.textContent.startsWith(copy('common.status.offline')));
    assert.equal(stoppedFilter.lastElementChild.textContent, '1');
    assert.equal(offlineFilter.lastElementChild.textContent, '1');
    stoppedFilter.click();
    assert.equal(env.root.querySelector('tbody').querySelectorAll('tr').length, 1);
    assert.match(env.root.querySelector('tbody').textContent, copyPattern('common.status.stopped'));
    assert.doesNotMatch(env.root.querySelector('tbody').textContent, copyPattern('common.status.offline'));
  } else if (page === 'overview') {
    const links = env.root.querySelector('.legend').querySelectorAll('a');
    assert.equal(links.find((link) => link.getAttribute('href') === '/nodes?status=stopped').querySelector('strong').textContent, '1');
    assert.equal(links.find((link) => link.getAttribute('href') === '/nodes?status=offline').querySelector('strong').textContent, '1');
  } else {
    assert.equal(env.root.querySelector('header').querySelector('.badge').textContent, copy('common.status.stopped'));
  }
});

for (const page of ['nodes', 'node']) test(`${page} rejects missing and nonboolean stopped live fields`, async (t) => {
  const env = await setup(t, {page});
  const send = (value) => env.send(page === 'node' ? {node: value, latest: null, runtime} : {nodes: [compact(value)]});
  send({...env.inventory[0], stopped: true, presence: 'offline'});
  for (const stopped of [undefined, null, 'false', 0]) {
    send({...env.inventory[0], stopped, presence: 'online'});
    assert.equal(env.root.querySelector('.badge').textContent, copy('common.status.stopped'));
  }
});

test('node list sorts Agent versions and patches the existing version cells', async (t) => {
  const env = await setup(t, {search: '?sort=agent_version&dir=desc'});
  const header = env.root.querySelectorAll('th').find((item) => item.dataset.sort === 'agent_version');
  assert.ok(header.classList.contains('hide-compact'));
  const added = env.root.querySelectorAll('th').find((item) => item.dataset.sort === 'created');
  assert.ok(added.classList.contains('hide-compact'));
  assert.ok(env.root.querySelector('tbody').querySelectorAll('tr').every((row) => row.lastElementChild.classList.contains('hide-compact')));
  assert.equal(header.getAttribute('aria-sort'), 'descending');
  env.send({nodes: [compact({...env.inventory[0], agent_version: '1.9'}), compact({...env.inventory[1], agent_version: '1.10'})]});
  const versionCells = env.root.querySelector('tbody').querySelectorAll('tr').map((row) => row.children[5]);
  assert.ok(versionCells.every((cell) => cell.classList.contains('hide-compact')));
  assert.deepEqual(versionCells.map((cell) => cell.textContent), ['1.9', '1.10']);
  header.querySelector('button').click();
  assert.equal(header.getAttribute('aria-sort'), 'ascending');
  header.querySelector('button').click();
  assert.equal(env.root.querySelector('tbody').querySelector('tr').children[5].textContent, '1.10');
});

test('Agent command polling detaches on tab changes and clears on navigation', async (t) => {
  const env = await setup(t, {page: 'node', hash: '#agent', commands: [{id: id(800), kind: 'restart', state: 'pending', result_code: '', requested_by: 'Alice', created_at: reported}]});
  const commands = () => env.requests.filter((item) => item.url.endsWith('/commands')).length;
  const before = commands();
  [...env.intervals.values()][0](); await flush();
  assert.equal(commands(), before + 1);
  button(env.root, copy('nodes.details')).click(); env.mutations(); await flush();
  env.advance(6000); await flush(); assert.equal(commands(), before + 1);
  button(env.root, copy('nodes.agentSettings')).click(); env.mutations(); await flush();
  const resumed = commands(); assert.equal(resumed, before + 2);
  env.scope.abort(); env.advance(6000); await flush(); assert.equal(commands(), resumed);
});

test('node tabs lazily build each panel, retain latest snapshots, and resume only their own polling', async (t) => {
  const env = await setup(t, {page: 'node', hash: '#enrollment'});
  const reads = (part) => env.requests.filter((request) => request.url.includes(part));
  const switchTo = async (id) => { env.root.querySelector(`#tab-${id}`).click(); env.mutations(); await flush(); env.mutations(); };
  assert.equal(sampleFact(env.root, copy('nodes.cpu')), undefined);
  assert.equal(control(env.root, 'name'), undefined);
  assert.equal(reads('/history?').length, 0); assert.equal(reads('/availability?').length, 0);
  assert.equal(reads('/traffic?').length, 0); assert.equal(reads('/probes').length, 0);
  env.send({node: {...env.inventory[0], name: 'Renamed before creation', presence: 'offline'}, latest: latest(7), runtime}, 10);
  assert.equal(env.root.querySelector('h1').textContent, 'Renamed before creation');
  await switchTo('details');
  assert.equal(control(env.root, 'name').value, 'Renamed before creation');
  assert.equal(sampleFact(env.root, copy('nodes.presence')).textContent, copy('common.status.offline'));
  assert.equal(reads('/history?').length, 0);
  env.send({node: {...env.inventory[0], name: 'Renamed again', presence: 'online'}, latest: latest(8), runtime}, 20);
  assert.equal(control(env.root, 'name').value, 'Renamed before creation');
  assert.equal(sampleFact(env.root, copy('nodes.presence')).textContent, copy('common.status.online'));
  await switchTo('monitor');
  const sample = sampleFact(env.root, copy('nodes.cpu')).querySelector('.sample-value');
  assert.equal(sample.textContent, '8%');
  assert.ok(reads('/history?').length > 0); assert.equal(reads('/availability?').length, 1);
  assert.equal(reads('/traffic?').length, 0); assert.equal(reads('/probes').length, 0);
  assert.equal(env.intervals.size, 1);
  await switchTo('details'); assert.equal(env.intervals.size, 0);
  assert.equal(control(env.root, 'name').value, 'Renamed again');
  env.send({node: {...env.inventory[0], name: 'Hidden monitor update'}, latest: latest(9), runtime}, 30);
  await env.advance(46000);
  assert.equal(sample.classList.contains('metric-stale'), false, 'detached sample freshness timer stays paused');
  await switchTo('monitor');
  assert.equal(sampleFact(env.root, copy('nodes.cpu')).querySelector('.sample-value'), sample);
  assert.equal(sample.textContent, '9%'); assert.equal(sample.classList.contains('metric-stale'), true);
  assert.equal(reads('/availability?').length, 1); assert.equal(env.intervals.size, 1);
  await switchTo('traffic'); assert.equal(env.intervals.size, 0);
  assert.equal(reads('/traffic?').length, 1); assert.equal(reads('/probes').length, 0);
  assert.ok(control(env.root, 'billing_day')); assert.equal(sampleFact(env.root, copy('nodes.cpu')), undefined);
  await switchTo('probes'); assert.equal(env.intervals.size, 1); assert.equal(reads('/probes').length, 1);
  await switchTo('details'); assert.equal(env.intervals.size, 0);
  await switchTo('traffic'); assert.equal(reads('/traffic?').length, 1, 'traffic retains its original explicit-reload lifecycle');
  await switchTo('probes'); assert.equal(reads('/probes').length, 2);
  env.scope.abort(); assert.equal(env.intervals.size, 0);
});

test('deletion before lazy tab creation keeps all future controls locked and monitor empty', async (t) => {
  const env = await setup(t, {page: 'node', hash: '#public'});
  env.send({node: null, latest: null, runtime: null});
  assert.equal(env.root.querySelector('.node-deleted-notice').hidden, false);
  assert.equal(env.root.querySelector('#tab-enrollment').disabled, true);
  for (const tab of ['details', 'traffic', 'agent', 'monitor', 'probes']) {
    env.root.querySelector(`#tab-${tab}`).click(); env.mutations(); await flush(); env.mutations();
    for (const control of env.root.querySelectorAll('form').flatMap((form) => form.querySelectorAll('input,select,textarea,button'))) {
      assert.ok(control.readOnly || control.disabled, `${tab} deleted control remains locked`);
    }
    if (tab === 'details') assert.equal(sampleFact(env.root, copy('nodes.presence')).textContent, copy('controls.deleted'));
    if (tab === 'monitor') assert.ok(env.root.textContent.includes(copy('nodes.sampleEmpty')));
  }
  env.send({node: env.inventory[0], latest: latest(20), runtime});
  env.root.querySelector('#tab-monitor').click(); env.mutations(); await flush();
  assert.equal(sampleFact(env.root, copy('nodes.cpu')), undefined);
  assert.ok(env.root.textContent.includes(copy('nodes.sampleEmpty')));
});

test('node list live metrics preserve rows and selection without inventory reload or apply-filters notice', async (t) => {
  const env = await setup(t, {search: '?sort=status&status=online'});
  const rows = [...env.root.querySelector('tbody').children];
  const headers = env.root.querySelectorAll('th');
  assert.equal(headers[3].textContent, copy('nodes.cpu'));
  assert.equal(headers[4].textContent, copy('nodes.memory'));
  assert.equal(rows[0].children[3].textContent, '—');
  const box = env.root.querySelector('input[data-node-id]');
  box.checked = true; box.dispatchEvent(new Event('change'));
  const requests = env.requests.length;
  const query = location.search;
  for (const value of [0, 42, 100, null]) {
    env.send({nodes: env.inventory.map((item) => ({...compact(item), cpu_percent: value, memory_used_percent: value}))});
    await flush();
    assert.deepEqual(env.root.querySelector('tbody').children, rows);
    assert.equal(rows[0].children[3].textContent, value === null ? '—' : `${value}%`);
    assert.equal(rows[0].children[4].textContent, value === null ? '—' : `${value}%`);
    assert.equal(box.checked, true);
    assert.equal(location.search, query);
    assert.equal(env.requests.length, requests);
    assert.equal(button(env.root, copy('nodes.applyFilters')).parentElement.hidden, true);
  }
});
