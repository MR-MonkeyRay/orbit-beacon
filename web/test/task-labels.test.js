import test from 'node:test';
import assert from 'node:assert/strict';

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
    assert.equal(taskState({state: 'running'}), 'Running');
    assert.equal(taskState({state: 'other'}), 'Unknown');
    assert.equal(categoryNames(['configuration', 'events']), 'Configuration and Events');
    assert.equal(categoryNames(['other']), 'Unknown category');
  } finally {
    if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage); else delete globalThis.localStorage;
  }
});
