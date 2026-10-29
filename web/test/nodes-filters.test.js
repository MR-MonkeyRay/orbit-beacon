import assert from 'node:assert/strict';
import test from 'node:test';

import {nodeSortDirection, readNodeFilter, scopedNodes, sortedNodes} from '../src/console/lib/nodes-filters.js';

test('node filters parse supported query values and reject invalid ones', () => {
  assert.deepEqual(readNodeFilter('?q=edge&status=online&group=g1&sort=name&dir=asc&page=2'), {
    q: 'edge', status: 'online', group: 'g1', sort: 'name', dir: 'asc', page: 2
  });
  assert.deepEqual(readNodeFilter('?status=bogus&sort=bogus&dir=sideways&page=-2'), {
    q: '', status: '', group: '', sort: 'created', dir: '', page: 0
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
