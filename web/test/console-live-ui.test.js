import {formatTimestamp, formatFullTimestamp} from '../src/shared/format.js';
import test from 'node:test';
import assert from 'node:assert/strict';
import {reducedMotion} from '../src/shared/dom.js';
import {metricText} from '../src/console/lib/nodes-table.js';
import {invalidate, setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, copyPattern, elements, flush} from './console-dom.js';
import {frame, liveHarness, response, stream} from './live-harness.js';

const id = (number) => `00000000-0000-4000-8000-${String(number).padStart(12, '0')}`;
const reported = '2026-10-28T00:00:00Z';
const node = (number) => ({id: id(number), name: `Node ${String(number).padStart(3, '0')}`, status: 'unknown', stopped: false, presence: 'online',
  last_seen: reported, agent_version: '1.0', group_id: null, region: '', provider: '', notes: 'Saved notes', tags: [], created_at: reported});
const latest = (seq = 1) => ({received_at: reported, sampled_at: reported, seq,
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
    if (path.endsWith('/traffic-settings')) return response({billing_day: 1, quota_bytes: '0'});
    if (path.endsWith('/public-settings')) return response({enabled: true, name: 'Saved public name', region: '', provider: ''});
    if (path.endsWith('/latest')) return sample ? response({...sample, server_time: reported}) : response({message: 'No sample'}, {status: 404});
    if (path.endsWith('/traffic')) return response({billing_day: 1, quota_bytes: '0', cycle: {}, daily: [], updated_at: null});
    if (path.endsWith('/probes')) return response({probes: []});
    return response({message: `Unexpected request ${request.url}`}, {status: 503});
  });
  const frames = new Map();
  let frameID = 0;
  globalThis.requestAnimationFrame = (callback) => { frames.set(++frameID, callback); return frameID; };
  globalThis.cancelAnimationFrame = (id) => frames.delete(id);
  const renderFrame = (time = performance.now()) => { const callbacks = [...frames.values()]; frames.clear(); callbacks.forEach((callback) => callback(time)); };
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
  return {...env, root, scope, socket, send, fallback, inventory, frames, renderFrame,
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
  assert.equal(location.search, '?q=Node');
  await env.advance(10000); env.socket.message(frame('heartbeat', {server_time: '2026-10-28T00:00:20Z'}));
  assert.equal(deletedBox.disabled, true, 'a quiet stream cannot leave a resurrected actionable row');
  env.replaceInventory([survivor, {...node(4), presence: 'offline'}]); button(env.root, copy('nodes.refreshList')).click(); await flush();
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
  assert.equal(location.search, '?q=Node&status=online');
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
      env.send({nodes: [compact(node(2)), compact(node(3)), compact({...node(4), presence: 'offline'})]}, 20);
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

for (const page of ['nodes']) {
  test(`${page} last-seen timestamps remain absolute through quiet heartbeats`, async (t) => {
    const env = await setup(t, {page});
    const times = () => page === 'nodes'
      ? env.root.querySelectorAll('tbody').flatMap((body) => body.querySelectorAll('tr').map((row) => row.children[6].querySelector('time')))
      : [];
    assert.equal(times().length, env.inventory.length);
    const assertObservedTime = (time, iso) => {
      assert.equal(time.textContent, formatTimestamp(new Date(iso)));
      assert.equal(time.getAttribute('title'), `${formatFullTimestamp(new Date(iso))} (UTC ${new Date(iso).toISOString()})`);
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
    assert.equal(times().every((time) => time.getAttribute('datetime') === seen && time.getAttribute('title').includes(new Date(seen).toISOString())), true);
    assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.live')); assert.equal(env.sockets.length, 1);
  });
}

for (const draft of [{hash: '#details', name: 'notes'}, {hash: '#public', name: 'public_name'}]) {
  test(`detail ten typed live updates preserve ${draft.name} draft, dirty state, focus and tab`, async (t) => {
    const env = await setup(t, {page: 'node', hash: draft.hash});
    const form = formFor(env.root, draft.name); const input = edit(form, draft.name, 'Unsaved text must survive'); input.focus(); input.setSelectionRange(2, 9);
    const panel = env.root.querySelector('#panel-settings');
    assert.equal(sampleFact(env.root, copy('nodes.cpu')), undefined);
    const requestsBefore = env.requests.length;
    for (let index = 1; index <= 10; index++) {
      env.send({node: {...env.inventory[0], presence: index % 2 ? 'offline' : 'online', notes: 'Changed elsewhere', agent_version: `1.${index}`}, latest: latest(index)}, index * 10);
      assert.equal(formFor(env.root, draft.name), form); assert.equal(input.value, 'Unsaved text must survive'); assert.equal(form.dataset.dirty, 'true');
      assert.equal(env.document.activeElement, input); assert.equal(input.selectionStart, 2); assert.equal(input.selectionEnd, 9);
      assert.equal(location.hash, draft.hash); assert.equal(env.root.querySelector('#panel-settings'), panel);

    }
    assert.equal(env.sockets.length, 1); assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.live'));
    assert.equal(env.requests.length, requestsBefore, 'live samples do not duplicate independent panel reads');

    env.send({node: null, latest: null}, 101);
    assert.equal(env.root.querySelector('.node-deleted-notice').hidden, false); assert.equal(input.value, 'Unsaved text must survive');
    assert.equal(form.dataset.dirty, 'true'); assert.equal(input.readOnly, true);
    assert.equal(form.querySelectorAll('button').every((item) => item.disabled), true);
    assert.equal(button(env.root, copy('nodes.enrollAgent')).disabled, true);
  });
}

test('detail missing sample is not deletion and lazy Agent section needs no runtime subscription', async (t) => {
  const env = await setup(t, {page: 'node'});
  env.send({node: {...env.inventory[0], status: 'disabled', presence: 'disabled'}, latest: null});
  assert.match(env.root.textContent, copyPattern('nodes.sampleEmpty'));
  assert.equal(env.root.querySelector('.node-deleted-notice').hidden, true);
  env.root.querySelector('#tab-settings').click(); await flush(); env.mutations();
  assert.ok(env.root.querySelector('#node-agent'));
  assert.ok(env.root.querySelector('#node-enrollment'));
  assert.equal(env.sockets.length, 1);
});

test('detail heartbeat changes only freshness and navigation closes the one live subscription', async (t) => {
  const env = await setup(t, {page: 'node'}); env.send({node: env.inventory[0], latest: latest()});
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
  env.fallback.frame(frame('snapshot', {data: {node: env.inventory[0], latest: latest()}})); await flush();
  assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.live'));
  env.document.hidden = true; env.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.stale'));
  env.document.hidden = false; env.document.dispatchEvent(new Event('visibilitychange')); await flush();
  assert.equal(env.root.querySelector('.live-status').textContent, copy('controls.live.reconnecting'));
  env.scope.abort(); assert.equal(env.socket.closed, true);
});

test('overview updates attention membership while preserving existing rows, activity and the live subscription', async (t) => {
  const env = await setup(t, {page: 'overview'});
  const section = (title) => env.root.querySelectorAll('section').find((item) => item.querySelector('h2')?.textContent === title);
  const activity = section(copy('nodes.recentActivity')); const attention = section(copy('nodes.needsAttention'));
  assert.equal(attention.hidden, true);
  const statusLines = section(copy('nodes.fleet')).querySelectorAll('.overview-status-line');
  assert.deepEqual(statusLines.map((line) => line.textContent), [copy('nodes.noAlerts'), copy('nodes.allHealthy')]);
  assert.ok(attention.parentElement.classList.contains('single'));
  assert.equal(section(copy('alerts.events.activeTitle')), undefined);
  const offline = compact({...env.inventory[0], presence: 'offline'});
  env.send({nodes: [offline, compact(env.inventory[1])]});
  const row = attention.querySelector('li');
  assert.equal(attention.hidden, false);
  assert.equal(attention.parentElement.classList.contains('single'), false);
  assert.equal(section(copy('nodes.recentActivity')), activity);
  env.send({nodes: [offline, compact(env.inventory[1])]}, 10);
  assert.equal(attention.querySelector('li'), row);
  env.send({nodes: [compact(env.inventory[1]), compact(node(3))]}, 20);
  assert.equal(row.querySelector('a').hasAttribute('href'), false);
  assert.match(row.textContent, copyPattern('controls.deleted'));
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, false);
  env.replaceInventory([node(2), {...node(3), presence: 'offline'}]); button(env.root, copy('nodes.refreshList')).click(); await flush();
  assert.equal(section(copy('nodes.needsAttention')), attention); assert.match(attention.textContent, /Node 003/);
  assert.equal(button(env.root, copy('nodes.refreshList')).parentElement.hidden, true);
  assert.equal(env.sockets.filter((socket) => !socket.closed).length, 1);
});

test('overview live group segments retain metadata when fleet frames omit group IDs', async (t) => {
  const env = await setup(t, {page: 'overview'});
  const grouped = {...node(3), group_id: id(700)};
  env.replaceInventory([node(1), node(2), grouped]);
  env.send({nodes: [compact(node(1)), compact(node(2)), compact(grouped)]});
  button(env.root, copy('nodes.refreshList')).click(); await flush();
  env.send({nodes: [compact(node(1)), compact(node(2)), compact({...grouped, presence: 'offline'})]}, 10);
  const groups = env.root.querySelectorAll('section').find((item) => item.querySelector('h2')?.textContent === copy('nodes.groups'));
  const row = groups.querySelectorAll('.meter-row').find((row) => row.textContent.includes('Production'));
  assert.ok(row.querySelector('.c-offline'));
  assert.equal(row.querySelector('.c-online'), null);
});

test('overview deferred refresh keeps newer fleet status, deleted links and unknown-inventory notice', async (t) => {
  const env = await setup(t, {page: 'overview'});
  const section = (title) => env.root.querySelectorAll('section').find((item) => item.querySelector('h2')?.textContent === title);
  const activity = section(copy('nodes.recentActivity')); const groups = section(copy('nodes.groups')); const newest = section(copy('nodes.needsAttention'));
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
  env.replaceInventory([survivor, {...node(4), presence: 'offline'}]); button(env.root, copy('nodes.refreshList')).click(); await flush();
  assert.equal(section(copy('nodes.needsAttention')), newest); assert.match(newest.textContent, /Node 004/); assert.doesNotMatch(newest.textContent, /Node 001/);
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
    env.send({node: null, latest: null});
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
  env.send({node: null, latest: null});
  button(dialog, copy('nodes.disableNode')).click(); dialog.dispatchEvent(new Event('animationend')); await flush();
  assert.equal(env.requests.some((request) => request.url === '/api/v1/nodes/batch'), false);
  assert.equal(button(env.root, copy('nodes.disable')).disabled, true);
});

test('detail deletion fences an already open enrollment revocation confirmation', async (t) => {
  const env = await setup(t, {page: 'node', hash: '#enrollment'}); await flush();
  button(env.root, copy('nodes.enrollment.revoke')).click(); await flush();
  const dialog = env.body.querySelector('dialog'); env.send({node: null, latest: null});
  button(dialog, copy('nodes.enrollment.revokeConfirm')).click(); dialog.dispatchEvent(new Event('animationend')); await flush();
  assert.equal(env.requests.some((request) => request.method === 'DELETE'), false);
  assert.equal(button(env.root, copy('nodes.enrollAgent')).disabled, true);
});

test('detail deletion suppresses a delayed deployment secret and sends no mode field', async (t) => {
  let release;
  const env = await setup(t, {page: 'node', hash: '#enrollment', mutate: (request) => {
    assert.equal(request.url, '/api/v1/enrollments');
    assert.deepEqual(JSON.parse(request.body), {node_id: id(1)});
    return new Promise((resolve) => { release = () => resolve(response({token: 'late-fake-secret', server_version: '1.2.3', expires_at: '2026-10-28T00:30:00Z'})); });
  }});
  const issue = button(env.root, copy('nodes.enrollAgent')); issue.click(); await flush();
  assert.equal(typeof release, 'function');
  env.send({node: null, latest: null}); release(); await flush();
  assert.equal(env.body.querySelector('dialog'), null);
  assert.equal(env.body.textContent.includes('late-fake-secret'), false);
  assert.equal(issue.disabled, true);
});

for (const page of ['nodes', 'overview', 'node']) test(`${page} uses Stopped badge and separates stopped from offline`, async (t) => {
  const env = await setup(t, {page, search: page === 'nodes' ? '?sort=status' : ''});
  const stopped = {...env.inventory[0], stopped: true, presence: 'offline'};
  const offline = {...env.inventory[1], presence: 'offline'};
  env.send(page === 'node' ? {node: stopped, latest: null} : {nodes: [compact(stopped), compact(offline)]});
  const badges = env.root.querySelectorAll('.badge');
  const stoppedBadge = badges.find((badge) => badge.textContent === copy('common.status.stopped'));
  if (page !== 'overview') { assert.ok(stoppedBadge); assert.ok(stoppedBadge.classList.contains('disabled')); }
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
  const send = (value) => env.send(page === 'node' ? {node: value, latest: null} : {nodes: [compact(value)]});
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
  env.root.querySelector('#tab-monitor').click(); env.mutations(); await flush();
  env.advance(6000); await flush(); assert.equal(commands(), before + 1);
  env.root.querySelector('#tab-settings').click(); env.mutations(); await flush();
  const resumed = commands(); assert.equal(resumed, before + 2);
  env.scope.abort(); env.advance(6000); await flush(); assert.equal(commands(), resumed);
});

test('four node tabs lazily build settings with newest snapshots and detach command polling', async (t) => {
  const env = await setup(t, {page: 'node', hash: '#probes'});
  const reads = (part) => env.requests.filter((request) => request.url.includes(part));
  const switchTo = async (id) => { env.root.querySelector(`#tab-${id}`).click(); env.mutations(); await flush(); env.mutations(); };
  assert.equal(control(env.root, 'name'), undefined);
  assert.equal(reads('/history?').length, 0); assert.equal(reads('/availability?').length, 0);
  assert.equal(reads('/commands').length, 0); assert.equal(reads('/enrollments').length, 0);
  env.send({node: {...env.inventory[0], name: 'Renamed before creation'}, latest: latest(7)}, 10);
  await switchTo('settings');
  assert.equal(control(env.root, 'name').value, 'Renamed before creation');
  assert.equal(reads('/enrollments').length, 1);
  env.send({node: {...env.inventory[0], name: 'Renamed again'}, latest: latest(8)}, 20);
  assert.equal(control(env.root, 'name').value, 'Renamed before creation');
  await switchTo('monitor');
  const sample = sampleFact(env.root, copy('nodes.cpu')).querySelector('.sample-value');
  assert.equal(sample.textContent, '8%');
  assert.ok(reads('/history?').length > 0); assert.equal(reads('/availability?').length, 1);
  const headings = env.root.querySelector('#panel-monitor').querySelectorAll('h2').map((heading) => heading.textContent);
  assert.equal(headings[0], copy('nodes.sample'));
  assert.ok(headings[1].includes(copy('public.availability')));
  assert.equal(env.root.querySelector('.revision'), null);
  const sampled = sampleFact(env.root, copy('nodes.sampled')).querySelector('time');
  assert.ok(sampled.title.includes(copy('nodes.sequence', {value: 8})));
  await switchTo('settings');
  assert.equal(control(env.root, 'name').value, 'Renamed again');
  env.send({node: {...env.inventory[0], name: 'Hidden monitor update'}, latest: latest(9)}, 30);
  await switchTo('monitor');
  assert.equal(sampleFact(env.root, copy('nodes.cpu')).querySelector('.sample-value'), sample);
  assert.equal(sample.textContent, '9%');
  assert.equal(reads('/availability?').length, 1);
  await switchTo('traffic'); assert.equal(reads('/traffic?').length, 1);
  await switchTo('probes');
  await switchTo('traffic'); assert.equal(reads('/traffic?').length, 1);
  env.scope.abort(); assert.equal(env.intervals.size, 0);
});

test('deletion before lazy tab creation keeps all future controls locked and monitor empty', async (t) => {
  const env = await setup(t, {page: 'node', hash: '#public'});
  env.send({node: null, latest: null});
  assert.equal(env.root.querySelector('.node-deleted-notice').hidden, false);
  assert.equal(button(env.root, copy('nodes.enrollAgent')).disabled, true);
  for (const tab of ['settings', 'traffic', 'monitor', 'probes']) {
    env.root.querySelector(`#tab-${tab}`).click(); env.mutations(); await flush(); env.mutations();
    for (const control of env.root.querySelectorAll('form').flatMap((form) => form.querySelectorAll('input,select,textarea,button'))) {
      assert.ok(control.readOnly || control.disabled, `${tab} deleted control remains locked`);
    }
    if (tab === 'monitor') assert.ok(env.root.textContent.includes(copy('nodes.sampleEmpty')));
  }
  env.send({node: env.inventory[0], latest: latest(20)});
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
    env.renderFrame(performance.now() + 250);
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


test('overview total retargets its displayed count and navigation abort clears its frame', async (t) => {
  const descriptor = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => false});
  t.after(() => Object.defineProperty(reducedMotion, 'matches', descriptor));
  const env = await setup(t, {page: 'overview'});
  const groupFills = env.root.querySelectorAll('.meter-row').map((row) => row.querySelector('i'));
  assert.ok(groupFills.length > 0);
  assert.ok(groupFills.every((fill) => !fill.hasAttribute('style') && Number(fill.style.flex) > 0));
  const total = env.root.querySelector('.fleet-total').querySelector('strong');
  assert.equal(total.textContent, '2');
  env.send({nodes: [1, 2, 3, 4, 5, 6].map((id) => compact(node(id)))});
  assert.equal(total.textContent, '2');
  env.renderFrame(performance.now() + 125);
  assert.equal(total.textContent, '4');
  env.send({nodes: env.inventory.map(compact)});
  assert.equal(total.textContent, '4');
  env.renderFrame(performance.now() + 250);
  assert.equal(total.textContent, '2');
  env.send({nodes: [compact(node(1))]});
  assert.equal(env.frames.size, 1);
  env.scope.abort();
  assert.equal(env.frames.size, 0);
  assert.equal(total.textContent, '1');
});

test('node metric tweens start from the original numeric cells and stop on tombstoning', async (t) => {
  const descriptor = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => false});
  t.after(() => Object.defineProperty(reducedMotion, 'matches', descriptor));
  const env = await setup(t);
  const row = env.root.querySelector('tbody').children[0];
  const cpu = row.children[3]; const memory = row.children[4];
  env.send({nodes: env.inventory.map((item) => ({...compact(item), cpu_percent: 10, memory_used_percent: 20}))});
  env.send({nodes: env.inventory.map((item) => ({...compact(item), cpu_percent: 30, memory_used_percent: 80}))});
  assert.equal(cpu.textContent, metricText(10));
  env.renderFrame(performance.now() + 125);
  assert.equal(cpu.textContent, metricText(20));
  assert.equal(memory.textContent, metricText(50));
  env.send({nodes: []});
  assert.equal(cpu.textContent, metricText(30));
  assert.equal(memory.textContent, metricText(80));
  assert.equal(env.frames.size, 0);
  assert.equal(env.root.querySelector('tbody').children[0], row);
});

