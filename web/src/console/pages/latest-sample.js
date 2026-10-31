import {t, tParts} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {createFreshnessController, sourceDeadline, timestampMillis} from '../../shared/freshness.js';
import {absoluteWhen, formatDuration, formatLoad, formatPercent} from '../format.js';
import {panel, readonlyFacts} from '../ui/components.js';
import {diskSummary, memorySummary, networkSummary} from '../lib/latest-sample-helpers.js';

const usableStatus = (value) => value === 'ok' || value === 'stale';
const number = (value) => Number.isFinite(value) && value >= 0;
const bytes = (value) => Number.isSafeInteger(value) && value >= 0;

// 此视图只更新只读的采样信息。时钟回调只更新过期标记，
// 不重建采样信息，也不改动数值、输入框或标签页。
export function latestSamplePanel(node, initial, {signal} = {}) {
  const sequence = h('span', {class: 'revision'});
  const body = h('div');
  const element = panel(t('nodes.sample'), {action: sequence}, body);
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
      record.state.textContent = old ? t('nodes.stale') : '';
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
    sequence.textContent = latest ? t('nodes.sequence', {value: latest.seq}) : '';
    if (!latest) {
      metricRecords.clear();
      lastReport.replaceChildren();
      body.replaceChildren(h('p', {class: 'muted'}, t('nodes.sampleEmpty')));
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
    const load = ['load1', 'load5', 'load15'].map((key) => metric([key], 'load', number(metrics[key]), number(metrics[key]) ? formatLoad(metrics[key]) : '—'));
    const disks = Array.isArray(metrics.disks) && metrics.disks.length ? h('div', {class: 'sample-series'}, metrics.disks.map((disk) => metric(
      [`disk:${disk.path}`], `disk:${disk.path}`, bytes(disk.total_bytes) && bytes(disk.used_bytes) && disk.used_bytes <= disk.total_bytes,
      diskSummary([disk])))) : '—';
    const networks = Array.isArray(metrics.networks) && metrics.networks.length ? h('div', {class: 'sample-series'}, metrics.networks.map((network) => metric(
      [`network:${network.name}`], `network:${network.name}`, number(network.rx_bytes_per_sec) && number(network.tx_bytes_per_sec),
      networkSummary([network])))) : '—';
    const sampled = timestampMillis(latest.sampled_at);
    const entries = [
      [t('nodes.cpu'), metric(['cpu_percent'], 'cpu', number(metrics.cpu_percent) && metrics.cpu_percent <= 100, formatPercent(metrics.cpu_percent))],
      [t('nodes.memory'), metric(['memory_total_bytes', 'memory_available_bytes'], 'memory', memoryValid, memorySummary(metrics))],
      [t('nodes.loadSeries'), h('span', {class: 'sample-load'}, tParts('nodes.loadValues', {one: load[0], five: load[1], fifteen: load[2]}))],
      [t('nodes.disk'), disks], [t('nodes.network'), networks],
      [t('nodes.uptime'), metric(['uptime_seconds'], 'uptime', number(metrics.uptime_seconds), formatDuration(metrics.uptime_seconds))],
      [t('nodes.agentVersion'), latest.system?.agent_version || node.agent_version || '—'],
      [t('nodes.sampled'), Number.isFinite(sampled) ? absoluteWhen(latest.sampled_at) : '—'],
    ];
    const received = timestampMillis(latest.received_at);
    metricRecords = nextRecords;
    lastReport.replaceChildren(...(Number.isFinite(received)
      ? tParts('nodes.lastReport', {time: absoluteWhen(latest.received_at)}) : []));
    values.update(entries);
    if (!body.contains(values.element)) body.replaceChildren(values.element, lastReport);
    if (!freshness.clock(serverTime)) freshness.refresh();
  };
  snapshot(initial);
  return {element, snapshot, clock: freshness.clock, attach: freshness.resume, detach: freshness.pause, close};
}
