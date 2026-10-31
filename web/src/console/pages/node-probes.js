import {getLocale, t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {formatPercent, when} from '../format.js';
import {facts, panel} from '../ui/components.js';
import {historyError} from '../ui/history-error.js';
import {panelLifecycle} from '../ui/polling-panel.js';

const ERRORS = {
  timeout: t('nodes.probe.timeout'), refused: t('nodes.probe.refused'), dns: t('nodes.probe.dns'),
  tls: t('nodes.probe.tls'), unreachable: t('nodes.probe.unreachable'),
  unsupported: t('nodes.probe.unsupported'), error: t('nodes.probe.error')
};

export function probeErrorText(error) {
  if (!error) return '';
  if (error === 'http_status') return t('nodes.probe.http');
  return ERRORS[error] || t('nodes.probe.error');
}

export function formatProbeLatency(value) {
  return Number.isFinite(value) && value >= 0 ? t('nodes.probe.latencyValue', {value: Number(value.toFixed(1)).toLocaleString(getLocale(), {useGrouping: false, maximumFractionDigits: 1})}) : t('common.unavailable');
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
          ...(error ? [[t('nodes.probe.result'), error]] : []),
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
  const content = h('div', {class: 'probes-body'});
  const root = panel(t('nodes.probes'), {}, content);
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
      if (!request.signal.aborted && !signal?.aborted && controller === request) content.replaceChildren(probeContent(data.probes));
    } catch (error) {
      if (!request.signal.aborted && !signal?.aborted && controller === request && error.name !== 'AbortError') {
        content.replaceChildren(historyError(t('nodes.probe.loadError', {error: errorText(error)}),
          {retryID: 'probes-retry', onRetry: load, status: true}));
      }
    } finally {
      if (!request.signal.aborted && !signal?.aborted && controller === request) content.setAttribute('aria-busy', 'false');
    }
  }

  lifecycle = panelLifecycle({root, signal, load, cancel: () => controller?.abort()});
  return root;
}
