import test from 'node:test';
import assert from 'node:assert/strict';
import {allNodes, invalidate, nextOffset} from '../src/console/api.js';

test('allNodes follows the pagination envelope at an exact page boundary', async () => {
  const calls = [];
  const first = Array.from({length: 100}, (_, n) => ({id: `node-${n}`}));
  const final = {id: 'node-final'};
  globalThis.fetch = async (path) => {
    calls.push(path);
    const body = calls.length === 1
      ? {nodes: first, has_more: true, next_offset: 41}
      : {nodes: [final], has_more: false};
    return new Response(JSON.stringify(body), {headers: {'Content-Type': 'application/json'}});
  };
  invalidate('nodes');
  try {
    assert.deepEqual(await allNodes(), [...first, final]);
    assert.deepEqual(calls, ['/api/v1/nodes?limit=100&offset=0', '/api/v1/nodes?limit=100&offset=41']);
  } finally {
    invalidate('nodes');
    delete globalThis.fetch;
  }
});

test('allNodes stops when a full page says there are no more nodes', async () => {
  const nodes = Array.from({length: 100}, (_, n) => ({id: `node-${n}`}));
  let calls = 0;
  globalThis.fetch = async () => {
    calls += 1;
    return new Response(JSON.stringify({nodes, has_more: false}), {headers: {'Content-Type': 'application/json'}});
  };
  invalidate('nodes');
  try {
    assert.deepEqual(await allNodes(), nodes);
    assert.equal(calls, 1);
  } finally {
    invalidate('nodes');
    delete globalThis.fetch;
  }
});

test('continuations must be safe integers that move forward', () => {
  assert.equal(nextOffset({next_offset: 100}, 0), 100);
  for (const page of [{}, {next_offset: 0}, {next_offset: '100'}, {next_offset: 1.5}, {next_offset: 2 ** 60}]) {
    assert.throws(() => nextOffset(page, 0), /invalid page offset/);
  }
});
