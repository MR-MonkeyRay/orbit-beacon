import {t, tParts} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {chart} from '../ui/chart.js';
import {button, link, panel} from '../ui/components.js';
import {select} from '../ui/forms.js';
import {historyError} from '../ui/history-error.js';
import {panelLifecycle} from '../ui/polling-panel.js';

const STORAGE_KEY = 'orbitbeacon.history.range';
const RANGES = ['1h', '6h', '24h', '7d', '30d', '90d'];
const RANGE_MS = Object.fromEntries(RANGES.map((range) => [range, Number.parseInt(range, 10) * (range.endsWith('h') ? 3600000 : 86400000)]));
const CHARTS = [
  {title: t('nodes.cpu'), unit: 'percent', metrics: [['cpu_percent', t('nodes.cpu')]]},
  {title: t('nodes.memory'), unit: 'percent', metrics: [['memory_used_percent', t('nodes.memory')]]},
  {title: t('nodes.load'), unit: 'load', metrics: [['load1', t('nodes.load')]]},
  {title: t('nodes.disk'), unit: 'percent', metrics: [['disk_used_percent', t('nodes.disk')]]},
  {title: t('nodes.inode'), optional: true, unit: 'percent', metrics: [['inode_used_percent', t('nodes.inode')]]},
  {title: t('nodes.diskIO'), optional: true, unit: 'bytes_per_second', metrics: [['disk_read_bytes_per_second', t('nodes.diskRead')], ['disk_write_bytes_per_second', t('nodes.diskWrite')]]},
  {title: t('nodes.diskBusy'), optional: true, unit: 'percent', metrics: [['disk_busy_percent', t('nodes.diskBusy')]]},
  {title: t('nodes.pressure'), optional: true, unit: 'percent', metrics: [['pressure_cpu_percent', t('nodes.pressureCPU')], ['pressure_memory_percent', t('nodes.pressureMemory')], ['pressure_io_percent', t('nodes.pressureIO')]]},
  {title: t('nodes.temperature'), optional: true, unit: 'celsius', metrics: [['temperature_celsius', t('nodes.temperature')]]},
  {title: t('nodes.network'), unit: 'bytes_per_second', metrics: [['net_rx_bytes_per_second', t('nodes.receiveRx')], ['net_tx_bytes_per_second', t('nodes.sendTx')]]}
];

function savedRange() {
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (RANGES.includes(stored)) return stored;
  } catch {}
  return '24h';
}

