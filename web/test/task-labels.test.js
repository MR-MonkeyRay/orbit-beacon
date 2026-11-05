import test from 'node:test';
import assert from 'node:assert/strict';
import {copy} from './console-dom.js';

test('backup task state and category labels share the console locale', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  Object.defineProperty(globalThis, 'localStorage', {configurable: true, value: {getItem: () => 'en', setItem() {}}});
  try {
    const {categoryNames, knownTaskState, taskCategoryKey, taskState, taskStateKey} = await import('../src/console/lib/task-labels.js');
    assert.equal(knownTaskState('running'), true);
    assert.equal(knownTaskState('other'), false);
    assert.equal(knownTaskState('__proto__'), false);
    assert.equal(taskStateKey('running'), 'common.running');
    assert.equal(taskStateKey('other'), undefined);
    assert.equal(taskCategoryKey('events'), 'common.events');
    assert.equal(taskCategoryKey('other'), undefined);
    assert.equal(taskState({state: 'running'}), copy('common.running'));
    assert.equal(taskState({state: 'other'}), copy('data.state.unknown'));
    assert.equal(categoryNames(['configuration', 'events']), new Intl.ListFormat('en').format([copy('common.configuration'), copy('common.events')]));
    assert.equal(categoryNames(['other']), copy('data.category.unknown'));
  } finally {
    if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage); else delete globalThis.localStorage;
  }
});
