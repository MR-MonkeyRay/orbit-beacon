import test from 'node:test';
import assert from 'node:assert/strict';
import {nodeCommandsPanel} from '../src/console/pages/node-commands.js';
import {groupCommandActions} from '../src/console/lib/group-commands.js';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, copyPattern, flush, installDOM} from './console-dom.js';
import {errorText} from '../src/shared/errors.js';
import {describeAction} from '../src/console/format.js';

const id = '00000000-0000-4000-8000-000000000001';
const node = () => ({id, name: 'Host', status: 'unknown', presence: 'online', stopped: false});
const runtime = {remote_commands: ['restart', 'stop']};
const record = (state = 'pending', result_code = '') => ({id, kind: 'restart', state, result_code, requested_by: 'Alice', created_at: '2026-11-05T10:00:00Z'});
const button = (root, key) => root.querySelectorAll('button').find((item) => item.textContent === copy(key));
async function setup(t, {current = node(), observed = runtime, records = []} = {}) {
  const dom = installDOM();
  const previous = new Map(['fetch', 'window', 'setTimeout', 'clearTimeout'].map((key) => [key, globalThis[key]]));
  globalThis.window = new EventTarget();
  globalThis.setTimeout = () => 0;
  globalThis.clearTimeout = () => {};
  const scope = new AbortController();
  setNavigationSignal(scope.signal);
  setSession({user: {id, username: 'Alice', must_change_password: false}, csrf_token: 'csrf', expires_at: ''});
  const host = document.createElement('div'); host.id = 'toasts'; dom.body.append(host);
  const requests = [];
  globalThis.fetch = async (url, options) => {
    requests.push({url, options});
    return new Response(JSON.stringify(url.includes('/groups/') ? {created: 2, skipped: 3} : {commands: records}));
  };
  const panel = nodeCommandsPanel(current, observed, scope.signal);
  dom.body.append(panel.element); panel.attach(); dom.mutations(); await flush();
  t.after(() => {
    scope.abort(); setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''});
    for (const [key, value] of previous) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    dom.restore();
  });
  return {...dom, panel, scope, requests, current, setRecords(value) { records = value; }};
}
function confirmTyped(env, expected) {
  const dialog = env.body.querySelector('dialog');
  assert.ok(dialog);
  const entry = dialog.querySelector('input');
  const submit = dialog.querySelectorAll('button').find((item) => item.type === 'submit');
  assert.equal(submit.disabled, true);
  entry.value = 'wrong'; entry.dispatchEvent(new Event('input')); assert.equal(submit.disabled, true);
  entry.value = expected; entry.dispatchEvent(new Event('input')); assert.equal(submit.disabled, false);
  dialog.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true}));
  dialog.dispatchEvent(new Event('animationend'));
}

test('remote controls gate each capability, runtime, active state, disable and stopped', async (t) => {
  for (const [label, options, disabled] of [
    ['allowed', {}, [false, false]], ['no runtime', {observed: null}, [true, true]],
    ['stop only', {observed: {remote_commands: ['stop']}}, [true, false]],
    ['restart only', {observed: {remote_commands: ['restart']}}, [false, true]],
    ['no commands', {observed: {remote_commands: []}}, [true, true]],
    ['pending', {records: [record()]}, [true, true]], ['delivered', {records: [record('delivered')]}, [true, true]],
    ['disabled', {current: {...node(), status: 'disabled'}}, [true, true]],
    ['stopped', {current: {...node(), stopped: true}}, [true, true]],
    ['deleted', {current: {...node(), deleted: true}}, [true, true]]
  ]) await t.test(label, async (t) => {
    const env = await setup(t, options);
    assert.deepEqual(['restart', 'stop'].map((kind) => button(env.panel.element, `nodes.command.${kind}`).disabled), disabled);
    if (!options.records?.length) assert.equal(env.panel.element.querySelector('.table-wrap').hidden, true);
  });
});

test('stop requires the node name and restart has no body', async (t) => {
  const env = await setup(t);
  button(env.panel.element, 'nodes.command.stop').click(); await flush();
  let dialog = env.body.querySelector('dialog');
  assert.equal(dialog.querySelector('h2').textContent, copy('nodes.command.stopTitle'));
  assert.equal(dialog.querySelector('p').textContent, copy('nodes.command.stopConsequence'));
  assert.equal(env.requests.filter((item) => item.options.method === 'POST').length, 0);
  confirmTyped(env, 'Host'); await flush();
  const post = env.requests.find((item) => item.options.method === 'POST');
  assert.deepEqual(JSON.parse(post.options.body), {kind: 'stop', confirm: 'Host'});
  button(env.panel.element, 'nodes.command.restart').click(); await flush();
  dialog = env.body.querySelector('dialog');
  assert.equal(dialog.querySelector('h2').textContent, copy('nodes.command.restartTitle'));
  assert.equal(dialog.querySelector('p'), null);
  button(dialog, 'nodes.command.restart').click(); dialog.dispatchEvent(new Event('animationend')); await flush();
  assert.deepEqual(JSON.parse(env.requests.filter((item) => item.options.method === 'POST').at(-1).options.body), {kind: 'restart'});
});

