import test from 'node:test';
import assert from 'node:assert/strict';

import {installDOM} from './console-dom.js';

test('quota conversion uses exact decimal BigInt arithmetic and uint64 bounds', async (t) => {
  const dom = installDOM();
  t.after(() => dom.restore());
  const {quotaToBytes} = await import('../src/console/pages/node-traffic-settings.js');
  assert.deepEqual(quotaToBytes('9', 'GB'), {ok: true, value: '9000000000'});
  assert.deepEqual(quotaToBytes('0', 'TB'), {ok: true, value: '0'});
  assert.equal(quotaToBytes('1', 'KB').value, '1000');
  assert.equal(quotaToBytes('1', 'MB').value, '1000000');
  assert.equal(quotaToBytes('1', 'TB').value, '1000000000000');
  assert.equal(quotaToBytes('18446744073709551', 'KB').ok, true);
  assert.equal(quotaToBytes('18446744073709552', 'KB').ok, false);
  assert.equal(quotaToBytes('9007199254740993', 'bytes').value, '9007199254740993');
  assert.equal(quotaToBytes(Number.MAX_SAFE_INTEGER + 1, 'bytes').ok, false);
  assert.equal(quotaToBytes('01', 'bytes').ok, true);
  assert.equal(quotaToBytes('1', 'constructor').ok, false);
  for (const value of ['', '-1', '1.5', '1e3', null, undefined]) assert.equal(quotaToBytes(value, 'bytes').ok, false);
});


test('billing mode select submits and discard restores the saved mode', async (t) => {
  const dom = installDOM();
  const previousFetch = globalThis.fetch;
  const {copy, elements, flush} = await import('./console-dom.js');
  const {setNavigationSignal, setSession} = await import('../src/console/api.js');
  const {trafficSettingsPanel} = await import('../src/console/pages/node-traffic-settings.js');
  setNavigationSignal(null);
  setSession({csrf_token: 'csrf', user: {id: '00000000-0000-4000-8000-000000000001', username: 'admin', must_change_password: false}, expires_at: ''});
  let payload;
  let saved;
  globalThis.fetch = async (_url, options) => {
    payload = JSON.parse(options.body);
    return new Response(JSON.stringify(payload), {headers: {'Content-Type': 'application/json'}});
  };
  t.after(() => {globalThis.fetch = previousFetch; setSession({user: null, csrf_token: '', expires_at: ''}); dom.restore();});
  const root = trafficSettingsPanel('node', {billing_day: 1, quota_bytes: '1000', billing_mode: 'total'}, async (value) => {saved = value;});
  const toasts = dom.document.createElement('div');
  toasts.id = 'toasts';
  dom.body.append(toasts, root);
  const form = root.querySelector('form');
  const mode = elements(root).find((element) => element.name === 'billing_mode');
  assert.deepEqual(mode.options.map((option) => option.value), ['total', 'max', 'rx', 'tx']);
  assert.equal(mode.options[1].textContent, copy('nodes.billingMode.max'));
  mode.value = 'max';
  form.dispatchEvent(new Event('input'));
  form.dispatchEvent(new Event('submit', {cancelable: true}));
  await flush();
  assert.deepEqual(payload, {billing_mode: 'max', billing_day: 1, quota_bytes: '1000'});
  assert.deepEqual(saved, payload);
  mode.value = 'rx';
  form.dispatchEvent(new Event('input'));
  elements(root).find((element) => element.tagName === 'button' && element.textContent === copy('controls.discard')).click();
  assert.equal(mode.value, 'max');
});
