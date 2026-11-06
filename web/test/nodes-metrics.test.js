import test from 'node:test';
import assert from 'node:assert/strict';
import {readConsoleFleet} from '../src/console/live.js';
import {setSession} from '../src/console/api.js';
import {createNodeRow} from '../src/console/lib/nodes-table.js';
import {patchNodesFromFleet} from '../src/console/lib/nodes-live.js';
import {formatPercent} from '../src/console/format.js';
import {copy, installDOM} from './console-dom.js';
import {frame, liveHarness, response} from './live-harness.js';

const node = () => ({id: '00000000-0000-4000-8000-000000000001', name: 'Node', status: 'unknown', presence: 'online', stopped: false,
  last_seen: '2026-10-28T00:00:00Z', agent_version: '1.0', tags: [], created_at: '2026-10-28T00:00:00Z'});

for (const key of ['cpu_percent', 'memory_used_percent']) {
  test(`fleet validator accepts optional, null and bounded ${key}`, async (t) => {
    let value;
    liveHarness(t, () => response(frame('snapshot', {data: {nodes: [{...node(), ...value}]}})));
    t.after(() => setSession({user: null, csrf_token: '', expires_at: ''}));
    for (value of [{}, {[key]: null}, {[key]: 0}, {[key]: 100}, {[key]: 23.456}]) {
      assert.equal((await readConsoleFleet()).nodes.length, 1);
    }
  });
  test(`fleet validator rejects invalid ${key}`, async (t) => {
    let value;
    liveHarness(t, () => response(frame('snapshot', {data: {nodes: [{...node(), [key]: value}]}})));
    t.after(() => setSession({user: null, csrf_token: '', expires_at: ''}));
    for (value of [-1, 100.001, '12', false, {}, []]) {
      await assert.rejects(readConsoleFleet(), (error) => error.code === 'unreadable_response');
    }
    // Raw JSON avoids JSON.stringify converting non-finite numbers to null.
    globalThis.fetch = async () => new Response(JSON.stringify(frame('snapshot', {data: {nodes: [{...node(), [key]: 'overflow'}]}})).replace('"overflow"', '1e400'));
    await assert.rejects(readConsoleFleet(), (error) => error.code === 'unreadable_response');
    // NaN is not valid JSON; a raw NaN token must reject the response too.
    globalThis.fetch = async () => new Response(JSON.stringify(frame('snapshot', {data: {nodes: [{...node(), [key]: 'nan'}]}})).replace('"nan"', 'NaN'));
    await assert.rejects(readConsoleFleet(), (error) => error.code === 'unreadable_response');
  });
}

test('node metric cells patch in place without reload flags, selection or sort changes; tombstones stay frozen', () => {
  const dom = installDOM();
  try {
    const item = node();
    const state = {nodes: [item], filter: {sort: 'status', status: 'online'}, rowRefs: new Map(), deleted: new Set(), selected: new Set([item.id]), visibleIDs: new Set([item.id])};
    const row = createNodeRow({node: item, query: '', selected: state.selected, deleted: state.deleted, groupNames: new Map(), rowRefs: state.rowRefs, onSelectionChange() {}});
    const refs = state.rowRefs.get(item.id);
    const children = [...row.children];
    const filter = {...state.filter};
    assert.equal(refs.cpu.textContent, '—');
    assert.equal(refs.memory.textContent, '—');
    assert.equal(row.children[3], refs.cpu);
    assert.equal(row.children[4], refs.memory);
    for (const cell of [refs.cpu, refs.memory]) {
      assert.ok(!cell.classList.contains('hide-compact'));
      assert.ok(cell.classList.contains('num'));
    }
    for (const [cpu, memory] of [[0, 100], [12.34, 76.54], [null, null]]) {
      const flags = patchNodesFromFleet({nodes: [{...node(), cpu_percent: cpu, memory_used_percent: memory}]}, state);
      assert.deepEqual(flags, {inventoryChanged: false, statusChanged: false, selectionRemoved: false});
      assert.equal(refs.cpu.textContent, cpu === null ? '—' : formatPercent(cpu));
      assert.equal(refs.memory.textContent, memory === null ? '—' : formatPercent(memory));
      assert.deepEqual(row.children, children);
      assert.equal(refs.checkbox.checked, true);
      assert.deepEqual([...state.selected], [item.id]);
      assert.deepEqual(state.filter, filter);
    }
    patchNodesFromFleet({nodes: [{...node(), cpu_percent: 10, memory_used_percent: 20}]}, state);
    patchNodesFromFleet({nodes: [node()]}, state);
    assert.equal(refs.cpu.textContent, '—');
    assert.equal(refs.memory.textContent, '—');
    patchNodesFromFleet({nodes: []}, state);
    patchNodesFromFleet({nodes: [{...node(), cpu_percent: 99, memory_used_percent: 88}]}, state);
    assert.equal(refs.cpu.textContent, '—');
    assert.equal(refs.memory.textContent, '—');
    assert.equal(item.cpu_percent, null);
    assert.equal(item.memory_used_percent, null);
    assert.ok(refs.checkbox.disabled);
  } finally { dom.restore(); }
});