test('tombstones fade once without moving focus or selection and the first render stays still', async (t) => {
  const descriptor = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => false});
  t.after(() => Object.defineProperty(reducedMotion, 'matches', descriptor));
  const env = await setup(t);
  const rows = [...env.root.querySelector('tbody').children];
  assert.ok(rows.every((row) => row.animations.length === 0));
  const boxes = env.root.querySelectorAll('input[data-node-id]');
  boxes[1].checked = true; boxes[1].dispatchEvent(new Event('change')); boxes[1].focus();
  env.send({nodes: [compact(env.inventory[1]), compact(node(3))]});
  assert.equal(rows[0].animations.length, 1);
  assert.deepEqual(rows[0].animations[0].keyframes, [{opacity: 0}, {opacity: 1}]);
  assert.deepEqual(rows[0].animations[0].options, {duration: 150, easing: 'ease'});
  assert.equal(boxes[0].disabled, true);
  assert.equal(boxes[1].checked, true);
  assert.equal(env.document.activeElement, boxes[1]);
  assert.deepEqual(env.root.querySelector('tbody').children, rows);
  env.send({nodes: [compact(env.inventory[1]), compact(node(3))]});
  assert.equal(rows[0].animations.length, 1);
  env.scope.abort();
  assert.ok(rows[0].animations.every((animation) => animation.cancelled));
});

