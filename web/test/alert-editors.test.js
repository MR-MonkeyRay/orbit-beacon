import test from 'node:test';
import assert from 'node:assert/strict';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, elements, flush, installDOM} from './console-dom.js';

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
    assert.equal(metric.options.find((option) => option.value === 'traffic_quota_percent').textContent, copy('alerts.metric.trafficQuota'));
    change(metric, 'traffic_quota_percent');
    assert.equal(control(root, 'threshold').max, 1000);
    assert.ok(root.textContent.includes(copy('alerts.rules.thresholdPercent')));
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
    const canceled = ruleEditor(null, context);
    elements(root).find((element) => element.tagName === 'button' && element.textContent === copy('controls.cancel')).click();
    await canceled.closed;
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
    assert.ok(elements(panel).some((element) => element.tagName === 'button' && element.textContent === copy('alerts.channels.sendTest')));
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
    const send = elements(root).find((element) => element.tagName === 'button' && element.textContent === copy('alerts.channels.sendTest'));
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

test('channel editor shows templates for Telegram and email, prefills and saves or clears them', {timeout: 10000}, async () => {
  const dom = installDOM();
  const toasts = dom.document.createElement('div'); toasts.id = 'toasts'; dom.body.append(toasts);
  const originalFetch = globalThis.fetch;
  setNavigationSignal(null);
  admin();
  const payloads = [];
  let saves = 0;
  globalThis.fetch = async (_path, options) => {
    payloads.push(JSON.parse(options.body));
    return new Response('{}');
  };
  try {
    const {channelEditor} = await import('../src/console/pages/notification-channels.js');
    const context = {signal: new AbortController().signal, saved: async () => { saves += 1; }};
    const dialog = channelEditor(null, context);
    const root = dom.body;
    const template = control(root, 'template');
    const wrapper = elements(root).find((element) => element.children.includes(template));
    assert.equal(template.tagName, 'textarea');
    assert.equal(template.disabled, true);
    assert.equal(wrapper.hidden, true);
    assert.equal(wrapper.querySelector('label').textContent, copy('alerts.channels.template'));
    assert.equal(wrapper.querySelector('.hint').textContent, '{state} {rule} {node} {metric} {severity} {value} {started_at} {resolved_at} {event_id}');
    change(control(root, 'kind'), 'telegram');
    assert.equal(template.disabled, false);
    assert.equal(wrapper.hidden, false);
    template.value = '{unknown}';
    control(root, 'name').value = 'Bot';
    control(root, 'chat_id').value = '-123';
    control(root, 'secret').value = '123:token';
    const form = elements(root).find((element) => element.tagName === 'form');
    form.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.equal(payloads.length, 0);
    assert.equal(wrapper.querySelector('.field-error').textContent, copy('alerts.validation.template'));
    template.value = '{node}';
    change(control(root, 'kind'), 'email');
    assert.equal(template.disabled, false);
    assert.equal(wrapper.hidden, false);
    change(control(root, 'kind'), 'webhook');
    assert.equal(template.disabled, true);
    assert.equal(wrapper.hidden, true);
    control(root, 'url').value = 'https://example.com/hook';
    form.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.equal(payloads.length, 1, root.textContent);
    assert.equal(saves, 1, root.textContent);
    await dialog.closed;
    assert.deepEqual(payloads[0].config, {url: 'https://example.com/hook'});
    for (const kind of ['telegram', 'email']) {
      const item = {id, name: 'Channel', kind, enabled: true, secret_set: true, config: kind === 'telegram' ? {chat_id: '-123', template: '{node}\n{rule}'} : {host: 'smtp.example.com', port: 587, from: 'a@example.com', to: ['b@example.com'], template: '{node}\n{rule}'}};
      for (const value of [' \r\n{state}\r\n{node} ', ' \t\r\n ']) {
        const edited = channelEditor(item, context);
        assert.equal(control(root, 'template').value, item.config.template);
        assert.equal(control(root, 'template').disabled, false);
        control(root, 'template').value = value;
        elements(root).find((element) => element.tagName === 'form').dispatchEvent(new Event('submit', {cancelable: true}));
        await flush();
        assert.equal(payloads.length, saves, root.textContent);
        assert.ok(!elements(root).some((element) => element.getAttribute('role') === 'alert' && !element.hidden), root.textContent);
        await edited.closed;
        const payload = payloads.at(-1);
        assert.equal(payload.config.template, value.includes('{state}') ? '{state}\n{node}' : '');
        assert.ok(!Object.hasOwn(payload, 'kind'));
        assert.ok(!Object.hasOwn(payload, 'secret'));
        assert.ok(!Object.hasOwn(payload.config, 'url'));
      }
    }
    assert.equal(saves, 5);
  } finally { globalThis.fetch = originalFetch; dom.restore(); }
});

