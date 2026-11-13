import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {absoluteWhen, formatMilliseconds, formatPercent, when} from '../format.js';
import {chart} from '../ui/chart.js';
import {select} from '../ui/forms.js';
import {panel} from '../ui/components.js';
import {historyError} from '../ui/history-error.js';
import {panelLifecycle} from '../ui/polling-panel.js';
import {skLines} from '../ui/placeholders.js';

const ERRORS = {
  timeout: t('nodes.probe.timeout'), refused: t('nodes.probe.refused'), dns: t('nodes.probe.dns'),
  tls: t('nodes.probe.tls'), unreachable: t('nodes.probe.unreachable'),
  unsupported: t('nodes.probe.unsupported'), error: t('nodes.probe.error')
};
const HISTORY_RANGES = ['24h', '7d'];

export function probeErrorText(error) {
  if (!error || error === 'pending') return '';
  if (error === 'http_status') return t('nodes.probe.http');
  return ERRORS[error] || t('nodes.probe.error');
}

export function formatProbeLatency(value) {
  return formatMilliseconds(value);
}

export function formatProbeUptime(uptime, checks) {
  const count = Number.isSafeInteger(checks) && checks >= 0 ? checks : 0;
  const percent = count > 0 && Number.isFinite(uptime) && uptime >= 0 && uptime <= 100 ? formatPercent(uptime) : t('common.unavailable');
  return t('nodes.probe.checks', {percent, count});
}

// 每个探测一行；“结果”和“证书到期”只在至少一个探测有值时成列。
function probeContent(probes) {
  if (!probes.length) return h('p', {class: 'text-note'}, t('nodes.probe.empty'));
  const showResult = probes.some((probe) => probeErrorText(probe.error));
  const showTLS = probes.some((probe) => probe.tls_expires_at);
  const muted = () => h('span', {class: 'muted'}, '—');
  const columns = [t('nodes.presence'), t('nodes.audit.target'), t('nodes.probe.latency'),
    showResult && t('nodes.probe.result'), showTLS && t('nodes.probe.tlsExpires'), t('nodes.probe.checked'), t('nodes.probe.uptime')].filter(Boolean);
  return h('div', {class: 'table-wrap'}, h('table', {class: 'table probe-table'},
    h('thead', null, h('tr', null, columns.map((label) => h('th', {scope: 'col'}, label)))),
    h('tbody', null, probes.map((probe) => {
      const status = {up: ['online', t('nodes.probe.up')], down: ['offline', t('nodes.probe.down')], pending: ['pending', t('common.status.pending')]}[probe.status] || ['pending', t('common.status.pending')];
      const error = probeErrorText(probe.error);
      return h('tr', {class: 'probe-row'},
        h('td', null, h('span', {class: `badge ${status[0]}`}, status[1])),
        h('td', {class: 'probe-target-cell'}, h('code', {class: 'probe-target mono'}, probe.target)),
        h('td', {class: 'num'}, formatProbeLatency(probe.latency_ms)),
        showResult ? h('td', null, error ? (probe.status_code ? `${error} (${probe.status_code})` : error) : muted()) : null,
        showTLS ? h('td', null, probe.tls_expires_at ? absoluteWhen(probe.tls_expires_at) : muted()) : null,
        h('td', null, probe.checked_at ? when(probe.checked_at) : t('nodes.probe.notChecked')),
        h('td', null, formatProbeUptime(probe.uptime_24h, probe.checks_24h)));
    }))));
}