test('rebuilding the table fades only rows that were not already shown', async (t) => {
  const descriptor = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => false});
  t.after(() => Object.defineProperty(reducedMotion, 'matches', descriptor));
  const env = await setup(t, {count: 55});
  const faded = () => Object.fromEntries(env.root.querySelector('tbody').children
    .map((row) => [row.querySelector('input').dataset.nodeId, row.animations.length]));
  const sort = env.root.querySelectorAll('th').find((item) => item.dataset.sort === 'name').querySelector('button');
  sort.click();
  assert.ok(Object.values(faded()).every((count) => count === 0));
  const before = new Set(Object.keys(faded()));
  button(env.root, copy('nodes.next')).click();
  const after = faded();
  assert.ok(Object.keys(after).length > 0);
  for (const [nodeID, count] of Object.entries(after)) assert.equal(count, before.has(nodeID) ? 0 : 1);
});

for (const hidden of [false, true]) test(`list fades respect ${hidden ? 'hidden document' : 'reduced motion'} and paging respects motion preference`, async (t) => {
  const descriptor = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => !hidden});
  t.after(() => Object.defineProperty(reducedMotion, 'matches', descriptor));
  const env = await setup(t, {count: 55});
  env.document.hidden = hidden;
  const rows = [...env.root.querySelector('tbody').children];
  const counts = rows.map((row) => row.animations.length);
  // Hidden pages cannot receive live snapshots; exercise the same tombstone path directly.
  const {tombstoneNodeRow} = await import('../src/console/lib/nodes-table.js');
  const row = rows[0];
  tombstoneNodeRow({element: row, checkbox: row.querySelector('input'), name: row.querySelector('a'), badge: row.querySelector('.badge')});
  assert.equal(row.animations.length, counts[0]);
  if (!hidden) {
    assert.ok(rows.every((row) => row.animations.length === 0));
    const scrolls = []; env.window.scrollTo = (value) => scrolls.push(value);
    button(env.root, copy('nodes.next')).click();
    assert.deepEqual(scrolls, [{top: 0, behavior: 'instant'}]);
  }
});

