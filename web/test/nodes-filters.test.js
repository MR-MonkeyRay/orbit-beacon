import assert from 'node:assert/strict';
import test from 'node:test';

import {nodeSortDirection, readNodeFilter, scopedNodes, sortedNodes, writeNodeFilter} from '../src/console/lib/nodes-filters.js';

test('node filters parse supported query values and reject invalid ones', () => {
  assert.deepEqual(readNodeFilter('?q=edge&status=online&group=g1&sort=name&dir=asc&page=2'), {
    q: 'edge', status: 'online', group: 'g1', sort: 'name', dir: 'asc', page: 2
  });
  assert.deepEqual(readNodeFilter('?status=bogus&sort=bogus&dir=sideways&page=-2'), {
    q: '', status: '', group: '', sort: 'status', dir: '', page: 0
  });
});

test('node filtering and sorting preserve list behavior', () => {
  const alpha = {id: 'a', name: 'Alpha', presence: 'offline', group_id: 'g1', created_at: '2025-01-01'};
  const beta = {id: 'b', name: 'Beta', presence: 'online', group_id: '', created_at: '2025-02-01'};
  const indexed = [{node: alpha, text: 'alpha west'}, {node: beta, text: 'beta east'}];
  const filter = {q: 'east', status: '', group: 'none', sort: 'created', dir: '', page: 0};

  assert.deepEqual(scopedNodes(indexed, filter), [beta]);
  assert.equal(nodeSortDirection(filter), 'desc');
  assert.deepEqual(sortedNodes([alpha, beta], {...filter, q: '', group: ''}), [beta, alpha]);
});

for (const [sort, key] of [['cpu', 'cpu_percent'], ['memory', 'memory_used_percent']]) {
  for (const dir of ['asc', 'desc']) test(`${sort} sorts ${dir} with null and missing metrics last`, () => {
    const nodes = [null, 90, undefined, 0, 80].map((value, i) => ({id: String(i), name: String(i), [key]: value}));
    assert.deepEqual(sortedNodes(nodes, {sort, dir}).map((node) => node[key]), dir === 'asc' ? [0, 80, 90, null, undefined] : [90, 80, 0, null, undefined]);
    assert.equal(readNodeFilter(`?sort=${sort}&dir=${dir}`).sort, sort);
  });
}

test('default status sort is offline, never, pending, online, disabled, stopped, then name', () => {
  const statuses = ['online', 'disabled', 'pending', 'never', 'offline', 'stopped'];
  const nodes = statuses.flatMap((presence, i) => ['B', 'A'].map((name) => ({id: `${i}${name}`, name, presence: presence === 'stopped' ? 'offline' : presence, stopped: presence === 'stopped'})));
  const sorted = sortedNodes(nodes, readNodeFilter(''));
  assert.deepEqual(sorted.map((node) => node.stopped ? 'stopped' : node.presence), ['offline', 'never', 'pending', 'online', 'disabled', 'stopped'].flatMap((status) => [status, status]));
  assert.deepEqual(sorted.map((node) => node.name), statuses.flatMap(() => ['A', 'B']));
});

test('default status URL stays clean and explicit legacy created sort round-trips', () => {
  const original = {location: globalThis.location, history: globalThis.history};
  const urls = [];
  globalThis.location = {pathname: '/nodes', search: '?sort=status&dir=asc'};
  globalThis.history = {state: null, replaceState(_state, _title, url) { urls.push(url); }};
  try {
    writeNodeFilter(readNodeFilter('?sort=status&dir=asc'));
    assert.equal(urls.at(-1), '/nodes');
    writeNodeFilter(readNodeFilter('?sort=created&page=1'));
    assert.equal(urls.at(-1), '/nodes?sort=created&page=1');
    assert.equal(nodeSortDirection(readNodeFilter('?sort=created')), 'desc');
  } finally { Object.assign(globalThis, original); }
});

test('order sort defaults descending and stabilizes ties by name then id', () => {
  const filter = readNodeFilter('?sort=order');
  assert.equal(nodeSortDirection(filter), 'desc');
  const nodes = [
    {id: 'z', name: 'B', display_order: 10},
    {id: 'b', name: 'A', display_order: 10},
    {id: 'a', name: 'A', display_order: 10},
    {id: 'c', name: 'C', display_order: -1},
    {id: 'd', name: 'D'},
  ];
  assert.deepEqual(sortedNodes(nodes, filter).map((node) => node.id), ['a', 'b', 'z', 'd', 'c']);
});
