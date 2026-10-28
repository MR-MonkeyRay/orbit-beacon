import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {chart} from '../ui/chart.js';
import {button, link, note, panel} from '../ui/components.js';

const STORAGE_KEY = 'orbitbeacon.history.range';
const RANGES = ['1h', '6h', '24h', '7d', '30d'];
const CHARTS = [
  {title: 'CPU', unit: 'percent', metrics: [['cpu_percent', 'CPU']]},
  {title: 'Memory', unit: 'percent', metrics: [['memory_used_percent', 'Memory']]},
  {title: 'Load (1 min)', unit: 'load', metrics: [['load1', 'Load']]},
  {title: 'Disk', unit: 'percent', metrics: [['disk_used_percent', 'Disk']]},
  {title: 'Network', unit: 'bytes_per_second', metrics: [['net_rx_bytes_per_second', 'Receive (rx)'], ['net_tx_bytes_per_second', 'Send (tx)']]}
];

function savedRange() {
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (RANGES.includes(stored)) return stored;
  } catch {}
  return '24h';
}

// One panel per page lifetime, reused when switching back to the Details tab.
// The router's abort signal also disposes the timer on refresh/navigation.
export function historyPanel(id, signal) {
  let range = savedRange();
  let controller;
  let gone = false;
  let timer;
  const status = h('p', {class: 'muted', role: 'status'});
  const charts = h('div', {class: 'history-grid'});
  const controls = RANGES.map((value) => h('button', {type: 'button', id: `history-range-${value}`,
    'aria-pressed': String(value === range), onclick: () => {
      if (range === value) return;
      range = value;
      try { localStorage.setItem(STORAGE_KEY, range); } catch {}
      for (const control of controls) control.setAttribute('aria-pressed', String(control.textContent === range));
      load();
    }}, value));
  const selector = h('div', {class: 'segmented', role: 'group', 'aria-label': 'History range'}, controls);
  const root = panel('History', {action: selector},
    h('p', {class: 'lede'}, 'Average values with bucket maxima. Times are local; gaps mean no samples were recorded.'),
    status, charts, note('History is best effort and starts after the agent reports. Visible history refreshes every 60 seconds.'));
  const stop = () => { controller?.abort(); clearInterval(timer); timer = undefined; };
  const dispose = () => {
    gone = true;
    stop();
    observer?.disconnect();
    signal?.removeEventListener('abort', dispose);
  };
  // Details reuses this panel across tabs. Detaching pauses its requests/timer;
  // reattaching resumes them, while navigation and 404 dispose it permanently.
  let attached = false;
  const observer = typeof MutationObserver === 'function' ? new MutationObserver(() => {
    if (gone || signal?.aborted) return;
    if (root.isConnected) {
      attached = true;
      if (timer === undefined) { startTimer(); load(); }
    } else if (attached) stop();
  }) : null;
  observer?.observe(document.body, {childList: true, subtree: true});
  signal?.addEventListener('abort', dispose, {once: true});

  async function load() {
    if (gone || signal?.aborted) return;
    controller?.abort();
    const request = new AbortController();
    controller = request;
    const requestedRange = range;
    status.textContent = 'Loading history…';
    charts.setAttribute('aria-busy', 'true');
    // Clear charts when the selected range changes so stale data cannot be
    // mistaken for the new range. Refreshes keep focused charts while loading.
    if (charts.dataset.range !== range) charts.replaceChildren();
    charts.dataset.range = range;
    const metrics = CHARTS.flatMap((item) => item.metrics.map(([metric]) => metric));
    const results = new Map();
    await Promise.all(metrics.map(async (metric) => {
      try {
        const data = await api(`/nodes/${encodeURIComponent(id)}/history?metric=${metric}&range=${requestedRange}`, {signal: request.signal});
        if (data.unit !== CHARTS.find((item) => item.metrics.some(([name]) => name === metric)).unit) throw new Error('The server returned an unsupported history unit.');
        results.set(metric, {data});
      } catch (error) {
        results.set(metric, {error});
        if (error.status === 404 && !request.signal.aborted && !signal?.aborted && controller === request) {
          dispose();
          selector.hidden = true;
          charts.replaceChildren();
          charts.setAttribute('aria-busy', 'false');
          status.replaceChildren('This node no longer exists. ', link('/nodes', 'Go to nodes'));
        }
      }
    }));
    if (request.signal.aborted || signal?.aborted || controller !== request) return;
    // Read focus at completion: the reader may have focused a chart while this request ran.
    const focusedID = charts.contains(document.activeElement) ? document.activeElement.id : null;
    const selectedTime = Number(document.activeElement?.dataset?.selectedTime) || undefined;
    charts.replaceChildren(...CHARTS.map((item) => {
      const error = item.metrics.map(([metric]) => results.get(metric).error).find(Boolean);
      const content = error
        ? h('div', {class: 'history-error'}, h('p', {class: 'muted'}, `Could not load history: ${error.message}`),
          button('Retry', {small: true, id: `history-retry-${item.metrics[0][0]}`, onClick: load}))
        : chart({title: item.title, unit: item.unit, id: `history-chart-${item.metrics[0][0]}`,
          selectedTime: focusedID === `history-chart-${item.metrics[0][0]}` ? selectedTime : undefined,
          series: item.metrics.map(([metric, label]) => {
            const data = results.get(metric).data;
            // Render only well-formed points; anything else is treated as a gap.
            const points = Array.isArray(data.points) ? data.points.filter((point) => point && Number.isFinite(Date.parse(point.t))
              && Number.isFinite(point.avg) && Number.isFinite(point.max)) : [];
            return {label, points, stepSeconds: Number.isFinite(data.step_seconds) && data.step_seconds > 0 ? data.step_seconds : 60};
          })});
      return h('section', {class: 'history-metric', 'aria-label': `${item.title} history`}, h('h3', null, item.title), content);
    }));
    charts.setAttribute('aria-busy', 'false');
    const failures = [...results.values()].filter((result) => result.error).length;
    status.textContent = failures ? 'Some history could not load. Retry below.' : '';
    if (focusedID) {
      const target = document.getElementById(focusedID);
      if (target && root.contains(target)) target.focus({preventScroll: true});
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
