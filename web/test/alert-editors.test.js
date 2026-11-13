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
    for (const check of elements(root).filter((element) => element.type === 'checkbox')) {
      assert.equal(check.classList.contains('input'), false);
      assert.equal(check.parentElement.tagName, 'label');
      assert.equal(check.parentElement.className, 'check-row');
      assert.equal(check.parentElement.children[0], check);
    }
    assert.equal(control(root, 'for_seconds').max, 1440);
    assert.equal(control(root, 'duration_unit').value, '60');
    assert.equal(control(root, 'duration_seconds'), undefined);
    assert.equal(control(root, 'channel_ids').type, 'checkbox');
    assert.ok(!root.textContent.includes(copy('alerts.rules.routingHint')));
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
    assert.equal(elements(root).filter((element) => element.name === 'channel_ids' && element.checked).length, 1);
    assert.equal(control(root, 'scope_id').value, id);
    assert.equal(control(root, 'enabled').checked, false);
    existing.close('done'); await existing.closed;
    const withoutChannels = ruleEditor(null, {...context, channels: []});
    assert.ok(root.textContent.includes(copy('alerts.channels.add')));
    assert.equal(control(root, 'channel_ids'), undefined);
    withoutChannels.close('done'); await withoutChannels.closed;
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
    for (const check of elements(root).filter((element) => element.type === 'checkbox')) {
      assert.equal(check.classList.contains('input'), false);
      assert.equal(check.parentElement.tagName, 'label');
      assert.equal(check.parentElement.className, 'check-row');
      assert.equal(check.parentElement.children[0], check);
    }
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
    assert.equal(wrapper.querySelector('.text-note').textContent, '{state} {rule} {node} {metric} {target} {severity} {value} {started_at} {resolved_at} {event_id}');
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
      if (kind === 'wecom') assert.equal(control(root, 'replace_secret').parentElement.parentElement.hidden, true);
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

test('rule presets preserve routing and scope, preview updates, and duration units validate and round-trip', async () => {
  const dom = installDOM();
  const original = globalThis.fetch;
  setNavigationSignal(null); admin();
  const payloads = [];
  globalThis.fetch = async (_path, options) => { payloads.push(JSON.parse(options.body)); return new Response('{}'); };
  const toasts = document.createElement('div'); toasts.id = 'toasts'; dom.body.append(toasts);
  try {
    const {ruleEditor} = await import('../src/console/pages/alert-rules.js');
    const context = {groups: [{id, name: 'Production'}], nodes: [{id, name: 'Edge'}], channels: [{id, name: 'Hook', enabled: true}], signal: new AbortController().signal, saved: async () => {}};
    const dialog = ruleEditor(null, context);
    const root = dom.body;
    const preview = () => elements(root).find((e) => e.classList.contains('alert-rule-preview')).textContent;
    assert.equal(preview(), copy('alerts.rules.preview', {condition: copy('alerts.rules.metricCondition', {metric: copy('alerts.metric.cpu'), comparator: '>', value: '80%'}), duration: copy('alerts.rules.previewDuration.60', {count: 3}), scope: copy('alerts.scope.all'), severity: copy('alerts.severity.warning')}));
    control(root, 'threshold').value = '70'; control(root, 'threshold').dispatchEvent(new Event('input'));
    assert.match(preview(), /70%/);
    change(control(root, 'scope_kind'), 'group'); change(control(root, 'scope_id'), id);
    control(root, 'channel_ids').checked = true;
    for (const [label, metric, duration, severity] of [['offline', 'offline', 3, 'critical'], ['cpu', 'cpu_percent', 5, 'warning'], ['memory', 'memory_used_percent', 5, 'warning'], ['disk', 'disk_used_percent', 10, 'critical']]) {
      elements(root).find((e) => e.tagName === 'button' && e.textContent === copy(`alerts.rules.preset.${label}`)).click();
      assert.equal(control(root, 'metric').value, metric);
      assert.equal(Number(control(root, 'for_seconds').value), duration);
      assert.equal(control(root, 'duration_unit').value, '60');
      assert.equal(control(root, 'severity').value, severity);
      assert.equal(control(root, 'threshold').value, '90');
      assert.equal(control(root, 'comparator').value, '>');
      assert.equal(control(root, 'scope_kind').value, 'group'); assert.equal(control(root, 'scope_id').value, id);
      assert.equal(elements(root).filter((element) => element.name === 'channel_ids' && element.checked).length, 1);
      assert.equal(control(root, 'name').value, copy('alerts.rules.preset.offline'));
      assert.match(preview(), /Production/);
    }
    dialog.close('done'); await dialog.closed;
    const item = {id, name: 'Existing', enabled: true, metric: 'cpu_percent', comparator: '>', threshold: 80, for_seconds: 60, scope_kind: 'all', scope_id: null, severity: 'warning', channel_ids: []};
    for (const [seconds, unit, value] of [[0, '3600', 0], [65, '1', 65], [180, '60', 3], [3600, '3600', 1], [86400, '3600', 24]]) {
      const edited = ruleEditor({...item, for_seconds: seconds}, context);
      assert.equal(control(root, 'duration_unit').value, unit);
      assert.equal(Number(control(root, 'for_seconds').value), value);
      assert.equal(elements(root).filter((e) => e.tagName === 'button' && e.textContent === copy('alerts.rules.preset.cpu')).length, 0);
      elements(root).find((e) => e.tagName === 'form').dispatchEvent(new Event('submit', {cancelable: true})); await flush();
      assert.equal(payloads.at(-1).for_seconds, seconds, root.textContent);
      await edited.closed;
    }
    const offline = ruleEditor({...item, metric: 'offline', comparator: null, threshold: null, for_seconds: 180}, context);
    change(control(root, 'duration_unit'), '1');
    change(control(root, 'for_seconds'), '179');
    assert.equal(control(root, 'for_seconds').min, 180);
    const form = elements(root).find((e) => e.tagName === 'form');
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(payloads.length, 5);
    assert.ok(root.textContent.includes(copy('alerts.validation.offlineDuration')));
    change(control(root, 'duration_unit'), '3600');
    assert.equal(control(root, 'for_seconds').min, 0.05);
    control(root, 'for_seconds').value = '25';
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(payloads.length, 5);
    control(root, 'for_seconds').value = '0.05';
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(payloads.at(-1).for_seconds, 180);
    await offline.closed;
  } finally { globalThis.fetch = original; dom.restore(); }
});
