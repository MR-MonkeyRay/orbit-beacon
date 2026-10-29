import test from 'node:test';
import assert from 'node:assert/strict';
import {flush, installDOM} from './console-dom.js';

test('locale selection preserves dirty content when cancelled and restores the preference on storage failure', async () => {
  const dom = installDOM();
  let stored = 'system';
  const writes = [];
  globalThis.localStorage = {getItem: () => stored, setItem: (key, value) => { writes.push([key, value]); stored = value; }};
  try {
    const {localeSelect} = await import('../src/shared/locale-select.js');
    const secret = document.createElement('input'); secret.value = 'unsaved secret'; dom.body.append(secret);
    let reloads = 0;
    let accept = false;
    const errors = [];
    const select = localeSelect({beforeChange: async () => accept, reload: () => { reloads += 1; }, onError: (error) => errors.push(error)});
    dom.body.append(select);
    select.value = 'en'; select.dispatchEvent(new Event('change')); await flush();
    assert.equal(select.value, 'system');
    assert.equal(secret.value, 'unsaved secret');
    assert.deepEqual(writes, []);
    assert.equal(reloads, 0);
    accept = true;
    localStorage.setItem = () => { throw new Error('private storage internals'); };
    select.value = 'zh-CN'; select.dispatchEvent(new Event('change')); await flush();
    assert.equal(select.value, 'system');
    assert.equal(secret.value, 'unsaved secret');
    assert.equal(reloads, 0);
    assert.equal(errors.length, 1);
    assert.equal(errors[0].code, 'locale_storage_failed');
    assert.equal(select.disabled, false);
  } finally { dom.restore(); }
});

test('locale selection persists an explicit preference even when its effective language is unchanged', async () => {
  const dom = installDOM();
  let stored = 'system';
  let writes = 0;
  localStorage.getItem = () => stored;
  localStorage.setItem = (_key, value) => { stored = value; writes += 1; };
  try {
    const {localeSelect} = await import('../src/shared/locale-select.js');
    let confirmations = 0;
    let reloads = 0;
    const select = localeSelect({beforeChange: async () => { confirmations += 1; return true; }, reload: () => { reloads += 1; }});
    select.value = 'system'; select.dispatchEvent(new Event('change')); await flush();
    assert.equal(confirmations, 0);
    select.value = 'en'; select.dispatchEvent(new Event('change')); await flush();
    assert.equal(stored, 'en');
    assert.equal(writes, 1);
    assert.equal(reloads, 1);
  } finally { dom.restore(); }
});

test('standalone dirty tracking observes changes and resets its saved baseline without a save bar', async () => {
  const dom = installDOM();
  try {
    const {trackFormDirty} = await import('../src/console/ui/forms.js');
    const form = document.createElement('form');
    const input = document.createElement('input'); input.name = 'password'; input.value = '';
    form.append(input); dom.body.append(form);
    const state = trackFormDirty(form);
    assert.equal(state.dirty, false);
    input.value = 'unsaved'; form.dispatchEvent(new Event('input'));
    assert.equal(state.dirty, true);
    assert.equal(form.dataset.dirty, 'true');
    input.value = ''; form.syncDirty();
    assert.equal(state.dirty, false);
    input.value = 'saved'; form.markClean();
    assert.equal(state.dirty, false);
    input.value = ''; form.dispatchEvent(new Event('change'));
    assert.equal(state.dirty, true);
  } finally { dom.restore(); }
});

test('field validation translates native validity rather than exposing the browser language', async () => {
  const dom = installDOM();
  try {
    const {field} = await import('../src/console/ui/forms.js');
    const {t} = await import('../src/shared/i18n.js');
    const input = document.createElement('input');
    Object.defineProperty(input, 'validity', {configurable: true, value: {valid: false, valueMissing: true}});
    input.validationMessage = 'browser-native message in another language';
    const wrapper = field('Name', input); dom.body.append(wrapper);
    input.dispatchEvent(new Event('invalid', {cancelable: true}));
    assert.equal(wrapper.querySelector('.field-error').textContent, t('controls.validation.required'));
    Object.defineProperty(input, 'validity', {value: {valid: false, customError: true}});
    input.validationMessage = 'Application-localized custom error';
    input.dispatchEvent(new Event('input'));
    assert.equal(wrapper.querySelector('.field-error').textContent, input.validationMessage);
  } finally { dom.restore(); }
});

test('dialog dirty state detects routing changes beyond the first selected channel', async () => {
  const dom = installDOM();
  try {
    const {modal, hasUnsavedDialogChanges} = await import('../src/console/ui/dialogs.js');
    const select = document.createElement('select'); select.multiple = true;
    const first = document.createElement('option'); first.value = 'first'; first.selected = true;
    const second = document.createElement('option'); second.value = 'second'; second.selected = false;
    select.append(first, second); select.value = 'first';
    const dialog = modal('Routing', () => select);
    assert.equal(hasUnsavedDialogChanges(), false);
    second.selected = true;
    assert.equal(select.value, 'first');
    assert.equal(hasUnsavedDialogChanges(), true);
    second.selected = false;
    assert.equal(hasUnsavedDialogChanges(), false);
    dialog.close('done');
    await dialog.closed;
  } finally { dom.restore(); }
});
