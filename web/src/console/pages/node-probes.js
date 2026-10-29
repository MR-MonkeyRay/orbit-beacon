import {getLocale, t} from '../../shared/i18n.js';
import {errorText} from '../../shared/errors.js';
import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {formatPercent, when} from '../format.js';
import {button, facts, panel} from '../ui/components.js';

const ERRORS = {
  timeout: t('nodes.probe.timeout'), refused: t('nodes.probe.refused'), dns: t('nodes.probe.dns'),
  tls: t('nodes.probe.tls'), unreachable: t('nodes.probe.unreachable'),
  unsupported: t('nodes.probe.unsupported'), error: t('nodes.probe.error')
};

export function probeErrorText(error, statusCode) {
  if (!error) return '';
  if (error === 'http_status') return Number.isInteger(statusCode) && statusCode >= 100 && statusCode <= 599 ? `HTTP ${statusCode}` : t('nodes.probe.http');
  return ERRORS[error] || t('nodes.probe.error');
}

export function formatProbeLatency(value) {
  return Number.isFinite(value) && value >= 0 ? t('nodes.probe.latencyValue', {value: Number(value.toFixed(1)).toLocaleString(getLocale(), {useGrouping: false, maximumFractionDigits: 1})}) : t('nodes.unavailable');
}

export function formatProbeUptime(uptime, checks) {
  const count = Number.isSafeInteger(checks) && checks >= 0 ? checks : 0;
  const percent = count > 0 && Number.isFinite(uptime) && uptime >= 0 && uptime <= 100 ? formatPercent(uptime) : t('nodes.unavailable');
  return t('nodes.probe.checks', {percent, count});
}

function probeContent(probes) {
  if (!probes.length) return h('div', {class: 'stack'},
    h('p', {class: 'muted'}, t('nodes.probe.empty')),
    h('p', {class: 'muted'}, t('nodes.probe.guidance')));
  return h('div', {class: 'stack'},
    h('ul', {class: 'probe-list'}, probes.map((probe) => {
      const status = {up: ['online', t('nodes.probe.up')], down: ['offline', t('nodes.probe.down')], pending: ['pending', t('nodes.status.pending')]}[probe.status] || ['pending', t('nodes.status.pending')];
      const error = probeErrorText(probe.error, probe.status_code);
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

// Retained across Details tab visits. Detaching pauses polling; navigation
// permanently disposes it and prevents late responses from repainting.
export function probesPanel(id, signal) {
  let controller;
  let timer;
  let gone = false;
  let attached = false;
  const content = h('div', {class: 'probes-body'});
  const root = panel(t('nodes.probes'), {}, content);
  const stop = () => { controller?.abort(); clearInterval(timer); timer = undefined; };
  const observer = typeof MutationObserver === 'function' ? new MutationObserver(() => {
    if (gone || signal?.aborted) return;
    if (root.isConnected) {
      attached = true;
      if (timer === undefined) { startTimer(); if (!document.hidden) load(); }
    } else if (attached) stop();
  }) : null;
  const dispose = () => {
    gone = true;
    stop();
    observer?.disconnect();
    signal?.removeEventListener('abort', dispose);
  };
  observer?.observe(document.body, {childList: true, subtree: true});
  signal?.addEventListener('abort', dispose, {once: true});

  async function load() {
    if (gone || signal?.aborted) return;
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
        content.replaceChildren(h('div', {class: 'history-error'},
          h('p', {class: 'muted', role: 'status'}, t('nodes.probe.loadError', {error: errorText(error)})),
          button(t('nodes.retry'), {small: true, id: 'probes-retry', onClick: load})));
      }
    } finally {
      if (!request.signal.aborted && !signal?.aborted && controller === request) content.setAttribute('aria-busy', 'false');
    }
  }

  function startTimer() {
    timer = setInterval(() => {
      if (!root.isConnected) { stop(); return; }
      if (!document.hidden && !gone) load();
    }, 60000);
  }
  if (!signal?.aborted) { startTimer(); load(); }
  else dispose();
  return root;
}