test('new channel editors switch URL and secret requirements and submit URL templates', {timeout: 10000}, async () => {
  const dom = installDOM();
  const toasts = dom.document.createElement('div'); toasts.id = 'toasts'; dom.body.append(toasts);
  const originalFetch = globalThis.fetch;
  setNavigationSignal(null); admin();
  const payloads = [];
  globalThis.fetch = async (_path, options) => { payloads.push(JSON.parse(options.body)); return new Response('{}'); };
  try {
    const {channelEditor, channelsPanel} = await import('../src/console/pages/notification-channels.js');
    const context = {signal: new AbortController().signal, saved: async () => {}};
    const root = dom.body;
    const targets = {
      feishu: 'https://open.feishu.cn/open-apis/bot/v2/hook/token',
      dingtalk: 'https://oapi.dingtalk.com/robot/send?access_token=token',
      wecom: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=token',
      bark: 'https://bark.example', ntfy: 'https://ntfy.example/topic'
    };
    const dialog = channelEditor(null, context);
    const labels = {feishu: 'alerts.channels.optionalSecret', dingtalk: 'alerts.channels.optionalSecret', bark: 'alerts.channels.deviceKey', ntfy: 'alerts.channels.accessToken'};
    for (const kind of Object.keys(targets)) {
      change(control(root, 'kind'), kind);
      assert.equal(control(root, 'url').disabled, false);
      assert.equal(control(root, 'url').required, true);
      assert.ok(control(root, 'url').placeholder.startsWith('https://'));
      assert.equal(control(root, 'template').disabled, false);
      const secret = control(root, 'secret');
      const secretField = elements(root).find((element) => element.classList?.contains('field') && element.querySelector?.('label')?.getAttribute('for') === secret.id);
      assert.equal(secret.disabled, kind === 'wecom');
      assert.equal(secret.required, kind === 'bark');
      if (kind === 'wecom') assert.equal(secretField.hidden, true);
      else assert.equal(secretField.querySelector('label').textContent, copy(labels[kind]));
    }
    change(control(root, 'kind'), 'telegram');
    assert.equal(control(root, 'url').disabled, true);
    dialog.close('done'); await dialog.closed;
    for (const [kind, url] of Object.entries(targets)) {
      const created = channelEditor(null, context);
      change(control(root, 'kind'), kind);
      control(root, 'name').value = 'Channel';
      control(root, 'url').value = url;
      control(root, 'template').value = ' {node}\r\n{rule} ';
      if (kind === 'bark') control(root, 'secret').value = 'device-key';
      elements(root).find((element) => element.tagName === 'form').dispatchEvent(new Event('submit', {cancelable: true}));
      await flush();
      assert.equal(payloads.at(-1)?.kind, kind, root.textContent);
      await created.closed;
      assert.deepEqual(payloads.at(-1), {name: 'Channel', kind, enabled: true, config: {url, template: '{node}\n{rule}'}, ...(kind === 'bark' ? {secret: 'device-key'} : {})});
      const item = {id, name: 'Channel', kind, enabled: true, secret_set: kind === 'bark', config: {url_set: true, url_redacted: new URL(url).origin + '/…', template: '{node}'}};
      const edited = channelEditor(item, context);
      assert.equal(control(root, 'url').disabled, true);
      assert.equal(control(root, 'template').value, '{node}');
      if (kind === 'wecom') assert.equal(control(root, 'replace_secret').parentElement.hidden, true);
      control(root, 'template').value = '{rule}';
      elements(root).find((element) => element.tagName === 'form').dispatchEvent(new Event('submit', {cancelable: true}));
      await flush();
      assert.deepEqual(payloads.at(-1), {name: 'Channel', enabled: true, config: {template: '{rule}'}}, root.textContent);
      await edited.closed;
      assert.deepEqual(payloads.at(-1), {name: 'Channel', enabled: true, config: {template: '{rule}'}});
      const panel = channelsPanel({...context, channels: [item]});
      assert.ok(panel.textContent.includes(item.config.url_redacted));
    }
  } finally { globalThis.fetch = originalFetch; dom.restore(); }
});
