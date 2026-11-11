import test from 'node:test';
import assert from 'node:assert/strict';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {formatMetricValue, METRICS, metricMax, validateRule} from '../src/console/lib/alert-utils.js';
import {rulesPanel} from '../src/console/pages/alert-rules.js';
import {eventRows} from '../src/console/pages/alert-events.js';
import {copy, elements, flush, installDOM} from './console-dom.js';

const id = '00000000-0000-4000-8000-000000000001';
const control = (root, name) => elements(root).find((element) => element.name === name);
const change = (element, value) => { element.value = value; element.dispatchEvent(new Event('change')); };
const rule = () => ({id, name: 'Expiry', enabled: true, scope_kind: 'all', scope_id: null, metric: 'node_expiry_days', comparator: '<', threshold: 17, for_seconds: 60, severity: 'warning', channel_ids: []});

test('expiry metric follows quota, validates day boundaries and formats one decimal', () => {
  assert.equal(METRICS[METRICS.findIndex(([key]) => key === 'traffic_quota_percent') + 1][0], 'node_expiry_days');
  assert.equal(METRICS.find(([key]) => key === 'node_expiry_days')[1], copy('alerts.metric.nodeExpiry'));
  assert.equal(metricMax('node_expiry_days'), 365);
  for (const threshold of [1, 30, 365]) assert.deepEqual(validateRule({...rule(), threshold}), {});
  for (const threshold of [0, .5, 366, NaN, null]) assert.equal(validateRule({...rule(), threshold}).threshold, copy('alerts.validation.expiryThreshold'));
  assert.equal(validateRule({...rule(), comparator: '>'}).comparator, copy('alerts.validation.comparator'));
  for (const [value, formatted] of [[0, '0'], [30, '30'], [.25, '0.3'], [12.34, '12.3']]) {
    assert.equal(formatMetricValue('node_expiry_days', value), copy('alerts.value.expiryDays', {value: formatted}));
  }
});

test('expiry rule editor hides and restores comparator, defaults days and saves fixed comparator', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  setNavigationSignal(null);
  setSession({csrf_token: 'csrf', user: {id, username: 'admin', must_change_password: false}, expires_at: ''});
  let payload;
  globalThis.fetch = async (_url, options) => { payload = JSON.parse(options.body); return new Response(JSON.stringify(rule())); };
  try {
    const {ruleEditor} = await import('../src/console/pages/alert-rules.js');
    const context = {groups: [], nodes: [], channels: [], saved: async () => {}};
    const dialog = ruleEditor(null, context);
    const root = dom.body;
    const metric = control(root, 'metric'), comparator = control(root, 'comparator'), threshold = control(root, 'threshold');
    change(metric, 'node_expiry_days');
    assert.equal(comparator.parentElement.hidden, true);
    assert.equal(comparator.disabled, true);
    assert.equal(comparator.value, '<');
    assert.equal(threshold.value, '30');
    assert.equal(threshold.min, 1);
    assert.equal(threshold.max, 365);
    for (const value of ['0', '366']) {
      threshold.value = value; threshold.dispatchEvent(new Event('input'));
      assert.equal(threshold.validationMessage, copy('alerts.validation.expiryThreshold'));
    }
    for (const value of ['1', '365']) {
      threshold.value = value; threshold.dispatchEvent(new Event('input'));
      assert.equal(threshold.validationMessage, '');
    }
    assert.equal(threshold.parentElement.querySelector('label').textContent, copy('alerts.rules.thresholdDays'));
    change(metric, 'cpu_percent');
    assert.equal(comparator.parentElement.hidden, false);
    assert.equal(comparator.disabled, false);
    assert.equal(comparator.value, '>');
    change(comparator, '<');
    change(metric, 'node_expiry_days');
    change(metric, 'traffic_quota_percent');
    assert.equal(comparator.value, '<');
    change(metric, 'node_expiry_days');
    control(root, 'name').value = 'Renew VPS';
    comparator.value = '>';
    const form = elements(root).find((element) => element.tagName === 'form');
    form.dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.equal(payload.metric, 'node_expiry_days');
    assert.equal(payload.comparator, '<');
    assert.equal(payload.threshold, 30);
    dialog.close('done'); await dialog.closed;
    const edited = ruleEditor(rule(), context);
    assert.equal(control(root, 'metric').value, 'node_expiry_days');
    assert.equal(control(root, 'comparator').parentElement.hidden, true);
    assert.equal(control(root, 'threshold').value, '17');
    assert.equal(Number(control(root, 'for_seconds').value), 1);
    assert.equal(control(root, 'duration_unit').value, '60');
    edited.close('done'); await edited.closed;
  } finally {
    globalThis.fetch = originalFetch;
    setSession({user: null, csrf_token: '', expires_at: ''});
    dom.restore();
  }
});

test('expiry rules and alert event rows use the days format', () => {
  const dom = installDOM();
  try {
    const root = rulesPanel({rules: [rule()], groups: [], nodes: [], channels: [], saved: async () => {}});
    assert.ok(root.textContent.includes(copy('alerts.metric.nodeExpiry')));
    assert.ok(root.textContent.includes(copy('alerts.value.expiryDays', {value: '17'})));
    const [row] = eventRows([{id, node_id: id, node_name: 'VPS', rule_name: 'Expiry', metric: 'node_expiry_days', severity: 'warning', state: 'firing', value: 12.34, started_at: '2026-11-05T00:00:00Z', resolved_at: null}]);
    assert.equal(row.children[3].textContent, copy('alerts.value.expiryDays', {value: '12.3'}));
  } finally { dom.restore(); }
});