for (const sort of ['cpu', 'memory']) test(`${sort} sorting waits for explicit apply, preserving live row order and bar tween state`, async (t) => {
  const env = await setup(t, {search: `?sort=${sort}&dir=desc`});
  const key = sort === 'cpu' ? 'cpu_percent' : 'memory_used_percent';
  const index = sort === 'cpu' ? 3 : 4;
  assert.equal(env.root.querySelectorAll('th').find((header) => header.dataset.sort === sort).getAttribute('aria-sort'), 'descending');
  // 列表接口不含用量；首帧带来指标时直接重排，否则按用量排序在首屏无效。
  env.send({nodes: env.inventory.map((item, i) => ({...compact(item), [key]: i}))});
  assert.equal(env.root.querySelector('tbody').firstElementChild.querySelector('input').dataset.nodeId, id(env.inventory.length));
  assert.equal(button(env.root, copy('nodes.applyFilters')).parentElement.hidden, true);
  const rows = [...env.root.querySelector('tbody').children];
  // 数值变化但顺序不变时不提示。
  env.send({nodes: env.inventory.map((item, i) => ({...compact(item), [key]: i * 2 + 1}))});
  assert.equal(button(env.root, copy('nodes.applyFilters')).parentElement.hidden, true);
  env.send({nodes: env.inventory.map((item, i) => ({...compact(item), [key]: i ? 10 : 95}))});
  env.renderFrame(performance.now() + 250);
  assert.deepEqual(env.root.querySelector('tbody').children, rows);
  const raised = rows.find((row) => row.querySelector('input').dataset.nodeId === id(1));
  assert.equal(raised.children[index].querySelector('.usage-track').firstElementChild.style.width, '95%');
  assert.ok(raised.children[index].querySelector('.usage-meter').classList.contains('usage-bad'));
  const apply = button(env.root, copy('nodes.applyFilters'));
  assert.equal(apply.parentElement.hidden, false);
  apply.click();
  const first = env.root.querySelector('tbody').firstElementChild;
  assert.equal(first.querySelector('input').dataset.nodeId, id(1));
  assert.equal(apply.parentElement.hidden, true);
  assert.equal(location.search, `?sort=${sort}&dir=desc`);
});