// 每个页面生命周期一个面板，切回详情标签页时复用。
// 路由器的中止信号在刷新/导航时也会销毁计时器。
export function historyPanel(id, signal, latest) {
  let range = savedRange();
  let loadMetric = 'load1';
  let disk = '';
  let diskPaths = [];
  let controller;
  let lifecycle;
  const status = h('p', {class: 'text-note', role: 'status'});
  const charts = h('div', {class: 'history-grid'});
  const controls = RANGES.map((value) => h('button', {type: 'button', id: `history-range-${value}`, dataset: {range: value},
    'aria-pressed': String(value === range), onclick: () => {
      if (range === value) return;
      range = value;
      try { localStorage.setItem(STORAGE_KEY, range); } catch {}
      for (const control of controls) control.setAttribute('aria-pressed', String(control.dataset.range === range));
      load();
    }}, t(value.endsWith('h') ? 'nodes.range.hours' : 'nodes.range.days', {count: Number.parseInt(value, 10)})));
  const shortSpan = h('div', {class: 'history-short-span text-note', hidden: true},
    h('span', null, t('nodes.historyShortSpan')),
    button(t('nodes.historyShowHour'), {small: true, onClick: () => {
      controls[0].focus();
      controls[0].click();
    }}));
  const selector = h('div', {class: 'segmented', role: 'group', 'aria-label': t('nodes.historyRange')}, controls);
  const loadControls = ['load1', 'load5', 'load15'].map((value) => h('button', {type: 'button', id: `history-load-${value}`, dataset: {metric: value},
    'aria-pressed': String(value === loadMetric), onclick: () => {
      if (loadMetric === value) return;
      loadMetric = value;
      for (const control of loadControls) control.setAttribute('aria-pressed', String(control.dataset.metric === loadMetric));
      load();
    }}, t('nodes.duration.minutes', {minutes: Number(value.slice(4))})));
  const loadSelector = h('div', {class: 'segmented', role: 'group', 'aria-label': t('nodes.load')}, loadControls);
  const diskSelect = select([], '', {id: 'history-disk', 'aria-label': t('nodes.disk'), onchange: () => {
    if (disk === diskSelect.value) return;
    disk = diskSelect.value;
    load();
  }});
  const diskSelector = h('div', {class: 'head-actions history-disk', hidden: true}, h('label', {htmlFor: 'history-disk'}, t('nodes.disk')), diskSelect);
  const metricSection = (item, content) => h('section', {class: 'history-metric', 'aria-label': t('nodes.historyAria', {title: item.title})},
    item.unit === 'load' ? h('div', {class: 'head-actions'}, h('h3', null, item.title), loadSelector) : h('h3', null, item.title), content);

  const root = panel(t('nodes.history'), {action: selector}, diskSelector, status, shortSpan, charts);
  // Paths follow the current sample, including updates from the node live stream.
  root.snapshot = (sample) => {
    const next = [...new Set((sample?.metrics?.disks || []).map((item) => item.path))];
    if (JSON.stringify(next) === JSON.stringify(diskPaths)) return;
    diskPaths = next;
    const changed = disk && !diskPaths.includes(disk);
    if (changed) disk = '';
    diskSelect.replaceChildren(h('option', {value: ''}, t('nodes.allDisks')),
      ...diskPaths.map((path) => h('option', {value: path}, path)));
    diskSelect.value = disk;
    diskSelector.hidden = diskPaths.length <= 1;
    if (changed && lifecycle) load();
  };
  root.snapshot(latest);
  async function load() {
    if (lifecycle?.disposed || signal?.aborted) return;
    controller?.abort();
    const request = new AbortController();
    controller = request;
    const requestedRange = range;
    const requestedDisk = disk;
    const requestedCharts = CHARTS.map((item) => item.unit === 'load'
      ? {...item, metrics: [[loadMetric, t('nodes.load')]]} : item);
    shortSpan.hidden = true;
    status.textContent = t('nodes.historyLoading');
    charts.setAttribute('aria-busy', 'true');
    // Keep rendered charts and their controls until the replacement data arrives.
    if (!charts.children.length) charts.replaceChildren(...requestedCharts.filter((item) => !item.optional).map((item) => metricSection(item)));
    charts.dataset.range = range;
    charts.dataset.loadMetric = loadMetric;
    const metrics = requestedCharts.flatMap((item) => item.metrics.map(([metric]) => metric));
    const results = new Map();
    await Promise.all(metrics.map(async (metric) => {
      try {
        const diskQuery = requestedDisk && ['disk_used_percent', 'inode_used_percent'].includes(metric)
          ? `&disk=${encodeURIComponent(requestedDisk)}` : '';
        const data = await api(`/nodes/${encodeURIComponent(id)}/history?metric=${metric}&range=${requestedRange}${diskQuery}`, {signal: request.signal});
        if (data.unit !== requestedCharts.find((item) => item.metrics.some(([name]) => name === metric)).unit) throw Object.assign(new Error('Unsupported history unit'), {code: 'unreadable_response'});
        results.set(metric, {data});
      } catch (error) {
        results.set(metric, {error});
        if (error.status === 404 && !request.signal.aborted && !signal?.aborted && controller === request) {
          lifecycle.dispose();
          selector.hidden = true;
          shortSpan.hidden = true;
          diskSelector.hidden = true;
          charts.replaceChildren();
          charts.setAttribute('aria-busy', 'false');
          status.replaceChildren(...tParts('nodes.nodeMissingPrefix', {link: link('/nodes', t('nodes.goNodes'))}));
        }
      }
    }));
    if (request.signal.aborted || signal?.aborted || controller !== request) return;
    // 请求完成后再读取焦点，因为用户可能已在等待期间聚焦某个图表。
    const focusedID = charts.contains(document.activeElement) ? document.activeElement.id : null;
    const selectedTime = Number(document.activeElement?.dataset?.selectedTime) || undefined;
    let firstTime = Infinity, lastTime = -Infinity;
    charts.replaceChildren(...requestedCharts.flatMap((item) => {
      const error = item.metrics.map(([metric]) => results.get(metric).error).find(Boolean);
      const series = error ? [] : item.metrics.map(([metric, label]) => {
        const data = results.get(metric).data;
        const points = Array.isArray(data.points) ? data.points.filter((point) => point && Number.isFinite(Date.parse(point.t))
          && Number.isFinite(point.avg) && Number.isFinite(point.max)) : [];
        return {label, points, stepSeconds: Number.isFinite(data.step_seconds) && data.step_seconds > 0 ? data.step_seconds : 60};
      });
      if (!error && item.optional && series.every((item) => !item.points.length)) return [];
      for (const {points} of series) for (const point of points) {
        const time = Date.parse(point.t);
        firstTime = Math.min(firstTime, time);
        lastTime = Math.max(lastTime, time);
      }
      const content = error
        ? historyError(t('nodes.historyLoadError', {error: errorText(error)}),
          {retryID: `history-retry-${item.metrics[0][0]}`, onRetry: load})
        : chart({title: item.title, unit: item.unit, id: `history-chart-${item.metrics[0][0]}`,
          selectedTime: focusedID === `history-chart-${item.metrics[0][0]}` ? selectedTime : undefined, series});
      return [metricSection(item, content)];
    }));
    shortSpan.hidden = requestedRange === '1h' || !Number.isFinite(firstTime) || lastTime - firstTime >= RANGE_MS[requestedRange] * 0.05;
    charts.setAttribute('aria-busy', 'false');
    const failures = [...results.values()].filter((result) => result.error).length;
    status.textContent = failures ? t('nodes.historyPartial') : '';
    if (focusedID) {
      const target = document.getElementById(focusedID);
      if (target && root.contains(target)) target.focus({preventScroll: true});
    }
  }

  lifecycle = panelLifecycle({root, signal, load, cancel: () => controller?.abort(), loadWhenHidden: true});
  return root;
}
