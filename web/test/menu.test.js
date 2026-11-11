import test from 'node:test';
import assert from 'node:assert/strict';
import {installDOM} from './console-dom.js';
import {setNavigationSignal} from '../src/console/api.js';
import {menuButton} from '../src/console/ui/menu.js';
import {copy} from './console-dom.js';

const key = (element, value) => {
  const event = Object.assign(new Event('keydown', {cancelable: true}), {key: value});
  element.dispatchEvent(event);
  return event;
};

test('menu keyboard activation, roving focus, Escape, Tab, selection and outside dismissal', () => {
  const dom = installDOM();
  const scope = new AbortController(); setNavigationSignal(scope.signal);
  try {
    let selected = 0;
    const root = menuButton({label: 'More', icon: 'key', items: [
      {label: 'First', onSelect: () => selected++}, {label: 'Disabled', disabled: true}, {label: 'Delete', danger: true}
    ]});
    dom.body.append(root); dom.mutations();
    const trigger = root.querySelector('button'); const menu = root.querySelector('[role="menu"]');
    const items = root.querySelectorAll('[role="menuitem"]');
    assert.equal(trigger.getAttribute('aria-haspopup'), 'menu');
    assert.equal(trigger.getAttribute('aria-controls'), menu.id);
    assert.equal(items[2].classList.contains('menu-danger'), true);
    for (const activation of ['Enter', ' ', 'ArrowDown']) {
      assert.equal(key(trigger, activation).defaultPrevented, true);
      assert.equal(menu.hidden, false); assert.equal(document.activeElement, items[0]);
      key(menu, 'ArrowDown'); assert.equal(document.activeElement, items[2]);
      key(menu, 'ArrowDown'); assert.equal(document.activeElement, items[0]);
      key(menu, 'ArrowUp'); assert.equal(document.activeElement, items[2]);
      key(menu, 'Home'); assert.equal(document.activeElement, items[0]);
      key(menu, 'End'); assert.equal(document.activeElement, items[2]);
      key(root, 'Escape'); assert.equal(menu.hidden, true); assert.equal(document.activeElement, trigger);
    }
    trigger.click(); key(root, 'Tab');
    assert.equal(menu.hidden, true); assert.equal(document.activeElement, trigger);
    trigger.click(); items[0].click();
    assert.equal(selected, 1); assert.equal(menu.hidden, true); assert.equal(document.activeElement, trigger);
    trigger.click(); items[1].click(); assert.equal(menu.hidden, false);
    document.dispatchEvent(new Event('pointerdown')); assert.equal(menu.hidden, true);
    trigger.click(); scope.abort(); assert.equal(menu.hidden, true);
    assert.equal(trigger.getAttribute('aria-expanded'), 'false');
  } finally { scope.abort(); setNavigationSignal(null); dom.restore(); }
});

for (const status of ['pending', 'unknown', 'disabled']) test(`node header keeps ${status} lifecycle rules and puts ticket/revoke/delete in More`, async () => {
  const dom = installDOM();
  const scope = new AbortController(); setNavigationSignal(scope.signal);
  try {
    const previous = globalThis.window; globalThis.window = new EventTarget();
    let lifecycleActions;
    try { ({lifecycleActions} = await import('../src/console/pages/node-lifecycle.js')); }
    finally { if (previous === undefined) delete globalThis.window; else globalThis.window = previous; }
    const node = {id: 'node', name: 'Node', status};
    const root = lifecycleActions(node); dom.body.append(root); dom.mutations();
    const items = root.querySelectorAll('[role="menuitem"]');
    assert.deepEqual(items.map((item) => item.textContent), [copy('nodes.enrollAgent'), copy('nodes.revokeCredential'), copy('nodes.deleteNode')]);
    assert.deepEqual(items.map((item) => item.disabled), [false, false, false]);
    assert.deepEqual(items.map((item) => item.classList.contains('menu-danger')), [false, true, true]);
    assert.equal(root.querySelector('button').textContent, copy(status === 'disabled' ? 'nodes.enable' : 'nodes.disable'));
    node.deleted = true; root.snapshot();
    assert.equal(root.querySelectorAll('button').every((control) => control.disabled), true);
  } finally { scope.abort(); setNavigationSignal(null); dom.restore(); }
});