test('column preferences survive filtering and live updates without showing default-hidden columns', async (t) => {
  const env = await setup(t);
  const rows = () => env.root.querySelector('tbody').querySelectorAll('tr');
  assert.equal(rows()[0].children[5].classList.contains('column-hidden'), true);
  assert.equal(rows()[0].children[10].classList.contains('column-hidden'), true);
  const picker = env.root.querySelector('.column-picker');
  const checkbox = picker.querySelectorAll('input').find((control) => control.dataset.column === 'last_seen');
  checkbox.checked = false; checkbox.dispatchEvent(new Event('change'));
  assert.equal(rows()[0].children[6].classList.contains('column-hidden'), true);
  env.send({nodes: env.inventory.map(compact)});
  assert.equal(rows()[0].children[6].classList.contains('column-hidden'), true);
  const nameHeader = env.root.querySelectorAll('th').find((header) => header.dataset.sort === 'name');
  nameHeader.querySelector('button').click();
  assert.equal(rows()[0].children[6].classList.contains('column-hidden'), true);
});

test('cancel selection clears all pages and restores the visible select-all control', async (t) => {
  const env = await setup(t, {count: 55});
  const all = env.root.querySelectorAll('input').find((control) => control.getAttribute('aria-label') === copy('nodes.selectAll'));
  all.checked = true; all.dispatchEvent(new Event('change'));
  button(env.root, copy('nodes.next')).click();
  const box = env.root.querySelector('input[data-node-id]');
  box.checked = true; box.dispatchEvent(new Event('change'));
  assert.equal(env.root.querySelector('.selection-count').textContent, copy('nodes.selected', {count: 51}));
  button(env.root, copy('nodes.cancelSelection')).click();
  assert.equal(env.root.querySelector('.bulk-actions').hidden, true);
  assert.equal(all.checked, false); assert.equal(all.indeterminate, false);
  assert.equal(document.activeElement, all);
  button(env.root, copy('nodes.previous')).click();
  assert.ok(env.root.querySelector('tbody').querySelectorAll('input').every((control) => !control.checked));
});

