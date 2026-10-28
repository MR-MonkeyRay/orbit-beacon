import test from 'node:test';
import assert from 'node:assert/strict';
import {describeAction} from '../src/console/format.js';

test('lifecycle and enrollment audit actions have friendly labels', () => {
  for (const action of ['node.credential.revoke', 'node.disable', 'node.enable', 'node.delete', 'enrollment.create', 'enrollment.revoke', 'enrollment.claim']) {
    const label = describeAction(action);
    assert.ok(label.length > 0);
    assert.doesNotMatch(label, /[._]/);
    assert.notEqual(label, action);
  }
});
