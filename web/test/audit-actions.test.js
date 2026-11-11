import test from 'node:test';
import assert from 'node:assert/strict';
import {copy, installDOM} from './console-dom.js';
import {auditItem} from '../src/console/lib/audit-item.js';
import {describeAction} from '../src/console/format.js';

test('lifecycle and enrollment audit actions have friendly labels', () => {
  for (const action of ['node.credential.revoke', 'node.disable', 'node.enable', 'node.delete', 'enrollment.create', 'enrollment.revoke', 'enrollment.claim']) {
    const label = describeAction(action);
    assert.ok(label.length > 0);
    assert.doesNotMatch(label, /[._]/);
    assert.notEqual(label, action);
  }
});


for (const [resource, field, typeKey] of [['alert_rule', 'name', 'audit.targetRule'], ['announcement', 'title', 'audit.targetAnnouncement']]) {
  test(`${resource} audit targets use recorded names and typed legacy IDs`, () => {
    const dom = installDOM();
    try {
      for (const verb of ['create', 'update', 'delete']) for (const withDetails of [false, true]) {
        const event = {id: 'event', target_id: '612e13a1-0000-4000-8000-000000000001', action: `${resource}.${verb}`, created_at: '2026-11-09T12:00:00Z'};
        const render = (details, names = new Map()) => auditItem({...event, details}, names, withDetails).querySelector('.ev-meta').querySelectorAll('b').at(-1).textContent;
        assert.equal(render({[field]: '<Name & title>'}), '<Name & title>');
        assert.equal(render({[field]: 'Snapshot'}, new Map([[event.target_id, 'Current name']])), 'Snapshot');
        assert.equal(render({}), `${copy(typeKey)} 612e13a1`);
        assert.equal(render(undefined), `${copy(typeKey)} 612e13a1`);
        assert.equal(render({[field]: '   '}), `${copy(typeKey)} 612e13a1`);
      }
      const node = auditItem({id: 'event', target_id: 'node', action: 'node.create', details: {name: 'Not used'}, created_at: '2026-11-09T12:00:00Z'}, new Map([['node', 'Node name']]), false);
      assert.equal(node.querySelector('.ev-meta').querySelectorAll('b').at(-1).textContent, 'Node name');
    } finally { dom.restore(); }
  });
}