const selectNodes = (env) => {
  const boxes = env.root.querySelectorAll('input[data-node-id]');
  for (const box of boxes) { box.checked = true; box.dispatchEvent(new Event('change')); }
};
const submitMetadata = async (env, value) => {
  const dialog = env.body.querySelector('dialog');
  const form = dialog.querySelector('form');
  const control = form.querySelector('input,select');
  control.value = value; control.dispatchEvent(new Event('input'));
  form.dispatchEvent(new Event('submit', {cancelable: true}));
  await flush();
  if (dialog.classList.contains('closing')) dialog.dispatchEvent(new Event('animationend'));
  await flush();
  return dialog;
};

for (const value of ['', id(700)]) test(`bulk group PATCH ${value ? 'sets group' : 'clears group'} and reports partial failures`, async (t) => {
  const env = await setup(t, {mutate: (request) => request.url.endsWith(id(2))
    ? response({code: 'node_not_found'}, {status: 404}) : response({...node(1), group_id: value || null})});
  selectNodes(env);
  button(env.root, copy('nodes.setGroup')).click(); await flush();
  const dialog = env.body.querySelector('dialog');
  assert.deepEqual(dialog.querySelector('select').options.map((option) => option.value), ['', id(700)]);
  await submitMetadata(env, value);
  const patches = env.requests.filter((request) => request.method === 'PATCH');
  assert.deepEqual(patches.map((request) => [request.url, JSON.parse(request.body)]), [1, 2].map((n) => [`/api/v1/nodes/${id(n)}`, {group_id: value || null}]));
  const results = env.body.querySelector('dialog');
  assert.equal(results.querySelector('h2').textContent, copy('nodes.batchResults', {action: copy('nodes.setGroup')}));
  assert.ok(results.textContent.includes(copy('nodes.resultCounts', {count: 1, failed: 1})));
  assert.ok(results.textContent.includes(copy('nodes.result.failed')));
  assert.equal(env.root.querySelector('.bulk-actions').hidden, true);
  button(results, copy('nodes.close')).click(); results.dispatchEvent(new Event('animationend')); await flush();
});

