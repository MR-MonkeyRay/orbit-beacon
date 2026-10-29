import test from 'node:test';
import assert from 'node:assert/strict';
import {elements, flush, installDOM} from './console-dom.js';

test('billing success records its normalized baseline while Notes and Public name remain dirty', async () => {
  const dom = installDOM();
  const originals = new Map(['fetch', 'setTimeout', 'clearTimeout'].map((key) => [key, globalThis[key]]));
  const toastHost = document.createElement('div');
  toastHost.id = 'toasts';
  dom.body.append(toastHost);
  globalThis.setTimeout = () => 0;
  globalThis.clearTimeout = () => {};
  const requests = [];
  globalThis.fetch = async (path, options) => {
    requests.push({path, options});
    return new Response(JSON.stringify({billing_day: 2, quota_bytes: '1000'}));
  };
  try {
    const {h} = await import('../src/shared/dom.js');
    const {input, saveBar, trackDirty} = await import('../src/console/ui/forms.js');
    const {trafficSettingsPanel} = await import('../src/console/pages/node-traffic-settings.js');
    const section = (name, value) => {
      const control = input({name, value});
      const controls = saveBar('Save');
      const form = h('form', null, control, controls.bar);
      trackDirty(form, controls);
      dom.body.append(form);
      return {control, controls, form};
    };
    let trafficRefreshes = 0;
    const panel = trafficSettingsPanel('node-1', {billing_day: 1, quota_bytes: '0'}, () => { trafficRefreshes += 1; });
    dom.body.append(panel);
    const billing = panel.querySelector('form');
    const quota = elements(billing).find((element) => element.name === 'quota_value');
    const unit = elements(billing).find((element) => element.name === 'quota_unit');
    elements(billing).find((element) => element.name === 'billing_day').value = '2';
    quota.value = '0001';
    unit.value = 'KB';
    unit.dispatchEvent(new Event('change'));
    billing.dispatchEvent(new Event('input'));
    const notes = section('notes', 'Saved notes');
    const publicName = section('public_name', 'Saved public name');
    for (const [section, value] of [[notes, 'Unsaved notes'], [publicName, 'Unsaved public name']]) {
      section.control.value = value;
      section.form.dispatchEvent(new Event('input'));
      assert.equal(section.form.dataset.dirty, 'true');
    }
    notes.control.focus();
    billing.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.equal(requests.length, 1);
    assert.equal(requests[0].path, '/api/v1/nodes/node-1/traffic-settings');
    assert.deepEqual(JSON.parse(requests[0].options.body), {billing_day: 2, quota_bytes: '1000'});
    assert.equal(trafficRefreshes, 1);
    assert.equal(billing.dataset.dirty, 'false');
    assert.equal(billing.querySelector('#billing-save').disabled, true);
    assert.equal(billing.querySelector('.form-actions').querySelectorAll('button').find((button) => button !== billing.querySelector('#billing-save')).hidden, true);
    assert.equal(billing.querySelector('.dirty-note').classList.contains('on'), false);
    for (const section of [notes, publicName]) {
      assert.equal(section.form.dataset.dirty, 'true');
      assert.equal(section.controls.save.disabled, false);
    }
    assert.equal(notes.control.value, 'Unsaved notes');
    assert.equal(publicName.control.value, 'Unsaved public name');
    assert.equal(dom.document.activeElement, notes.control);
    const savedQuotaDisplay = quota.value;
    quota.value = '2000';
    billing.dispatchEvent(new Event('input'));
    assert.equal(billing.dataset.dirty, 'true');
    quota.value = savedQuotaDisplay;
    billing.dispatchEvent(new Event('input'));
    assert.equal(billing.dataset.dirty, 'false');
  } finally {
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    dom.restore();
  }
});

test('a detached submitted form cannot publish late private errors into the next page', async () => {
  const dom = installDOM();
  const toasts = document.createElement('div'); toasts.id = 'toasts'; dom.body.append(toasts);
  const originalTimeout = globalThis.setTimeout;
  globalThis.setTimeout = () => 0;
  try {
    const {onSubmit} = await import('../src/console/ui/forms.js');
    for (const inline of [false, true]) {
      const form = document.createElement('form'); const submit = document.createElement('button');
      const errorBox = inline ? document.createElement('div') : undefined;
      if (errorBox) { errorBox.hidden = true; form.append(errorBox); }
      form.append(submit); dom.body.append(form);
      let reject;
      onSubmit(form, submit, () => new Promise((_resolve, fail) => { reject = fail; }), errorBox);
      form.dispatchEvent(new Event('submit', {cancelable: true}));
      form.remove();
      reject(new Error('Private old-session error'));
      await flush();
      assert.equal(toasts.textContent, '');
      if (errorBox) { assert.equal(errorBox.textContent, ''); assert.equal(errorBox.hidden, true); }
    }
    const form = document.createElement('form'); const submit = document.createElement('button'); const errorBox = document.createElement('div');
    form.append(submit, errorBox); dom.body.append(form);
    const error = new Error('Current form error');
    onSubmit(form, submit, async () => { throw error; }, errorBox);
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(errorBox.hidden, false);
    assert.ok(errorBox.textContent.length > 0);
    assert.equal(errorBox.textContent.includes(error.message), false);
  } finally { globalThis.setTimeout = originalTimeout; dom.restore(); }
});
