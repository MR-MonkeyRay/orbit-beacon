import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {absoluteWhen, formatMilliseconds, formatPercent, when} from '../format.js';
import {chart} from '../ui/chart.js';
import {select} from '../ui/forms.js';
import {facts, panel} from '../ui/components.js';
import {historyError} from '../ui/history-error.js';
import {panelLifecycle} from '../ui/polling-panel.js';

const ERRORS = {
  timeout: t('nodes.probe.timeout'), refused: t('nodes.probe.refused'), dns: t('nodes.probe.dns'),
  tls: t('nodes.probe.tls'), unreachable: t('nodes.probe.unreachable'),
  unsupported: t('nodes.probe.unsupported'), error: t('nodes.probe.error')
};
const HISTORY_RANGES = ['24h', '7d'];

export function probeErrorText(error) {
  if (!error) return '';
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

function probeContent(probes) {
  if (!probes.length) return h('div', {class: 'stack'},
    h('p', {class: 'muted'}, t('nodes.probe.empty')),
    h('p', {class: 'muted'}, t('nodes.probe.guidance')));
  return h('div', {class: 'stack'},
    h('ul', {class: 'probe-list'}, probes.map((probe) => {
      const status = {up: ['online', t('nodes.probe.up')], down: ['offline', t('nodes.probe.down')], pending: ['pending', t('common.status.pending')]}[probe.status] || ['pending', t('common.status.pending')];
      const error = probeErrorText(probe.error);
      return h('li', {class: 'probe-row'},
        h('div', {class: 'probe-heading'}, h('span', {class: `badge ${status[0]}`}, status[1]),
          h('code', {class: 'probe-target mono'}, probe.target)),
        facts([
          [t('nodes.probe.latency'), formatProbeLatency(probe.latency_ms)],
          ...(error ? [[t('nodes.probe.result'), probe.status_code ? `${error} (${probe.status_code})` : error]] : []),
          ...(probe.tls_expires_at ? [[t('nodes.probe.tlsExpires'), absoluteWhen(probe.tls_expires_at)]] : []),
          [t('nodes.probe.checked'), probe.checked_at ? when(probe.checked_at) : t('nodes.probe.notChecked')],
          [t('nodes.probe.uptime'), formatProbeUptime(probe.uptime_24h, probe.checks_24h)]
        ]));
    })),
    probes.every((probe) => probe.status === 'pending') ? h('p', {class: 'muted'},
      t('nodes.probe.pendingHint')) : null);
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
  const targetSelector = h('div', {class: 'head-actions history-disk', hidden: true},
    h('label', {htmlFor: 'probe-history-target'}, t('nodes.audit.target')), targetSelect);
  const controls = HISTORY_RANGES.map((value) => h('button', {type: 'button', id: `probe-history-range-${value}`,
    'aria-pressed': String(value === range), onclick: () => {
      if (range === value) return;
      range = value;
      controls.forEach((control, index) => control.setAttribute('aria-pressed', String(HISTORY_RANGES[index] === range)));
      loadHistory();
    }}, t(value === '24h' ? 'nodes.range.hours' : 'nodes.range.days', {count: value === '24h' ? 24 : 7})));
  const rangeSelector = h('div', {class: 'segmented', role: 'group', 'aria-label': t('nodes.historyRange')}, controls);
  const historyControls = h('div', {class: 'head-actions', hidden: true}, targetSelector, rangeSelector);
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
        h('h3', null, t('nodes.probe.latency')),
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
  const root = panel(t('nodes.probes'), {}, h('div', {class: 'stack'}, content, historyControls, historyStatus, charts));
  async function load() {
    if (lifecycle?.disposed || signal?.aborted) return;
    controller?.abort();
    const request = new AbortController();
    controller = request;
    content.setAttribute('aria-busy', 'true');
    if (!content.children.length) content.replaceChildren(h('p', {class: 'muted', role: 'status'}, t('nodes.probe.loading')));
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
          historyControls.hidden = !targets.length;
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
