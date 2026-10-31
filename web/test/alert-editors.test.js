import test from 'node:test';
import assert from 'node:assert/strict';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {elements, flush, installDOM} from './console-dom.js';

const id = '00000000-0000-4000-8000-000000000001';
const admin = () => setSession({csrf_token: 'csrf', user: {id, username: 'admin-two', must_change_password: false}, expires_at: ''});
const change = (element, value) => { element.value = value; element.dispatchEvent(new Event('change')); };
const control = (root, name) => elements(root).find((element) => element.name === name);

test('rule editor toggles offline fields, scopes and channel routing for every administrator', async () => {
  const dom = installDOM();
  setNavigationSignal(null);
  try {
    // 前面的测试用最小 document 导入了 dialogs；复用同一模块。
    const {ruleEditor} = await import('../src/console/pages/alert-rules.js');
    const context = {groups: [{id, name: 'Group'}], nodes: [{id, name: 'Node'}], channels: [{id, name: 'Hook', enabled: true}], signal: new AbortController().signal, saved: async () => {}};
    admin();
    const dialog = ruleEditor(null, context);
    const root = dom.body;
    const metric = control(root, 'metric');
    change(metric, 'offline');
    assert.equal(control(root, 'threshold').disabled, true);
    assert.equal(control(root, 'threshold').required, false);
    assert.equal(control(root, 'comparator').disabled, true);
    assert.equal(control(root, 'for_seconds').min, 3);
    change(metric, 'load1');
    assert.equal(control(root, 'threshold').disabled, false);
    assert.equal(control(root, 'threshold').max, 1000000);
    change(control(root, 'scope_kind'), 'group');
    assert.equal(control(root, 'scope_id').disabled, false);
    assert.match(control(root, 'scope_id').textContent, /Group/);
    change(control(root, 'scope_kind'), 'all');
    assert.equal(control(root, 'scope_id').disabled, true);
    dialog.close('done');
    await dialog.closed;
    const existing = ruleEditor({id, name: 'Offline', enabled: false, metric: 'offline', comparator: null, threshold: null, for_seconds: 180, scope_kind: 'node', scope_id: id, severity: 'critical', channel_ids: [id]}, context);
    assert.equal(control(root, 'channel_ids').selectedOptions.length, 1);
    assert.equal(control(root, 'scope_id').value, id);
    assert.equal(control(root, 'enabled').checked, false);
    existing.close('done'); await existing.closed;
    setSession({user: null, csrf_token: '', expires_at: ''});
    assert.equal(ruleEditor(null, context), undefined);
    admin();
    const aborted = new AbortController();
    aborted.abort();
    assert.equal(ruleEditor(null, {...context, signal: aborted.signal}), undefined);
  } finally { dom.restore(); }
});
test('channel editor starts with redacted URL and no secret, supports replacement, and maps config validation', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  setNavigationSignal(null);
  admin();
  let payload;
  globalThis.fetch = async (_path, options) => {
    payload = JSON.parse(options.body);
    return new Response(JSON.stringify({code: 'validation_failed', message: 'Invalid channel', fields: {config: 'Check the webhook destination.'}}), {status: 400});
  };
  try {
    const {channelEditor, channelsPanel} = await import('../src/console/pages/notification-channels.js');
    const item = {id, name: 'Hook', kind: 'webhook', enabled: true, secret_set: true, config: {url_set: true, url_redacted: 'https://example.com/…'}};
    const context = {signal: new AbortController().signal, saved: async () => {}};
    const dialog = channelEditor(item, context);
    const root = dom.body;
    assert.equal(control(root, 'url').value, '');
    assert.equal(control(root, 'secret').value, '');
    assert.equal(control(root, 'url').disabled, true);
    assert.equal(control(root, 'secret').disabled, true);
    assert.match(root.textContent, /https:\/\/example.com\/…/);
    const form = elements(root).find((element) => element.tagName === 'form');
    form.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.deepEqual(payload.config, {});
    assert.ok(!Object.hasOwn(payload, 'secret'));
    const configGroup = elements(root).find((element) => element.dataset.field === 'config');
    const configError = configGroup.children.find((element) => element.classList?.contains('field-error'));
    assert.equal(configError.hidden, false);
    assert.equal(configError.getAttribute('role'), 'alert');
    assert.ok(configError.textContent);
    assert.equal(dom.document.activeElement, control(root, 'replace_url'));
    control(root, 'replace_url').checked = true;
    control(root, 'replace_url').dispatchEvent(new Event('change'));
    assert.equal(control(root, 'url').disabled, false);
    assert.equal(control(root, 'url').required, true);
    control(root, 'url').value = 'https://example.com/repaired';
    globalThis.fetch = async () => new Response(JSON.stringify({code: 'secret_unreadable', message: 'Unreadable'}), {status: 409});
    form.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.ok(elements(root).some((element) => element.getAttribute('role') === 'alert' && !element.hidden && element.textContent));
    dialog.close('done');
    await dialog.closed;
    const panel = channelsPanel({...context, channels: [item]});
    assert.ok(elements(panel).some((element) => element.tagName === 'button' && element.textContent === 'Send test'));
    setSession({user: null, csrf_token: '', expires_at: ''});
    assert.equal(channelEditor(item, context), undefined);
    admin();
    const aborted = new AbortController();
    aborted.abort();
    assert.equal(channelEditor(item, {...context, signal: aborted.signal}), undefined);
  } finally { globalThis.fetch = originalFetch; dom.restore(); }
});

test('channel test results update accessible live status after failures and success', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  setNavigationSignal(null);
  admin();
  const controller = new AbortController();
  const replies = [
    new Response(JSON.stringify({ok: false, error_code: 'starttls_required'})),
    new Response(JSON.stringify({code: 'secret_unreadable', message: 'Unreadable'}), {status: 409}),
    new Response(JSON.stringify({ok: true, error_code: ''}))
  ];
  globalThis.fetch = async () => replies.shift();
  try {
    const {channelsPanel} = await import('../src/console/pages/notification-channels.js');
    const root = channelsPanel({channels: [{id, name: 'Mail', kind: 'email', enabled: true, secret_set: true, config: {host: 'smtp.example.com', port: 587, to: ['a@example.com']}}], signal: controller.signal, saved: async () => {}});
    const send = elements(root).find((element) => element.tagName === 'button' && element.textContent === 'Send test');
    const status = elements(root).find((element) => element.getAttribute('role') === 'status');
    send.click(); await flush();
    const rejected = status.textContent;
    assert.match(rejected, /STARTTLS/);
    send.click(); await flush();
    const unreadable = status.textContent;
    assert.ok(unreadable);
    assert.notEqual(unreadable, rejected);
    send.click(); await flush();
    assert.ok(status.textContent);
    assert.notEqual(status.textContent, unreadable);
  } finally { controller.abort(); globalThis.fetch = originalFetch; dom.restore(); }
});
