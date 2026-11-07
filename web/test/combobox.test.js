import test from 'node:test';
import assert from 'node:assert/strict';
import {h} from '../src/shared/dom.js';
import {combobox, field, onSubmit} from '../src/console/ui/forms.js';
import {modal, closeDialogs, hasUnsavedDialogChanges} from '../src/console/ui/dialogs.js';
import {alertScope} from '../src/console/ui/alert-scope.js';
import {copy, elements, flush, installDOM} from './console-dom.js';

const key = (entry, key, props = {}) => {
  const event = Object.assign(new Event('keydown', {cancelable: true, bubbles: true}), {key, ...props});
  entry.dispatchEvent(event);
  return event;
};
const options = (root) => elements(root).filter((item) => item.getAttribute('role') === 'option');
function setup(t) {
  const dom = installDOM();
  t.after(() => { closeDialogs(); dom.restore(); });
  const control = combobox([['', copy('alerts.choose')], ['a', 'Alpha'], ['b', 'Beta'], ['c', 'GAMMA']], 'a', {name: 'scope_id', required: true, dataset: {field: 'scope_id'}});
  const wrapper = field(copy('alerts.rules.groupOrNode'), control);
  dom.body.append(wrapper);
  return {...dom, control, entry: control.entry, wrapper};
}

test('combobox shows the empty choice as a placeholder so typing searches', (t) => {
  const {control, entry, wrapper} = setup(t);
  control.value = '';
  assert.equal(entry.value, '');
  assert.equal(entry.placeholder, copy('alerts.choose'));
  entry.value = 'bet';
  entry.dispatchEvent(new Event('input', {bubbles: true}));
  assert.deepEqual(options(wrapper).map((item) => item.textContent), ['Beta']);
});

test('combobox selects its text on focus so typing replaces the current name', (t) => {
  const {entry} = setup(t);
  entry.focus();
  entry.dispatchEvent(new Event('focus'));
  assert.equal(entry.selected, true);
  assert.equal(entry.getAttribute('aria-expanded'), 'false');
});

test('combobox keyboard selects IDs while keeping focus and ARIA on input', (t) => {
  const {control, entry, wrapper, document} = setup(t);
  entry.focus(); key(entry, 'ArrowDown');
  assert.equal(entry.getAttribute('role'), 'combobox');
  const list = wrapper.querySelector('#' + entry.getAttribute('aria-controls'));
  assert.equal(list.getAttribute('role'), 'listbox');
  assert.equal(entry.getAttribute('aria-expanded'), 'true');
  key(entry, 'ArrowDown'); key(entry, 'ArrowDown'); key(entry, 'ArrowUp');
  assert.equal(entry.getAttribute('aria-activedescendant'), options(wrapper)[1].id);
  assert.equal(options(wrapper)[1].getAttribute('aria-selected'), 'true');
  let changes = 0;
  control.addEventListener('change', (event) => { changes++; assert.equal(event.bubbles, true); });
  assert.equal(key(entry, 'Enter').defaultPrevented, true);
  assert.equal(control.value, 'a'); assert.equal(entry.value, 'Alpha'); assert.equal(changes, 1);
  assert.equal(entry.getAttribute('aria-expanded'), 'false');
  assert.equal(entry.getAttribute('aria-activedescendant'), null);
  assert.equal(document.activeElement, entry);
  key(entry, 'ArrowUp'); assert.equal(entry.getAttribute('aria-activedescendant'), options(wrapper).at(-1).id);
  assert.equal(key(entry, 'Tab').defaultPrevented, false);
  assert.equal(entry.getAttribute('aria-expanded'), 'false');
});

test('combobox filters by name substring without case sensitivity and supports mouse selection', (t) => {
  const {control, entry, wrapper} = setup(t);
  entry.value = 'amm'; entry.dispatchEvent(new Event('input'));
  assert.deepEqual(options(wrapper).map((option) => option.textContent), ['GAMMA']);
  const option = options(wrapper)[0];
  const down = new Event('mousedown', {cancelable: true}); option.dispatchEvent(down);
  assert.equal(down.defaultPrevented, true);
  option.click(); assert.equal(control.value, 'c'); assert.equal(entry.value, 'GAMMA');
  entry.value = 'not found'; entry.dispatchEvent(new Event('input'));
  assert.equal(options(wrapper).length, 0);
  assert.equal(wrapper.querySelector('.text-note').textContent, copy('nodes.noMatching'));
  assert.equal(key(entry, 'Enter').defaultPrevented, true); assert.equal(control.value, 'c');
});

