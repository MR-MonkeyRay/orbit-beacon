import {h} from '../../shared/dom.js';
import {createFreshnessController, sourceDeadline, timestampMillis} from '../../shared/freshness.js';
import {absoluteWhen, formatDuration, formatPercent} from '../format.js';
import {panel, readonlyFacts} from '../ui/components.js';
import {diskSummary, memorySummary, networkSummary} from './latest-sample-helpers.js';

const usableStatus = (value) => value === 'ok' || value === 'stale';
const number = (value) => Number.isFinite(value) && value >= 0;
const bytes = (value) => Number.isSafeInteger(value) && value >= 0;

// The only mutable portion of this view is readonly sample facts. The clock
// callback never rebuilds facts or touches a numeric value, input, or tab.
export function latestSamplePanel(node, initial, {signal} = {}) {
  const sequence = h('span', {class: 'revision'});
  const body = h('div');
  const element = panel('Latest sample', {action: sequence}, body);
  let records = [];
  let metricRecords = new Map();
  const values = readonlyFacts();
  const lastReport = h('p', {class: 'last-report', hidden: true});
  let closed = false;
  const freshness = createFreshnessController({signal, onChange(now) {
    let next = Infinity;
    for (const record of records) {
      const old = record.available && (record.originalStale || !Number.isFinite(now) || now > record.deadline);
      record.value.classList.toggle('metric-stale', old);
      record.state.hidden = !old;
      record.state.textContent = old ? 'Stale' : '';
      if (record.available && !record.originalStale && record.deadline >= now) next = Math.min(next, record.deadline);
    }
    if (lastReport) lastReport.hidden = !lastReport.querySelector('time');
    return Number.isFinite(next) ? next : null;
  }});
  const close = () => {
    if (closed) return;
    closed = true;
    freshness.close();
    signal?.removeEventListener('abort', close);
  };
  signal?.addEventListener('abort', close, {once: true});
  if (signal?.aborted) close();

  const snapshot = (latest, serverTime = latest?.server_time) => {
    if (closed || signal?.aborted) return;
    records = [];
    const nextRecords = new Map();
    sequence.hidden = !latest;
    sequence.textContent = latest ? `Sequence ${latest.seq}` : '';
    if (!latest) {
      metricRecords.clear();
      lastReport.replaceChildren();
      body.replaceChildren(h('p', {class: 'muted'}, 'No current sample. The agent has not reported since the server started, or its credential was revoked or the node disabled.'));
      freshness.refresh();
      return;
    }
    const metrics = latest.metrics || {};
    const metric = (keys, family, valid, text) => {
      const statuses = keys.map((key) => metrics.metric_status?.[key]);
      const deadline = sourceDeadline(latest.received_at, metrics.sources?.[family]);
      const available = valid && statuses.every(usableStatus) && Number.isFinite(deadline);
      const key = keys.join('\n');
      let record = metricRecords.get(key);
      if (!record) {
        const value = h('span', {class: 'sample-value'});
        const state = h('small', {class: 'sample-state', hidden: true});
        record = {value, state, element: h('span', {class: 'sample-metric'}, value, state)};
      }
      const content = available ? text : '—';
      if (record.value.textContent !== content) record.value.textContent = content;
      Object.assign(record, {available, deadline, originalStale: statuses.includes('stale')});
      records.push(record);
      nextRecords.set(key, record);
      return record.element;
    };
    const memoryValid = bytes(metrics.memory_total_bytes) && bytes(metrics.memory_available_bytes)
      && metrics.memory_available_bytes <= metrics.memory_total_bytes;
    const load = ['load1', 'load5', 'load15'].map((key) => metric([key], 'load', number(metrics[key]), number(metrics[key]) ? metrics[key].toFixed(2) : '—'));
    const disks = Array.isArray(metrics.disks) && metrics.disks.length ? h('div', {class: 'sample-series'}, metrics.disks.map((disk) => metric(
      [`disk:${disk.path}`], `disk:${disk.path}`, bytes(disk.total_bytes) && bytes(disk.used_bytes) && disk.used_bytes <= disk.total_bytes,
      diskSummary([disk])))) : '—';
    const networks = Array.isArray(metrics.networks) && metrics.networks.length ? h('div', {class: 'sample-series'}, metrics.networks.map((network) => metric(
      [`network:${network.name}`], `network:${network.name}`, number(network.rx_bytes_per_sec) && number(network.tx_bytes_per_sec),
      networkSummary([network])))) : '—';
    const sampled = timestampMillis(latest.sampled_at);
    const entries = [
      ['CPU', metric(['cpu_percent'], 'cpu', number(metrics.cpu_percent) && metrics.cpu_percent <= 100, formatPercent(metrics.cpu_percent))],
      ['Memory', metric(['memory_total_bytes', 'memory_available_bytes'], 'memory', memoryValid, memorySummary(metrics))],
      ['Load (1 / 5 / 15 min)', h('span', {class: 'sample-load'}, load[0], ' / ', load[1], ' / ', load[2])],
      ['Disk', disks], ['Network', networks],
      ['Uptime', metric(['uptime_seconds'], 'uptime', number(metrics.uptime_seconds), formatDuration(metrics.uptime_seconds))],
      ['Agent version', latest.system?.agent_version || node.agent_version || '—'],
      ['Sampled', Number.isFinite(sampled) ? absoluteWhen(latest.sampled_at) : '—'],
    ];
    const received = timestampMillis(latest.received_at);
    metricRecords = nextRecords;
    lastReport.replaceChildren(...(Number.isFinite(received)
      ? ['Last report · ', absoluteWhen(latest.received_at)] : []));
    values.update(entries);
    if (!body.contains(values.element)) body.replaceChildren(values.element, lastReport);
    if (!freshness.clock(serverTime)) freshness.refresh();
  };
  snapshot(initial);
  return {element, snapshot, clock: freshness.clock, attach: freshness.resume, detach: freshness.pause, close};
}
