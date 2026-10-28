import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {formatPercent, when} from '../format.js';
import {button, facts, panel} from '../ui/components.js';

const ERRORS = {
  timeout: 'Timed out', refused: 'Connection refused', dns: 'DNS lookup failed',
  tls: 'TLS handshake failed', unreachable: 'Network unreachable',
  unsupported: 'Not supported on this agent', error: 'Probe failed'
};

export function probeErrorText(error, statusCode) {
  if (!error) return '';
  if (error === 'http_status') return Number.isInteger(statusCode) && statusCode >= 100 && statusCode <= 599 ? `HTTP ${statusCode}` : 'HTTP request failed';
  return ERRORS[error] || 'Probe failed';
}

export function formatProbeLatency(value) {
  return Number.isFinite(value) && value >= 0 ? `${Number(value.toFixed(1))} ms` : 'Unavailable';
}

export function formatProbeUptime(uptime, checks) {
  const count = Number.isSafeInteger(checks) && checks >= 0 ? checks : 0;
  const percent = count > 0 && Number.isFinite(uptime) && uptime >= 0 && uptime <= 100 ? formatPercent(uptime) : 'Unavailable';
  return `${percent} · ${count} ${count === 1 ? 'check' : 'checks'}`;
}

function probeContent(probes, onConfigure) {
  if (!probes.length) return h('div', {class: 'stack'},
    h('p', {class: 'muted'}, 'No service probes configured.'),
    onConfigure ? button('Configure service probes', {small: true, onClick: onConfigure}) : null);
  return h('div', {class: 'stack'},
    h('ul', {class: 'probe-list'}, probes.map((probe) => {
      const status = {up: ['online', 'Up'], down: ['offline', 'Down'], pending: ['pending', 'Pending']}[probe.status] || ['pending', 'Pending'];
      const error = probeErrorText(probe.error, probe.status_code);
      return h('li', {class: 'probe-row'},
        h('div', {class: 'probe-heading'}, h('span', {class: `badge ${status[0]}`}, status[1]),
          h('code', {class: 'probe-target mono'}, probe.target)),
        facts([
          ['Latency', formatProbeLatency(probe.latency_ms)],
          ...(error ? [['Result', error]] : []),
          ['Checked', probe.checked_at ? when(probe.checked_at) : 'Not checked yet'],
          ['24h uptime', formatProbeUptime(probe.uptime_24h, probe.checks_24h)]
        ]));
    })),
    probes.every((probe) => probe.status === 'pending') ? h('p', {class: 'muted'},
      'Results appear after the agent runs its first check (requires an agent version with probe support).') : null);
}

// Retained across Details tab visits. Detaching pauses polling; navigation
// permanently disposes it and prevents late responses from repainting.
export function probesPanel(id, signal, onConfigure) {
  let controller;
  let timer;
  let gone = false;
  let attached = false;
  const content = h('div', {class: 'probes-body'});
  const root = panel('Service probes', {}, content);
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
    if (!content.children.length) content.replaceChildren(h('p', {class: 'muted', role: 'status'}, 'Loading service probes…'));
    try {
      const data = await api(`/nodes/${encodeURIComponent(id)}/probes`, {signal: request.signal});
      if (!Array.isArray(data.probes)) throw new Error('The server returned unreadable probe results.');
      if (!request.signal.aborted && !signal?.aborted && controller === request) content.replaceChildren(probeContent(data.probes, onConfigure));
    } catch (error) {
      if (!request.signal.aborted && !signal?.aborted && controller === request && error.name !== 'AbortError') {
        content.replaceChildren(h('div', {class: 'history-error'},
          h('p', {class: 'muted', role: 'status'}, `Could not load service probes: ${error.message}`),
          button('Retry', {small: true, id: 'probes-retry', onClick: load})));
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