test('combobox retains select interface, unknown IDs, validation, focus and replacement options', async (t) => {
  const {control, entry, wrapper, document} = setup(t);
  assert.equal(control.name, 'scope_id'); assert.equal(control.dataset.field, 'scope_id');
  control.value = 'b'; assert.equal(entry.value, 'Beta');
  control.value = 'deleted-node'; assert.equal(control.value, 'deleted-node');
  assert.equal(entry.value, copy('nodes.unknownID', {id: 'deleted-'}));
  control.disabled = true; assert.equal(entry.disabled, true);
  assert.equal(key(entry, 'ArrowDown').defaultPrevented, false);
  control.disabled = false; assert.equal(entry.disabled, false);
  Object.defineProperty(control, 'validity', {configurable: true, get: () => ({valid: !control.validationMessage, customError: Boolean(control.validationMessage)})});
  control.setCustomValidity(copy('controls.validation.required'));
  const invalid = new Event('invalid', {cancelable: true}); control.dispatchEvent(invalid);
  assert.equal(invalid.defaultPrevented, true); assert.equal(document.activeElement, entry);
  assert.equal(entry.getAttribute('aria-invalid'), 'true');
  assert.equal(wrapper.querySelector('.field-error').textContent, copy('controls.validation.required'));
  const form = h('form', null, wrapper); document.body.append(form);
  const submit = h('button'); form.append(submit);
  const errorBox = h('div'); form.append(errorBox);
  onSubmit(form, submit, () => { throw {fields: {scope_id: copy('controls.validation.invalid')}, clientValidation: true}; }, errorBox);
  form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
  assert.equal(document.activeElement, entry);
  assert.equal(wrapper.querySelector('.field-error').textContent, copy('controls.validation.invalid'));
  control.setCustomValidity(''); control.setChoices([['x', 'New node']]); control.value = 'x';
  assert.equal(entry.value, 'New node'); assert.deepEqual(control.options.map((option) => option.value), ['x']);
  control.setChoices([]); assert.equal(control.value, ''); assert.equal(entry.value, '');
});

test('combobox Escape is consumed before dialog cancellation and IME leaves navigation untouched', (t) => {
  const {control, entry} = setup(t);
  modal(copy('alerts.rules.addTitle'), () => control.wrapper);
  entry.focus(); key(entry, 'ArrowDown');
  let stopped = false;
  const escape = Object.assign(new Event('keydown', {cancelable: true, bubbles: true}), {key: 'Escape'});
  escape.stopPropagation = () => { stopped = true; };
  entry.dispatchEvent(escape);
  assert.equal(stopped, true); assert.equal(escape.defaultPrevented, true);
  assert.equal(document.querySelector('dialog').open, true);
  assert.equal(key(entry, 'Escape').defaultPrevented, false);
  assert.equal(key(entry, 'ArrowDown', {isComposing: true}).defaultPrevented, false);
  assert.equal(entry.getAttribute('aria-expanded'), 'false');
  key(entry, 'ArrowDown');
  const value = control.value;
  for (const name of ['ArrowDown', 'Enter', 'Escape']) assert.equal(key(entry, name, {isComposing: true}).defaultPrevented, false);
  assert.equal(control.value, value); assert.equal(entry.getAttribute('aria-expanded'), 'true');
  entry.value = 'Beta'; entry.dispatchEvent(Object.assign(new Event('input'), {isComposing: true}));
  assert.equal(hasUnsavedDialogChanges(), false);
  entry.dispatchEvent(new Event('compositionend')); assert.equal(control.value, value);
});

test('alert scopes use searchable nodes, native groups, and retain deleted original IDs', (t) => {
  const dom = installDOM(); t.after(() => dom.restore());
  const {scope, scopeID, fields} = alertScope({scope_kind: 'node', scope_id: 'deleted-node'}, {nodes: [{id: 'a', name: 'Alpha'}], groups: [{id: 'g', name: 'Group'}]});
  dom.body.append(...fields);
  assert.equal(scopeID.hidden, true); assert.equal(scopeID.entry.hidden, false);
  assert.equal(scopeID.value, 'deleted-node');
  assert.equal(scopeID.entry.value, copy('nodes.unknownID', {id: 'deleted-'}));
  scope.value = 'group'; scope.dispatchEvent(new Event('change'));
  assert.equal(scopeID.hidden, false); assert.equal(scopeID.entry.hidden, true); assert.equal(scopeID.value, '');
  assert.deepEqual(scopeID.options.map((option) => option.value), ['', 'g']);
  assert.equal(fields[1].querySelector('label').getAttribute('for'), scopeID.id);
  scope.value = 'all'; scope.dispatchEvent(new Event('change')); assert.equal(scopeID.disabled, true); assert.equal(scopeID.required, false);
  scope.value = 'node'; scope.dispatchEvent(new Event('change'));
  assert.equal(scopeID.value, 'deleted-node'); assert.equal(scopeID.disabled, false); assert.equal(scopeID.required, true);
});