test('bulk tag preserves each node tags and exact strings, without duplicates', async (t) => {
  const env = await setup(t, {mutate: () => response(node(1))});
  const inventory = [{...node(1), tags: ['existing', ' new ']}, {...node(2), tags: ['other']}];
  env.replaceInventory(inventory);
  env.send({nodes: inventory.map(compact)});
  button(env.root, copy('nodes.refreshList')).click(); await flush();
  selectNodes(env);
  button(env.root, copy('nodes.addTag')).click(); await flush();
  await submitMetadata(env, ' new ');
  const patches = env.requests.filter((request) => request.method === 'PATCH');
  assert.deepEqual(patches.map((request) => JSON.parse(request.body)), [{tags: ['existing', ' new ']}, {tags: ['other', ' new ']}]);
  const results = env.body.querySelector('dialog');
  assert.ok(results.textContent.includes(copy('nodes.resultCounts', {count: 2, failed: 0})));
  button(results, copy('nodes.close')).click(); results.dispatchEvent(new Event('animationend')); await flush();
});

test('bulk tag rejects blank and more than 255 UTF-8 bytes before sending, accepts the boundary', async (t) => {
  const env = await setup(t, {mutate: () => response(node(1))});
  selectNodes(env);
  button(env.root, copy('nodes.addTag')).click(); await flush();
  for (const value of ['', '  ', '界'.repeat(86)]) {
    const dialog = await submitMetadata(env, value);
    assert.ok(dialog.open);
    assert.ok(dialog.querySelector('input').validationMessage);
    assert.equal(env.requests.some((request) => request.method === 'PATCH'), false);
  }
  const boundary = '界'.repeat(85);
  await submitMetadata(env, boundary);
  const patches = env.requests.filter((request) => request.method === 'PATCH');
  assert.equal(patches.length, 2);
  assert.deepEqual(JSON.parse(patches[0].body), {tags: [boundary]});
  const results = env.body.querySelector('dialog');
  button(results, copy('nodes.close')).click(); results.dispatchEvent(new Event('animationend')); await flush();
});

for (const action of ['group', 'tag']) test(`bulk ${action} navigation abort cancels the in-flight PATCH and stops subsequent nodes and results`, async (t) => {
  let release;
  const env = await setup(t, {mutate: () => new Promise((resolve) => { release = () => resolve(response(node(1))); })});
  selectNodes(env);
  button(env.root, copy(action === 'group' ? 'nodes.setGroup' : 'nodes.addTag')).click(); await flush();
  await submitMetadata(env, action === 'group' ? id(700) : 'new');
  assert.equal(typeof release, 'function');
  const patch = env.requests.find((request) => request.method === 'PATCH');
  env.scope.abort();
  assert.ok(patch.signal.aborted);
  release(); await flush(); await flush();
  assert.equal(env.requests.filter((request) => request.method === 'PATCH').length, 1);
  assert.equal(env.body.querySelector('dialog'), null);
  assert.equal(env.root.querySelector('.bulk-actions').hidden, false);
});


test('bulk tag keeps frozen targets through selection changes and reports partial failure', async (t) => {
  const env = await setup(t, {mutate: (request) => request.url.endsWith(id(1))
    ? response({code: 'storage_busy'}, {status: 503}) : response(node(2))});
  selectNodes(env);
  button(env.root, copy('nodes.addTag')).click(); await flush();
  const box = env.root.querySelector('input[data-node-id]');
  box.checked = false; box.dispatchEvent(new Event('change'));
  await submitMetadata(env, 'new');
  const patches = env.requests.filter((request) => request.method === 'PATCH');
  assert.deepEqual(patches.map((request) => [request.url, JSON.parse(request.body)]), [1, 2].map((n) => [`/api/v1/nodes/${id(n)}`, {tags: ['new']}]));
  const results = env.body.querySelector('dialog');
  assert.ok(results.textContent.includes(copy('nodes.resultCounts', {count: 1, failed: 1})));
  assert.ok(results.textContent.includes(copy('errors.storage_busy')));
  button(results, copy('nodes.close')).click(); results.dispatchEvent(new Event('animationend')); await flush();
});

