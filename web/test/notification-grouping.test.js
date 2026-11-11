import test from 'node:test';
import assert from 'node:assert/strict';
import {copy, elements, flush, installDOM} from './console-dom.js';
import {setNavigationSignal} from '../src/console/api.js';
import {createNotificationInbox, groupNotifications} from '../src/console/ui/notification-inbox.js';

const item = (id, minute, extra = {}) => ({id, kind: 'alert', transition: 'firing', source_id: id,
  created_at: `2026-11-10T00:${String(minute).padStart(2, '0')}:00Z`, read: false,
  details: {rule_id: 'cpu', rule_name: 'CPU high', node_id: id, node_name: `Node ${id}`}, ...extra});

test('matching rule IDs and states within ten minutes of the group start group even when interleaved; nodes are not grouping keys', () => {
  const entries = [item('1', 30), item('2', 20), item('3', 19), item('4', 18, {transition: 'resolved'}),
    item('5', 17), item('task', 17, {kind: 'task'}), item('6', 16), item('7', 15, {details: {rule_id: 'other', rule_name: 'CPU high'}}),
    item('8', 14, {details: {rule_name: 'CPU high'}}), item('9', 13, {details: {rule_name: 'CPU high'}})];
  assert.deepEqual(groupNotifications(entries).map((group) => group.map((value) => value.id)),
    [['1', '2'], ['3', '5', '6'], ['4'], ['task'], ['7'], ['8'], ['9']]);
  // 同一时刻两条规则交替出现时各自合并。
  const memory = (id, minute) => item(id, minute, {details: {rule_id: 'memory', rule_name: 'Memory high', node_id: id}});
  assert.deepEqual(groupNotifications([memory('m1', 30), item('c1', 30), memory('m2', 29), item('c2', 29)]).map((group) => group.map((value) => value.id)),
    [['m1', 'm2'], ['c1', 'c2']]);
});

for (const activation of ['expand', 'mark']) test(`group ${activation} marks every unread member, deduplicates node count and preserves detail access`, async () => {
  const dom = installDOM();
  setNavigationSignal(null);
  const records = [item('1', 30, {read: true}), item('2', 29), item('3', 28, {details: {rule_id: 'cpu', rule_name: 'CPU high', node_id: '2', node_name: 'Node 2'}})];
  const calls = [];
  let refreshed = 0;
  const api = async (path, options = {}) => {
    calls.push({path, ...options});
    if (path.startsWith('/notifications?')) return {notifications: records, has_more: false, read_through: '3'};
    if (path.endsWith('/read')) return {};
    if (path.startsWith('/notifications/')) return records.find((record) => path.endsWith(`/${record.id}`));
    return {};
  };
  let inbox;
  try {
    inbox = createNotificationInbox({api, isActive: () => true, isStale: () => false, failureText: () => '', refreshSummary: () => refreshed++, openTaskDetails: () => {}});
    await flush();
    const list = document.querySelector('.notification-list');
    assert.equal(list.children.length, 1);
    const toggle = document.getElementById('notification-toggle-1');
    assert.equal(toggle.textContent.includes('CPU high · Firing · 2 nodes'), true);
    assert.equal(toggle.getAttribute('aria-expanded'), 'false');
    const members = document.getElementById(toggle.getAttribute('aria-controls'));
    assert.equal(members.hidden, true);
    (activation === 'expand' ? toggle : document.getElementById('notification-read-1')).click(); await flush();
    assert.deepEqual(calls.filter((call) => call.method === 'PATCH').map((call) => call.path), ['/notifications/2/read', '/notifications/3/read']);
    assert.ok(records.every((record) => record.read)); assert.equal(refreshed, 1);
    assert.equal(document.getElementById('notification-read-1').disabled, true);
    if (activation === 'expand') {
      assert.equal(document.getElementById('notification-toggle-1').getAttribute('aria-expanded'), 'true');
      const members = document.getElementById('notification-group-1');
      assert.equal(members.hidden, false);
      assert.ok(members.textContent.includes('Node 1')); assert.ok(members.textContent.includes('Node 2'));
      members.querySelector('button').click(); await flush();
      assert.ok(calls.some((call) => call.path === '/notifications/1'));
    }
  } finally { inbox?.close(); await inbox?.closed; dom.restore(); }
});

test('group mark-read failures keep unread members retryable and closing aborts group writes', async () => {
  const dom = installDOM(); setNavigationSignal(null);
  const records = [item('1', 30), item('2', 29)];
  let failed = true;
  let release;
  let requestSignal;
  const api = async (path, options = {}) => {
    if (path.startsWith('/notifications?')) return {notifications: records, has_more: false, read_through: '2'};
    requestSignal = options.signal;
    if (failed) throw new Error('network');
    return new Promise((resolve) => { release = resolve; });
  };
  const inbox = createNotificationInbox({api, isActive: () => true, isStale: () => false, failureText: () => '', refreshSummary: () => {}, openTaskDetails: () => {}});
  try {
    await flush(); document.getElementById('notification-read-1').click(); await flush();
    assert.equal(records.some((record) => record.read), false);
    assert.ok(document.getElementById('notification-toggle-1').textContent.includes(copy('notifications.readSaveFailed', {error: copy('errors.generic')})));
    failed = false; document.getElementById('notification-read-1').click(); await flush();
    inbox.close(); assert.equal(requestSignal.aborted, true); release({}); await flush();
    assert.equal(records.some((record) => record.read), false);
  } finally { inbox.close(); await inbox.closed; dom.restore(); }
});
