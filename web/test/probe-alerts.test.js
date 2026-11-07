import test from 'node:test';
import assert from 'node:assert/strict';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {formatMetricValue, METRICS, metricMax, validateRule, MESSAGE_TEMPLATE_TOKENS} from '../src/console/lib/alert-utils.js';
import {formatMilliseconds} from '../src/console/format.js';
import {fieldErrorText} from '../src/shared/errors.js';
import {rulesPanel} from '../src/console/pages/alert-rules.js';
import {eventRows, activeAlertsSummary} from '../src/console/pages/alert-events.js';
import {copy, elements, flush, installDOM} from './console-dom.js';

const id = '00000000-0000-4000-8000-000000000001';
const target = 'https://example.test/health';
const metrics = [['probe_failed', 'probeFailed', '>', 0, 0], ['probe_latency_ms', 'probeLatency', '>', 250, 60000], ['probe_tls_expiry_days', 'probeTLSExpiry', '<', 150, 3650]];
const rule = (metric = 'probe_failed', comparator = '>', threshold = 0) => ({id, name: 'Probe', enabled: true, scope_kind: 'all', scope_id: null, metric, target, comparator, threshold, for_seconds: 0, severity: 'warning', channel_ids: []});
const control = (root, name) => elements(root).find((element) => element.name === name);
const change = (element, value) => { element.value = value; element.dispatchEvent(new Event('change')); };

test('probe alert options, units, bounds, validation errors and template variable', () => {
  for (const [metric, key, comparator, threshold, max] of metrics) {
    assert.equal(METRICS.find(([id]) => id === metric)[1], copy(`alerts.metric.${key}`));
    assert.equal(metricMax(metric), max);
    assert.deepEqual(validateRule(rule(metric, comparator, threshold)), {});
    assert.equal(validateRule({...rule(metric, comparator, threshold), target: null}).target, copy('alerts.validation.probeTarget'));
    assert.equal(validateRule({...rule(metric, comparator, threshold), target: ''}).target, copy('alerts.validation.probeTarget'));
    for (const value of [-1, max + 1, NaN, Infinity, null]) assert.ok(validateRule(rule(metric, comparator, value)).threshold);
  }
  assert.ok(validateRule(rule('probe_failed', '<', 0)).comparator);
  assert.ok(validateRule(rule('probe_tls_expiry_days', '>', 30)).comparator);
  for (const metric of ['probe_latency_ms', 'probe_tls_expiry_days']) {
    assert.deepEqual(validateRule(rule(metric, '<', .5)), {});
    assert.ok(validateRule(rule(metric, '<', 0)).threshold);
  }
  assert.equal(formatMetricValue('probe_failed', 0), copy('nodes.probe.up'));
  assert.equal(formatMetricValue('probe_failed', 1), copy('nodes.probe.down'));
  assert.equal(formatMetricValue('probe_latency_ms', 250.5), formatMilliseconds(250.5));
  assert.equal(formatMetricValue('probe_tls_expiry_days', 150.5), copy('alerts.value.expiryDays', {value: '150.5'}));
  for (const reason of ['must be a valid probe target', 'must be null for this metric']) assert.equal(fieldErrorText(reason), copy('alerts.validation.probeTarget'));
  assert.equal(fieldErrorText('must be >'), copy('fields.comparator'));
  assert.equal(fieldErrorText('must be <'), copy('fields.comparator'));
  assert.equal(fieldErrorText('must be greater than zero'), copy('fields.threshold'));
  assert.ok(MESSAGE_TEMPLATE_TOKENS.includes('{target}'));
});