test('mobile sort control uses the same URL-backed order and default status pagination', async (t) => {
  const env = await setup(t, {count: 55});
  assert.equal(location.search, '');
  assert.equal(env.root.querySelector('tbody').firstElementChild.querySelector('a').textContent, 'Node 001');
  button(env.root, copy('nodes.next')).click();
  assert.equal(location.search, '?page=1');
  assert.equal(env.root.querySelector('tbody').firstElementChild.querySelector('a').textContent, 'Node 051');
  const sort = env.root.querySelector('.nodes-mobile-sort');
  sort.value = 'name:desc'; sort.dispatchEvent(new Event('change'));
  assert.equal(location.search, '?sort=name&dir=desc&page=1');
  assert.equal(env.root.querySelector('tbody').firstElementChild.querySelector('a').textContent, 'Node 005');
  const table = env.root.querySelector('table');
  for (const element of [table, table.querySelector('thead'), table.querySelector('tbody'), table.querySelector('tr')]) {
    assert.equal(element.getAttribute('role'), null);
  }
});

test('header enrollment issues through More and reloads the built ticket list, then restores More focus', async (t) => {
  const env = await setup(t, {page: 'node', hash: '#settings-enrollment', mutate: (request) => {
    assert.equal(request.url, '/api/v1/enrollments');
    assert.deepEqual(JSON.parse(request.body), {node_id: id(1)});
    return response({token: 'test-secret', server_version: '1.2.3', expires_at: '2026-10-28T00:30:00Z'});
  }});
  const before = env.requests.filter((request) => request.url.startsWith('/api/v1/enrollments')).length;
  assert.equal(button(env.root, copy('nodes.enrollment.issue')), undefined);
  const more = button(env.root, copy('nodes.more')); more.click();
  button(env.root, copy('nodes.enrollAgent')).click(); await flush();
  const dialog = env.body.querySelector('dialog'); assert.ok(dialog);
  button(dialog, copy('nodes.close')).click(); dialog.dispatchEvent(new Event('animationend')); await flush();
  assert.equal(env.requests.filter((request) => request.url.startsWith('/api/v1/enrollments')).length, before + 2);
  assert.equal(document.activeElement, more); assert.equal(more.disabled, false);
  assert.equal(more.getAttribute('aria-expanded'), 'false');
});

test('detail live primary tiles retain their values and bar DOM while tweening', async (t) => {
  const env = await setup(t, {page: 'node'});
  const cpu = sampleFact(env.root, copy('nodes.cpu'));
  const meter = cpu.querySelector('.usage-meter');
  const value = cpu.querySelector('.sample-value');
  const track = meter.querySelector('i');
  const primary = env.root.querySelector('.sample-primary');
  const tiles = [...primary.children];
  env.send({node: env.inventory[0], latest: latest(95)});
  assert.equal(sampleFact(env.root, copy('nodes.cpu')), cpu);
  assert.equal(cpu.querySelector('.usage-meter'), meter);
  assert.equal(cpu.querySelector('.sample-value'), value);
  assert.equal(meter.querySelector('i'), track);
  assert.deepEqual(primary.children, tiles);
  env.renderFrame(performance.now() + 1000);
  assert.equal(value.textContent, '95%');
  assert.equal(track.style.width, '95%');
  assert.equal(meter.classList.contains('usage-bad'), true);
  assert.equal(primary.children.length, 4);
  assert.equal(env.requests.filter((request) => request.url.includes('/history?')).length, 5);
});


test('overview top usage uses live values and preserves rows/meters across reordering', async (t) => {
  const env = await setup(t, {page: 'overview', count: 8});
  const resources = env.root.querySelector('.resource-columns');
  assert.equal(resources.querySelectorAll('li').length, 0);
  const values = env.inventory.map((item, index) => ({...compact(item), cpu_percent: index * 10, memory_used_percent: 95 - index}));
  values[7].presence = 'offline'; values[6].memory_used_percent = null;
  env.send({nodes: values});
  const [cpu, memory] = resources.querySelectorAll('ul');
  assert.deepEqual(cpu.querySelectorAll('a').map((a) => a.textContent), ['Node 007', 'Node 006', 'Node 005', 'Node 004', 'Node 003']);
  assert.equal(memory.children.length, 5);
  assert.equal(memory.querySelector('a').textContent, 'Node 001');
  assert.ok(memory.querySelector('.usage-bad'));
  const row = cpu.children[0]; const meter = row.querySelector('.usage-meter'); const fill = meter.querySelector('i');
  values[6].cpu_percent = 35; env.send({nodes: values}, 10);
  assert.equal(cpu.children[2], row); assert.equal(row.querySelector('.usage-meter'), meter); assert.equal(meter.querySelector('i'), fill);
  assert.equal(fill.style.width, '35%');
  values[6].cpu_percent = null; env.send({nodes: values}, 20);
  assert.equal(cpu.children.includes(row), false);
});