test('active commands poll at 3 seconds, terminal commands stop, detach and abort clear timers', async (t) => {
  const env = await setup(t, {records: [record()]});
  assert.equal(env.timers.size, 1);
  env.setRecords([record('succeeded', 'ok')]);
  [...env.timers.values()][0](); await flush();
  assert.equal(env.timers.size, 0);
  env.setRecords([record('delivered')]);
  env.panel.detach(); env.panel.attach(); await flush();
  assert.equal(env.timers.size, 1);
  env.panel.detach(); assert.equal(env.timers.size, 0);
  env.panel.attach(); await flush(); assert.equal(env.timers.size, 1);
  env.scope.abort(); assert.equal(env.timers.size, 0);
});

test('pending cancellation, requester and all short status and result labels', async (t) => {
  const env = await setup(t, {records: [record()]});
  assert.match(env.panel.element.textContent, /Alice/);
  button(env.panel.element, 'controls.cancel').click(); await flush();
  assert.ok(env.requests.some((item) => item.url.endsWith(`/commands/${id}/cancel`) && item.options.method === 'POST'));
  for (const state of ['pending', 'delivered', 'succeeded', 'failed', 'rejected', 'expired', 'canceled']) {
    env.setRecords([record(state)]); env.panel.detach(); env.panel.attach(); await flush();
    assert.match(env.panel.element.querySelector('tbody').textContent, copyPattern(`nodes.command.state.${state}`));
    assert.equal(Boolean(button(env.panel.element, 'controls.cancel')), state === 'pending');
  }
  for (const code of ['disabled_locally', 'unsupported', 'interrupted', 'failed', 'ok', 'expired']) {
    env.setRecords([record('rejected', code)]); env.panel.detach(); env.panel.attach(); await flush();
    const label = env.panel.element.querySelector('tbody').querySelector('tr').children[1].textContent;
    assert.equal(label, copy('nodes.command.state.rejected') + (['ok', 'expired'].includes(code) ? '' : ` · ${copy(`nodes.command.result.${code}`)}`));
  }
});

for (const kind of ['restart', 'stop']) test(`group ${kind} requires its name and reports created and skipped`, async (t) => {
  const env = await setup(t);
  const controls = groupCommandActions({id, name: 'Production'}, env.scope.signal);
  env.body.append(...controls);
  controls[kind === 'restart' ? 0 : 1].click(); await flush();
  const dialog = env.body.querySelector('dialog');
  assert.equal(dialog.querySelector('h2').textContent, copy(`nodes.command.${kind}All`));
  if (kind === 'restart') assert.equal(dialog.querySelector('p'), null);
  confirmTyped(env, 'Production'); await flush();
  const post = env.requests.find((item) => item.url.includes('/groups/'));
  assert.equal(post.url, `/api/v1/groups/${id}/commands`);
  assert.deepEqual(JSON.parse(post.options.body), {kind, confirm: 'Production'});
  assert.match(env.body.querySelector('#toasts').textContent, copyPattern('nodes.command.bulkResult', {created: 2, skipped: 3}));
});

test('command errors and audit actions have localized labels', () => {
  for (const code of ['command_not_allowed', 'command_active', 'node_unavailable']) assert.equal(errorText({code}), copy(`errors.${code}`));
  for (const [action, key] of Object.entries({create: 'Create', bulk: 'Bulk', succeeded: 'Succeeded', failed: 'Failed', rejected: 'Rejected', expired: 'Expired', canceled: 'Canceled'})) {
    assert.equal(describeAction(`command.${action}`), copy(`audit.command${key}`));
  }
});

test('detaching aborts an in-flight command read and ignores its late response', async (t) => {
  const env = await setup(t);
  let release;
  let readSignal;
  globalThis.fetch = async (_url, options) => {
    readSignal = options.signal;
    return new Promise((resolve) => { release = resolve; });
  };
  env.panel.detach(); env.panel.attach();
  assert.equal(readSignal.aborted, false);
  env.panel.detach(); assert.equal(readSignal.aborted, true);
  release(new Response(JSON.stringify({commands: [record()]}))); await flush();
  assert.equal(env.timers.size, 0);
  assert.equal(env.panel.element.querySelector('.table-wrap').hidden, true);
});

test('a pending poll response cannot overwrite a newer command mutation refresh', async (t) => {
  const env = await setup(t, {records: [record()]});
  let release;
  let reads = 0;
  globalThis.fetch = async (_url, options) => {
    if (options.method !== 'POST' && reads++ === 0) return new Promise((resolve) => { release = resolve; });
    return new Response(JSON.stringify({commands: [record('canceled')]}));
  };
  [...env.timers.values()][0](); await flush();
  button(env.panel.element, 'controls.cancel').click(); await flush();
  assert.equal(env.timers.size, 0);
  release(new Response(JSON.stringify({commands: [record()]}))); await flush();
  assert.equal(env.timers.size, 0);
  assert.match(env.panel.element.querySelector('tbody').textContent, copyPattern('nodes.command.state.canceled'));
  assert.equal(button(env.panel.element, 'controls.cancel'), undefined);
});
