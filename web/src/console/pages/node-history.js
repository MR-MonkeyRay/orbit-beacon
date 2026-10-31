import {t, tParts} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {chart} from '../ui/chart.js';
import {link, note, panel} from '../ui/components.js';
import {historyError} from '../ui/history-error.js';
import {panelLifecycle} from '../ui/polling-panel.js';

const STORAGE_KEY = 'orbitbeacon.history.range';
const RANGES = ['1h', '6h', '24h', '7d', '30d'];
const CHARTS = [
  {title: t('nodes.cpu'), unit: 'percent', metrics: [['cpu_percent', t('nodes.cpu')]]},
  {title: t('nodes.memory'), unit: 'percent', metrics: [['memory_used_percent', t('nodes.memory')]]},
  {title: t('nodes.load1'), unit: 'load', metrics: [['load1', t('nodes.load')]]},
  {title: t('nodes.disk'), unit: 'percent', metrics: [['disk_used_percent', t('nodes.disk')]]},
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
export function historyPanel(id, signal) {
  let range = savedRange();
  let controller;
  let lifecycle;
  const status = h('p', {class: 'muted', role: 'status'});
  const charts = h('div', {class: 'history-grid'});
  const controls = RANGES.map((value) => h('button', {type: 'button', id: `history-range-${value}`, dataset: {range: value},
    'aria-pressed': String(value === range), onclick: () => {
      if (range === value) return;
      range = value;
      try { localStorage.setItem(STORAGE_KEY, range); } catch {}
      for (const control of controls) control.setAttribute('aria-pressed', String(control.dataset.range === range));
      load();
    }}, t(value.endsWith('h') ? 'nodes.range.hours' : 'nodes.range.days', {count: Number.parseInt(value, 10)})));
  const selector = h('div', {class: 'segmented', role: 'group', 'aria-label': t('nodes.historyRange')}, controls);
  const root = panel(t('nodes.history'), {action: selector},
    h('p', {class: 'lede'}, t('nodes.historyIntro')),
    status, charts, note(t('nodes.historyHint')));
  async function load() {
    if (lifecycle?.disposed || signal?.aborted) return;
    controller?.abort();
    const request = new AbortController();
    controller = request;
    const requestedRange = range;
    status.textContent = t('nodes.historyLoading');
    charts.setAttribute('aria-busy', 'true');
    // 所选范围变化时清空图表，避免旧数据被误认为新范围的数据。
    // 刷新时保留已聚焦的图表直到加载完成。
    if (charts.dataset.range !== range) charts.replaceChildren();
    charts.dataset.range = range;
    const metrics = CHARTS.flatMap((item) => item.metrics.map(([metric]) => metric));
    const results = new Map();
    await Promise.all(metrics.map(async (metric) => {
      try {
        const data = await api(`/nodes/${encodeURIComponent(id)}/history?metric=${metric}&range=${requestedRange}`, {signal: request.signal});
        if (data.unit !== CHARTS.find((item) => item.metrics.some(([name]) => name === metric)).unit) throw Object.assign(new Error('Unsupported history unit'), {code: 'unreadable_response'});
        results.set(metric, {data});
      } catch (error) {
        results.set(metric, {error});
        if (error.status === 404 && !request.signal.aborted && !signal?.aborted && controller === request) {
          lifecycle.dispose();
          selector.hidden = true;
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
    charts.replaceChildren(...CHARTS.map((item) => {
      const error = item.metrics.map(([metric]) => results.get(metric).error).find(Boolean);
      const content = error
        ? historyError(t('nodes.historyLoadError', {error: errorText(error)}),
          {retryID: `history-retry-${item.metrics[0][0]}`, onRetry: load})
        : chart({title: item.title, unit: item.unit, id: `history-chart-${item.metrics[0][0]}`,
          selectedTime: focusedID === `history-chart-${item.metrics[0][0]}` ? selectedTime : undefined,
          series: item.metrics.map(([metric, label]) => {
            const data = results.get(metric).data;
            // 只渲染格式正确的点；其余一律视为空隙。
            const points = Array.isArray(data.points) ? data.points.filter((point) => point && Number.isFinite(Date.parse(point.t))
              && Number.isFinite(point.avg) && Number.isFinite(point.max)) : [];
            return {label, points, stepSeconds: Number.isFinite(data.step_seconds) && data.step_seconds > 0 ? data.step_seconds : 60};
          })});
      return h('section', {class: 'history-metric', 'aria-label': t('nodes.historyAria', {title: item.title})}, h('h3', null, item.title), content);
    }));
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
