import test from 'node:test';
import assert from 'node:assert/strict';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, elements, flush, installDOM} from './console-dom.js';

const id = '00000000-0000-4000-8000-000000000001';
const control = (root, name) => elements(root).find((element) => element.name === name);
const admin = () => { setNavigationSignal(null); setSession({csrf_token: 'csrf', user: {id, username: 'admin', must_change_password: false}, expires_at: ''}); };
const context = () => ({groups: [{id, name: 'Group'}], nodes: [{id, name: 'Node'}], signal: new AbortController().signal, saved: async () => {}});

test('silences list renders empty, scopes, active and scheduled states; delete cancellation sends no request', async () => {
  const dom = installDOM(), original = globalThis.fetch;
  admin();
  let requests = 0;
  globalThis.fetch = async () => { requests++; return new Response(null, {status: 204}); };
  try {
    const {silenceList} = await import('../src/console/pages/alert-silences.js');
    assert.ok(silenceList([], context()).textContent.includes(copy('alerts.silences.empty')));
    const entries = [true, false].map((active) => ({id, reason: `Reason ${active}`, scope_kind: 'group', scope_id: id, scope_name: 'Current Group', starts_at: '2030-01-01T00:00:00Z', ends_at: '2030-01-01T02:00:00Z', active}));
    const root = silenceList(entries, context()); dom.body.append(root);
    for (const key of ['title', 'reason', 'starts', 'ends', 'active', 'scheduled']) assert.ok(root.textContent.includes(copy(`alerts.silences.${key}`)));
    assert.ok(root.textContent.includes('Current Group'));
    const remove = elements(root).find((element) => element.getAttribute('aria-label') === copy('alerts.silences.delete'));
    remove.click(); await flush();
    elements(dom.body).find((element) => element.tagName === 'button' && element.textContent === copy('controls.cancel')).click();
    await flush(); await new Promise((resolve) => setTimeout(resolve, 280));
    assert.equal(requests, 0);
    remove.click(); await flush();
    elements(dom.body).find((element) => element.tagName === 'button' && element.textContent === copy('alerts.silences.delete')).click();
    await new Promise((resolve) => setTimeout(resolve, 280)); await flush();
    assert.equal(requests, 1);
  } finally { globalThis.fetch = original; dom.restore(); }
});

test('silence editor validates dates and submits browser local time as UTC', async () => {
  const dom = installDOM(), original = globalThis.fetch, originalTZ = process.env.TZ;
  process.env.TZ = 'America/Los_Angeles';
  admin();
  let payload, requests = 0;
  globalThis.fetch = async (_path, options) => { requests++; payload = JSON.parse(options.body); return new Response(JSON.stringify({id})); };
  try {
    const {silenceEditor} = await import('../src/console/pages/alert-silences.js');
    const {fieldErrorText} = await import('../src/shared/errors.js');
    assert.equal(fieldErrorText('must be after starts_at and now'), copy('alerts.silences.invalidEnd'));
    assert.equal(fieldErrorText('must be within 30 days of starts_at'), copy('alerts.silences.tooLong'));
    const dialog = silenceEditor(context());
    const root = dom.body, form = elements(root).find((element) => element.tagName === 'form');
    const starts = control(root, 'starts_at'), ends = control(root, 'ends_at'), reason = control(root, 'reason');
    assert.equal(new Date(ends.value) - new Date(starts.value), 2 * 3600000);
    assert.ok(Math.abs(new Date(starts.value) - Date.now()) < 2000);
    reason.value = ' maintenance ';
    starts.value = '2030-01-01T12:00:00'; ends.value = '2030-01-01T11:00:00';
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(requests, 0);
    assert.ok(root.textContent.includes(copy('alerts.silences.invalidEnd')));
    ends.value = '2030-02-02T12:00:00';
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(requests, 0);
    assert.ok(root.textContent.includes(copy('alerts.silences.tooLong')));
    ends.value = '2000-01-01T14:00:00';
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(requests, 0);
    assert.ok(root.textContent.includes(copy('alerts.silences.invalidEnd')));
    ends.value = '2030-01-01T14:00:00';
    reason.value = ' ';
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(requests, 0);
    assert.ok(root.textContent.includes(copy('controls.validation.bytesRequired', {label: copy('alerts.silences.reason')})));
    reason.value = ' maintenance ';
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(requests, 1);
    assert.equal(payload.starts_at, '2030-01-01T20:00:00.000Z');
    assert.equal(payload.ends_at, '2030-01-01T22:00:00.000Z');
    assert.equal(payload.reason, 'maintenance'); assert.equal(payload.scope_id, null);
    await dialog.closed;
  } finally { process.env.TZ === originalTZ || (originalTZ === undefined ? delete process.env.TZ : process.env.TZ = originalTZ); globalThis.fetch = original; dom.restore(); }
});

test('silenced event badge appears for firing and resolved history using existing badge style', async () => {
  const dom = installDOM();
  try {
    const {eventRows} = await import('../src/console/pages/alert-events.js');
    for (const state of ['firing', 'resolved']) {
      const event = {id, rule_id: id, node_id: id, state, severity: 'warning', metric: 'cpu_percent', value: 90, started_at: '2030-01-01T00:00:00Z'};
      assert.ok(!eventRows([event])[0].textContent.includes(copy('alerts.silences.silenced')));
      const row = eventRows([{...event, silenced: true}])[0];
      const badge = elements(row).find((element) => element.classList.contains('badge') && element.textContent === copy('alerts.silences.silenced'));
      assert.ok(badge); assert.ok(badge.classList.contains('disabled'));
    }
  } finally { dom.restore(); }
});

test('silence changes have audit labels', async () => {
  const {describeAction} = await import('../src/console/format.js');
  assert.equal(describeAction('alert_silence.create'), copy('audit.silenceCreate'));
  assert.equal(describeAction('alert_silence.delete'), copy('audit.silenceDelete'));
});