test('probe rule editor shows target, submits fixed conditions, restores target and clears it for other metrics', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  setNavigationSignal(null);
  setSession({csrf_token: 'csrf', user: {id, username: 'admin', must_change_password: false}, expires_at: ''});
  let payload;
  globalThis.fetch = async (_url, options) => { payload = JSON.parse(options.body); return new Response(JSON.stringify(rule())); };
  try {
    const {ruleEditor} = await import('../src/console/pages/alert-rules.js');
    const context = {groups: [], nodes: [], channels: [], saved: async () => {}};
    for (const [metric, , comparator, threshold] of metrics) {
      const dialog = ruleEditor(null, context);
      const root = dom.body;
      const targetInput = control(root, 'target');
      assert.equal(targetInput.parentElement.hidden, true);
      assert.equal(targetInput.required, false);
      change(control(root, 'metric'), metric);
      assert.equal(targetInput.parentElement.hidden, false);
      assert.equal(targetInput.required, true);
      targetInput.value = ` ${target} `;
      control(root, 'name').value = 'Probe';
      control(root, 'threshold').value = String(threshold);
      assert.equal(control(root, 'comparator').disabled, metric !== 'probe_latency_ms');
      if (metric === 'probe_failed') {
        assert.equal(control(root, 'threshold').disabled, true);
        assert.equal(control(root, 'threshold').parentElement.parentElement.hidden, true);
      }
      if (metric === 'probe_latency_ms') assert.equal(control(root, 'threshold').parentElement.querySelector('label').textContent, copy('alerts.rules.thresholdMilliseconds'));
      if (metric === 'probe_tls_expiry_days') assert.equal(control(root, 'threshold').parentElement.querySelector('label').textContent, copy('alerts.rules.thresholdDays'));
      elements(root).find((element) => element.tagName === 'form').dispatchEvent(new Event('submit', {cancelable: true}));
      await flush();
      assert.equal(payload.target, target);
      assert.equal(payload.metric, metric);
      assert.equal(payload.comparator, comparator);
      assert.equal(payload.threshold, threshold);
      dialog.close('done'); await dialog.closed;
      const kept = ruleEditor(null, context);
      control(root, 'threshold').value = '80';
      change(control(root, 'metric'), metric);
      change(control(root, 'metric'), 'cpu_percent');
      assert.equal(control(root, 'threshold').value, metric === 'probe_tls_expiry_days' ? '30' : '80');
      kept.close('done'); await kept.closed;
      const edited = ruleEditor(rule(metric, comparator, threshold), context);
      assert.equal(control(root, 'target').value, target);
      assert.equal(control(root, 'target').required, true);
      change(control(root, 'metric'), 'cpu_percent');
      assert.equal(control(root, 'target').parentElement.hidden, true);
      assert.equal(control(root, 'target').disabled, true);
      control(root, 'threshold').value = '80';
      elements(root).find((element) => element.tagName === 'form').dispatchEvent(new Event('submit', {cancelable: true}));
      await flush();
      assert.equal(payload.target, null);
      edited.close('done'); await edited.closed;
    }
  } finally { globalThis.fetch = originalFetch; setSession({user: null, csrf_token: '', expires_at: ''}); dom.restore(); }
});

test('probe targets appear as mono code in rule lists, event rows, summary and detail', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  try {
    const event = {...rule(), rule_name: 'Probe', node_id: id, node_name: 'Node', state: 'firing', value: 1, started_at: '2026-11-06T00:00:00Z', resolved_at: null};
    const assertTarget = (root) => {
      const code = elements(root).find((element) => element.tagName === 'code' && element.textContent === target);
      assert.ok(code);
      assert.ok(code.classList.contains('mono'));
    };
    const rules = rulesPanel({rules: [rule()], groups: [], nodes: [], channels: [], saved: async () => {}});
    assertTarget(rules);
    assert.ok(rules.textContent.includes(copy('alerts.metric.probeFailed')));
    assert.ok(!rules.textContent.includes(copy('nodes.probe.up')));
    assertTarget(eventRows([event])[0]);
    assertTarget(activeAlertsSummary({firing: {total: 1, warning: 1}, newest: [event]}));
    globalThis.fetch = async () => new Response(JSON.stringify(event));
    const {showAlert} = await import('../src/console/pages/alert-detail.js');
    showAlert(id);
    await flush();
    assertTarget(dom.body);
    assert.ok(dom.body.textContent.includes(copy('nodes.audit.target')));
    for (const dialog of elements(dom.body).filter((element) => element.tagName === 'dialog')) dialog.close('done');
  } finally { globalThis.fetch = originalFetch; dom.restore(); }
});
