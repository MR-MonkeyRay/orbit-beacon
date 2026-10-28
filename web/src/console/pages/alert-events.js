import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {when} from '../format.js';
import {busy, button, link, panel} from '../ui/components.js';
import {formatMetricValue, severityLabel} from './alert-utils.js';

export const severityPill = (severity) => h('span', {class: `badge alert-${['info', 'warning', 'critical'].includes(severity) ? severity : 'unknown'}`}, severityLabel(severity));
export const eventTime = (iso) => h('div', {class: 'alert-time'}, when(iso), h('small', {class: 'muted'}, Number.isFinite(Date.parse(iso)) ? new Date(iso).toLocaleString() : 'Unknown time'));
export function eventRows(events) {
  return events.map((event) => h('tr', null,
    h('td', null, severityPill(event.severity)),
    h('td', null, link(`/alerts?event=${encodeURIComponent(event.id)}#history`, event.rule_name || 'Deleted rule')),
    h('td', null, link(`/nodes/${encodeURIComponent(event.node_id)}`, event.node_name || event.node_id)),
    h('td', null, formatMetricValue(event.metric, event.value)),
    h('td', null, event.state === 'firing' ? 'Firing' : 'Resolved'),
    h('td', null, eventTime(event.started_at)),
    h('td', null, event.resolved_at ? eventTime(event.resolved_at) : '—')));
}

export function eventsPanel(state, signal) {
  const active = state === 'firing';
  const rows = h('tbody');
  const table = h('div', {class: 'table-wrap', hidden: true}, h('table', {class: 'table'},
    h('caption', {class: 'sr'}, active ? 'Firing alerts, newest first' : 'Alert history, newest first'),
    h('thead', null, h('tr', null, ['Severity', 'Rule', 'Node', 'Value', 'State', 'Started', 'Resolved'].map((label) => h('th', {scope: 'col'}, label)))), rows));
  const status = h('p', {class: 'muted', role: 'status'}, 'Loading alerts…');
  let cursor = '';
  let loading = false;
  let request;
  let stopped = false;
  const seen = new Set();
  const cursors = new Set();
  const load = async (reset = false) => {
    if (loading || stopped || signal?.aborted) return;
    loading = true;
    request = new AbortController();
    busy(more, true);
    busy(reload, true);
    table.setAttribute('aria-busy', 'true');
    status.textContent = 'Loading alerts…';
    try {
      const page = await api(`/alerts?state=${state}&limit=50${!reset && cursor ? `&before=${encodeURIComponent(cursor)}` : ''}`, {signal: request.signal});
      if (stopped || signal?.aborted) return;
      if (page.has_more && (!page.next_cursor || (!reset && cursors.has(page.next_cursor)))) throw new Error('The server returned an invalid alert cursor.');
      if (reset) { rows.replaceChildren(); seen.clear(); cursors.clear(); }
      const fresh = page.events.filter((event) => !seen.has(event.id));
      fresh.forEach((event) => seen.add(event.id));
      const focusedMore = document.activeElement === more;
      const newRows = eventRows(fresh);
      rows.append(...newRows);
      if (focusedMore && newRows.length) { newRows[0].tabIndex = -1; newRows[0].focus(); }
      cursor = page.next_cursor || '';
      if (cursor) cursors.add(cursor);
      more.hidden = !page.has_more;
      table.hidden = seen.size === 0;
      status.textContent = seen.size ? `${seen.size} ${active ? 'firing alerts' : 'events'} loaded.` : active ? 'No firing alerts.' : 'No alert history yet.';
    } catch (error) {
      if (!stopped && !signal?.aborted && error.name !== 'AbortError') status.textContent = `Could not load alerts: ${error.message} Use Refresh to retry.`;
    } finally {
      loading = false;
      busy(more, false);
      busy(reload, false);
      table.setAttribute('aria-busy', 'false');
    }
  };
  const more = button('Load more', {onClick: () => load(), small: true});
  more.hidden = true;
  const reload = button('Refresh', {small: true, icon: 'refresh', onClick: () => load(true)});
  const root = panel(active ? 'Active alerts' : 'History', {action: reload}, status, table, more);
  const stop = () => { stopped = true; request?.abort(); observer?.disconnect(); signal?.removeEventListener('abort', stop); };
  // A tab can detach without navigation. Cancel its pending request as well.
  let attached = false;
  const observer = typeof MutationObserver === 'function' ? new MutationObserver(() => {
    if (root.isConnected) attached = true;
    else if (attached) stop();
  }) : null;
  observer?.observe(document.body, {childList: true, subtree: true});
  signal?.addEventListener('abort', stop, {once: true});
  if (signal?.aborted) stop();
  else load();
  return root;
}

export function activeAlertsSummary({firing, newest}) {
  const counts = ['critical', 'warning', 'info', 'unknown'].map((severity) => {
    const count = firing[severity];
    return count ? h('span', {class: 'alert-count'}, severityPill(severity), h('strong', null, count)) : null;
  });
  return panel('Active alerts', {action: link('/alerts', `View alerts (${firing.total})`)},
    firing.total ? [h('div', {class: 'alert-counts'}, counts),
      h('ul', {class: 'rows'}, newest.map((event) => h('li', null,
        h('div', {class: 'grow'}, h('div', {class: 'title'}, link(`/alerts?event=${encodeURIComponent(event.id)}#history`, event.rule_name || 'Deleted rule')),
          h('div', {class: 'sub'}, link(`/nodes/${encodeURIComponent(event.node_id)}`, event.node_name || event.node_id), ' · ', formatMetricValue(event.metric, event.value), ' · ', when(event.started_at))),
        severityPill(event.severity))))] : h('p', {class: 'muted'}, 'No firing alerts.'));
}
