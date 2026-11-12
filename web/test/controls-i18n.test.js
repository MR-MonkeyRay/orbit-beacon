import test from 'node:test';
import assert from 'node:assert/strict';
import {flush, installDOM} from './console-dom.js';

test('locale toggle preserves drafts on cancel or storage failure, and disables during confirmation or page work', async () => {
  const dom = installDOM();
  let stored = 'en';
  const writes = [];
  localStorage.getItem = () => stored;
  localStorage.setItem = (key, value) => { writes.push([key, value]); stored = value; };
  try {
    const {localeToggle} = await import('../src/shared/locale-select.js');
    const secret = document.createElement('input'); secret.value = 'unsaved secret'; dom.body.append(secret);
    let reloads = 0;
    let resolve;
    const errors = [];
    const toggle = localeToggle({beforeChange: () => new Promise((done) => { resolve = done; }), reload: () => reloads++, onError: (error) => errors.push(error)});
    dom.body.append(toggle);
    assert.equal(toggle.className, 'icon-button');
    assert.equal(toggle.title, 'Switch to 中文');
    assert.equal(toggle.getAttribute('aria-label'), toggle.title);
    assert.ok(toggle.querySelector('svg'));
    toggle.click(); toggle.click();
    assert.equal(toggle.disabled, true);
    resolve(false); await flush();
    assert.equal(toggle.disabled, false);
    assert.equal(secret.value, 'unsaved secret');
    assert.deepEqual(writes, []); assert.equal(reloads, 0);
    const form = document.createElement('form'); form.dataset.busy = 'true'; dom.body.append(form);
    dom.mutations(); assert.equal(toggle.disabled, true);
    toggle.click(); assert.deepEqual(writes, []);
    delete form.dataset.busy; dom.mutations(); assert.equal(toggle.disabled, false);
    localStorage.setItem = () => { throw new Error('private storage internals'); };
    toggle.click(); resolve(true); await flush();
    assert.equal(toggle.title, 'Switch to 中文');
    assert.equal(secret.value, 'unsaved secret'); assert.equal(reloads, 0);
    assert.equal(errors.length, 1); assert.equal(errors[0].code, 'locale_storage_failed');
    assert.equal(toggle.disabled, false);
  } finally { dom.restore(); }
});

for (const [preference, language, expected] of [['en', 'zh-CN', 'zh-CN'], ['zh-CN', 'en', 'en'], ['system', 'en', 'zh-CN'], ['system', 'zh-CN', 'en']]) {
  test(`locale toggle flips ${preference} with browser language ${language} to ${expected}`, async () => {
    const dom = installDOM();
    const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
    Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {languages: [language]}});
    let stored = preference;
    localStorage.getItem = () => stored;
    const events = [];
    localStorage.setItem = (key, value) => { stored = value; events.push([key, value]); };
    try {
      const {localeToggle} = await import('../src/shared/locale-select.js');
      const toggle = localeToggle({beforeChange: async () => { events.push('confirm'); return true; }, reload: () => events.push('reload')});
      const label = toggle.getAttribute('aria-label');
      assert.ok(label.endsWith(expected === 'en' ? 'English' : '中文'));
      toggle.click(); await flush();
      assert.deepEqual(events, ['confirm', ['orbitbeacon-locale', expected], 'reload']);
    } finally {
      if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor); else delete globalThis.navigator;
      dom.restore();
    }
  });
}

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

test('shared locale change path confirms before writing and reloads only on success', async () => {
  const dom = installDOM();
  try {
    const {changeLocalePreference} = await import('../src/shared/locale-select.js');
    const events = [];
    localStorage.setItem = (key, value) => events.push([key, value]);
    const hooks = {beforeChange: async () => { events.push('confirm'); return true; }, reload: () => events.push('reload')};
    assert.equal(await changeLocalePreference('en', {...hooks, beforeChange: async () => false}), false);
    assert.deepEqual(events, []);
    assert.equal(await changeLocalePreference('en', hooks), true);
    assert.deepEqual(events, ['confirm', ['orbitbeacon-locale', 'en'], 'reload']);
  } finally { dom.restore(); }
});

test('password forms use the short hint without changing UTF-8 byte validation', async () => {
  const dom = installDOM();
  const previousWindow = globalThis.window;
  globalThis.window = new EventTarget();
  try {
    const {t} = await import('../src/console/i18n.js');
    const {passwordChangeForm} = await import('../src/console/ui/password-form.js');
    const {form} = passwordChangeForm({hint: t('account.passwordBytes')});
    dom.body.append(form);
    assert.ok(form.textContent.includes('At least 8 characters'));
    const password = form.querySelectorAll('input').find((input) => input.name === 'new_password');
    password.value = 'short'; password.dispatchEvent(new Event('input'));
    assert.equal(password.validationMessage, t('controls.validation.bytesMinimum', {label: t('account.newPassword'), min: 8}));
    password.value = '密码字'; password.dispatchEvent(new Event('input'));
    assert.equal(password.validationMessage, '', 'three three-byte characters retain the existing byte contract');
    password.value = 'x'.repeat(1025); password.dispatchEvent(new Event('input'));
    assert.equal(password.validationMessage, t('controls.validation.bytesMaximum', {label: t('account.newPassword'), max: 1024, size: 1025}));
  } finally { if (previousWindow === undefined) delete globalThis.window; else globalThis.window = previousWindow; dom.restore(); }
});