// 切换详情标签页时保留面板，卸载时暂停轮询；
// 离开页面后永久销毁，防止迟到的响应重绘。
export function probesPanel(id, signal) {
  let controller;
  let lifecycle;
  let historyController;
  let targets = [];
  let target = '';
  let range = '24h';
  const charts = h('div', {class: 'history-grid', hidden: true});
  const historyStatus = h('div', {hidden: true});
  const showHistoryStatus = (...nodes) => {
    historyStatus.replaceChildren(...nodes);
    historyStatus.hidden = !nodes.length;
  };
  const targetSelect = select([], '', {id: 'probe-history-target', 'aria-label': t('nodes.audit.target'), onchange: () => {
    if (target === targetSelect.value) return;
    target = targetSelect.value;
    loadHistory();
  }});
  const targetSelector = h('div', {class: 'probe-target-select', hidden: true}, targetSelect);
  const controls = HISTORY_RANGES.map((value) => h('button', {type: 'button', id: `probe-history-range-${value}`,
    'aria-pressed': String(value === range), onclick: () => {
      if (range === value) return;
      range = value;
      controls.forEach((control, index) => control.setAttribute('aria-pressed', String(HISTORY_RANGES[index] === range)));
      loadHistory();
    }}, t(value === '24h' ? 'nodes.range.hours' : 'nodes.range.days', {count: value === '24h' ? 24 : 7})));
  const rangeSelector = h('div', {class: 'segmented', role: 'group', 'aria-label': t('nodes.historyRange')}, controls);
  const historyControls = h('div', {class: 'head-actions'}, targetSelector, rangeSelector);
  async function loadHistory() {
    historyController?.abort();
    if (!target || lifecycle?.disposed || signal?.aborted) return;
    const request = new AbortController();
    historyController = request;
    charts.setAttribute('aria-busy', 'true');
    try {
      const data = await api(`/nodes/${encodeURIComponent(id)}/probes/history?target=${encodeURIComponent(target)}&range=${range}`, {signal: request.signal});
      if (request.signal.aborted || signal?.aborted || historyController !== request) return;
      if (data.unit !== 'ms' || !Array.isArray(data.points)) throw Object.assign(new Error('Unreadable probe history'), {code: 'unreadable_response'});
      const points = data.points.filter((point) => point && Number.isFinite(Date.parse(point.t)) && Number.isFinite(point.avg) && Number.isFinite(point.max));
      const focused = charts.contains(document.activeElement) ? document.activeElement : null;
      const selectedTime = Number(focused?.dataset?.selectedTime) || undefined;
      const series = [{label: t('nodes.probe.latency'), stepSeconds: data.step_seconds, points}];
      charts.replaceChildren(...(points.length ? [h('section', {class: 'history-metric', 'aria-label': t('nodes.historyAria', {title: t('nodes.probe.latency')})},
        chart({title: t('nodes.probe.latency'), unit: 'ms', id: 'probe-history-chart', selectedTime, series}))] : []));
      charts.hidden = !points.length;
      showHistoryStatus();
      if (focused) root.querySelector('#probe-history-chart')?.focus({preventScroll: true});
    } catch (error) {
      if (!request.signal.aborted && !signal?.aborted && historyController === request && error.name !== 'AbortError') {
        showHistoryStatus(historyError(t('nodes.historyLoadError', {error: errorText(error)}),
          {retryID: 'probe-history-retry', onRetry: loadHistory}));
      }
    } finally {
      if (!request.signal.aborted && !signal?.aborted && historyController === request) charts.setAttribute('aria-busy', 'false');
    }
  }
  const content = h('div', {class: 'probes-body'});
  // 结果表由标签页命名；延迟历史单独成面板，目标与时间范围放在面板头右侧。
  const history = panel(t('nodes.historyAria', {title: t('nodes.probe.latency')}), {action: historyControls}, historyStatus, charts);
  history.classList.add('probes-history');
  history.hidden = true;
  const root = h('div', {class: 'stack'}, panel(null, {}, content), history);
  async function load() {
    if (lifecycle?.disposed || signal?.aborted) return;
    controller?.abort();
    const request = new AbortController();
    controller = request;
    content.setAttribute('aria-busy', 'true');
    if (!content.children.length) content.replaceChildren(h('p', {class: 'sr', role: 'status'}, t('nodes.probe.loading')), skLines('58%', '42%', '66%'));
    try {
      const data = await api(`/nodes/${encodeURIComponent(id)}/probes`, {signal: request.signal});
      if (!Array.isArray(data.probes)) throw Object.assign(new Error('Unreadable probe results'), {code: 'unreadable_response'});
      if (!request.signal.aborted && !signal?.aborted && controller === request) {
        content.replaceChildren(probeContent(data.probes));
        const next = data.probes.map((probe) => probe.target);
        if (JSON.stringify(next) !== JSON.stringify(targets)) {
          targets = next;
          if (!targets.includes(target)) target = targets[0] || '';
          targetSelect.replaceChildren(...targets.map((value) => h('option', {value}, value)));
          targetSelect.value = target;
          targetSelector.hidden = targets.length <= 1;
          history.hidden = !targets.length;
        }
        if (!target) {
          historyController?.abort();
          charts.replaceChildren();
          charts.hidden = true;
          showHistoryStatus();
        } else loadHistory();
      }
    } catch (error) {
      if (!request.signal.aborted && !signal?.aborted && controller === request && error.name !== 'AbortError') {
        content.replaceChildren(historyError(t('nodes.probe.loadError', {error: errorText(error)}),
          {retryID: 'probes-retry', onRetry: load, status: true}));
      }
    } finally {
      if (!request.signal.aborted && !signal?.aborted && controller === request) content.setAttribute('aria-busy', 'false');
    }
  }

  lifecycle = panelLifecycle({root, signal, load, cancel: () => { controller?.abort(); historyController?.abort(); }});
  return root;
}
