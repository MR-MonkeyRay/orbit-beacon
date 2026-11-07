import test from 'node:test';
import assert from 'node:assert/strict';
import {cursorPath, nextCursor} from '../src/console/ui/pager.js';

test('cursor pager accepts terminal pages and advancing opaque cursors', () => {
  assert.equal(nextCursor({has_more: false}, 'old'), null);
  assert.equal(nextCursor({has_more: true, next_cursor: 'opaque/+cursor'}, 'old'), 'opaque/+cursor');
});

test('cursor pager rejects missing and non-advancing cursors', () => {
  assert.throws(() => nextCursor({has_more: true}, 'old'), {code: 'unreadable_response'});
  assert.throws(() => nextCursor({has_more: true, next_cursor: 'old'}, 'old'), {code: 'unreadable_response'});
  assert.throws(() => nextCursor({has_more: true, next_cursor: 'seen'}, 'current', new Set(['seen'])), {code: 'unreadable_response'});
});

test('cursor paths preserve opaque cursor values', () => {
  assert.equal(cursorPath('/tasks', 20), '/tasks?limit=20');
  assert.equal(cursorPath('/tasks', 20, 'opaque/+cursor'), '/tasks?limit=20&before=opaque%2F%2Bcursor');
});
