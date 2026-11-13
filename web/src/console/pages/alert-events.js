import {h} from '../../shared/dom.js';
import {t, tParts} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {allNodes, api, groupsList} from '../api.js';
import {toastError} from '../ui/feedback.js';
import {silenceEditor} from './alert-silences.js';
import {absoluteWhen, formatDuration, when} from '../format.js';
import {busy, button, dataTable, emptyState, iconButton, link, panel, statusBadge} from '../ui/components.js';
import {nextCursor} from '../ui/pager.js';
import {formatMetricValue, severityLabel, sortActiveEvents} from '../lib/alert-utils.js';
import {panelLifecycle} from '../ui/polling-panel.js';

export const silencedBadge = () => {
  const badge = statusBadge('disabled');
  badge.textContent = t('alerts.silences.silenced');
  return badge;
};
export const severityPill = (severity) => h('span', {class: `badge alert-${['info', 'warning', 'critical'].includes(severity) ? severity : 'unknown'}`}, severityLabel(severity));
export const eventTime = (iso) => h('div', {class: 'alert-time'}, when(iso), h('small', {class: 'muted'}, Number.isFinite(Date.parse(iso)) ? absoluteWhen(iso) : t('alerts.unknownTime')));
const eventSummary = (event) => {
  return tParts('alerts.events.summary', {
    node: link(`/nodes/${encodeURIComponent(event.node_id)}`, event.node_name || event.node_id),
    value: formatMetricValue(event.metric, event.value), time: when(event.started_at)
  });
};
export function eventRows(events, {active = false, rules = [], silence} = {}) {
  return events.map((event) => {
    const rule = rules.find((rule) => rule.id === event.rule_id && rule.metric === event.metric);
    const value = formatMetricValue(event.metric, event.value);
    return h('tr', {dataset: {eventId: event.id}},
      h('td', null, severityPill(event.severity), active && event.silenced ? [' ', silencedBadge()] : null),
      h('td', null, link(`/alerts?event=${encodeURIComponent(event.id)}#history`, event.rule_name || t('alerts.deletedRule')), event.target ? h('div', null, h('code', {class: 'probe-target mono'}, event.target)) : null),
      h('td', null, link(`/nodes/${encodeURIComponent(event.node_id)}`, event.node_name || event.node_id)),
      h('td', null, active && ['>', '<'].includes(rule?.comparator) && Number.isFinite(rule.threshold) ? `${value} ${rule.comparator} ${formatMetricValue(event.metric, rule.threshold)}` : value),
      active ? null : h('td', null, t(event.state === 'firing' ? 'alerts.state.firing' : 'alerts.state.resolved'), event.silenced ? [' ', silencedBadge()] : null),
      h('td', null, eventTime(event.started_at)),
      active ? h('td', null, formatDuration((Date.now() - Date.parse(event.started_at)) / 1000)) : h('td', null, event.resolved_at ? eventTime(event.resolved_at) : '—'),
      active ? h('td', {class: 'actions'}, button(t('alerts.events.silence'), {small: true, onClick: () => silence(event)})) : null);
  });
}

export function eventsPanel(state, signal, {getRules = () => []} = {}) {
  const active = state === 'firing';
  const rows = h('tbody');
  const columns = active ? ['severity', 'rule', 'node', 'value', 'started', 'duration', 'actions'] : ['severity', 'rule', 'node', 'value', 'state', 'started', 'resolved'];
  const table = dataTable({hidden: true, caption: t(active ? 'alerts.events.activeCaption' : 'alerts.events.historyCaption'),
    columns: columns.map((key) => t(`alerts.column.${key}`)), body: rows});
  const status = h('p', {class: 'text-note', role: 'status'}, t('alerts.events.loading'));
  const updated = h('span', {class: 'muted alert-updated'});
  let severity = 'all', grouping = 'none';
  if (active) {
    try {
      const prefs = JSON.parse(localStorage.getItem('orbitbeacon-alerts-view'));
      if (['all', 'critical', 'warning', 'info'].includes(prefs?.severity)) severity = prefs.severity;
      if (['none', 'rule', 'node'].includes(prefs?.grouping)) grouping = prefs.grouping;
    } catch {}
  }
  let events = [];
  let cursor = '';
  let loading = false;
  let request;
  let stopped = false;
  let epoch = 0;
  const seen = new Set();
  const cursors = new Set();
  const remember = () => {
    try { localStorage.setItem('orbitbeacon-alerts-view', JSON.stringify({severity, grouping})); } catch {}
    render();
  };
  // 与节点列表一致的分段控件：级别带计数，分组方式紧随其后；更新时间与刷新靠右。
  const severityButtons = ['all', 'critical', 'warning', 'info'].map((key) => h('button', {type: 'button', dataset: {severity: key},
    onclick: () => { severity = key; remember(); }}, key === 'all' ? t('alerts.events.all') : severityLabel(key), h('span', {class: 'count'})));
  const groupButtons = ['none', 'rule', 'node'].map((key) => h('button', {type: 'button', dataset: {grouping: key},
    onclick: () => { grouping = key; remember(); }}, t(`alerts.events.group.${key}`)));
  const silence = async (event) => {
    try {
      const [groups, nodes] = await Promise.all([groupsList(), allNodes()]);
      if (stopped || signal?.aborted) return;
      silenceEditor({groups, nodes: nodes.some((node) => node.id === event.node_id) ? nodes : [...nodes, {id: event.node_id, name: event.node_name || event.node_id}],
        scope_kind: 'node', scope_id: event.node_id, signal, saved: () => { request?.abort(); ++epoch; loading = false; return load(true); }});
    } catch (error) { if (!stopped && !signal?.aborted) toastError(error); }
  };
  const render = () => {
    if (!active) return;
    severityButtons.forEach((control) => {
      const key = control.dataset.severity;
      control.lastElementChild.textContent = key === 'all' ? events.length : events.filter((event) => event.severity === key).length;
      control.setAttribute('aria-pressed', String(severity === key));
    });
    groupButtons.forEach((control) => control.setAttribute('aria-pressed', String(grouping === control.dataset.grouping)));
    const filtered = sortActiveEvents(events).filter((event) => severity === 'all' || event.severity === severity);
    const groups = new Map();
    for (const event of filtered) {
      const key = grouping === 'rule' ? event.rule_id : grouping === 'node' ? event.node_id : '';
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(event);
    }
    const focusedRow = document.activeElement?.closest?.('tr[data-event-id]');
    const focusedID = focusedRow && table.contains(focusedRow) ? focusedRow.dataset.eventId : null;
    const inner = table.querySelector('table');
    inner.querySelectorAll('tbody').forEach((body) => body.remove());
    for (const group of groups.values()) {
      const first = group[0];
      inner.append(h('tbody', null, grouping === 'none' ? null : h('tr', {class: 'alert-group'},
        h('th', {scope: 'rowgroup', colspan: columns.length},
          h('span', {class: 'alert-count'}, grouping === 'rule' ? first.rule_name || t('alerts.deletedRule') : first.node_name || first.node_id, ` (${group.length})`, severityPill(first.severity)))),
      eventRows(group, {active, rules: getRules(), silence})));
    }
    if (focusedID) {
      const replacement = [...inner.querySelectorAll('tr')].find((row) => row.dataset.eventId === focusedID);
      (replacement?.querySelector('button') || reload).focus({preventScroll: true});
    }
    table.hidden = filtered.length === 0;
    empty.hidden = filtered.length > 0;
    status.textContent = '';
  };
  const load = async (reset = active) => {
    if (loading || stopped || signal?.aborted || (active && document.hidden)) return;
    loading = true;
    const current = ++epoch;
    request = new AbortController();
    busy(more, true); busy(reload, true);
    table.setAttribute('aria-busy', 'true');
    status.textContent = t('alerts.events.loading');
    try {
      // 当前告警取完所有分页后再排序，筛选计数与分组不受服务端时间分页影响。
      let before = !reset ? cursor : '';
      const visited = new Set(reset ? [] : cursors);
      const fetched = [];
      let page, next;
      do {
        page = await api(`/alerts?state=${state}&limit=50${before ? `&before=${encodeURIComponent(before)}` : ''}`, {signal: request.signal});
        if (stopped || signal?.aborted || current !== epoch) return;
        next = nextCursor(page, before || null, visited);
        fetched.push(...page.events);
        if (next) visited.add(next);
        before = next;
      } while (active && next);
      if (reset) { rows.replaceChildren(); seen.clear(); cursors.clear(); events = []; }
      const fresh = fetched.filter((event) => { if (seen.has(event.id)) return false; seen.add(event.id); return true; });
      events.push(...fresh);
      const focusedMore = document.activeElement === more;
      const newRows = active ? [] : eventRows(fresh);
      if (active) render(); else rows.append(...newRows);
      if (focusedMore && newRows.length) { newRows[0].tabIndex = -1; newRows[0].focus(); }
      cursor = next || '';
      if (cursor) cursors.add(cursor);
      more.hidden = active || !page.has_more;
      if (!active) { table.hidden = seen.size === 0; status.textContent = seen.size ? '' : t('alerts.events.noHistory'); }
      const now = new Date().toISOString();
      updated.replaceChildren(absoluteWhen(now));
    } catch (error) {
      if (!stopped && !signal?.aborted && current === epoch && error.name !== 'AbortError') {
        empty.hidden = true;
        status.textContent = t('alerts.events.loadFailed', {error: errorText(error)});
      }
    } finally {
      if (current === epoch) {
        loading = false;
        busy(more, false); busy(reload, false);
        table.setAttribute('aria-busy', 'false');
      }
    }
  };
  const more = button(t('common.loadMore'), {onClick: () => load(false), small: true});
  more.hidden = true;
  const reload = iconButton('refresh', t('common.refresh'), () => load(true));
  const empty = emptyState(t('alerts.events.noActive'));
  empty.hidden = true;
  const toolbar = h('div', {class: 'toolbar alert-toolbar'},
    active ? h('div', {class: 'segmented', role: 'group', 'aria-label': t('alerts.column.severity')}, severityButtons) : null,
    active ? h('div', {class: 'segmented', role: 'group', 'aria-label': t('alerts.events.grouping')}, groupButtons) : null,
    h('div', {class: 'alert-refresh'}, updated, reload));
  const root = panel(null, {}, toolbar, status, active ? empty : null, table, more);
  root.classList.add('alert-events');
  const visibility = () => {
    if (document.hidden) {
      request?.abort(); ++epoch; loading = false;
      busy(more, false); busy(reload, false); table.setAttribute('aria-busy', 'false');
    } else load(true);
  };
  if (active) {
    render();
    // 规则列表晚于事件到达时补上阈值。
    root.rerender = render;
    document.addEventListener('visibilitychange', visibility);
  }
  panelLifecycle({root, signal, load, poll: active, interval: 30000, resume: false,
    cancel: () => { stopped = true; ++epoch; request?.abort(); document.removeEventListener('visibilitychange', visibility); }});
  return root;
}

export function activeAlertsSummary({firing, newest}) {
  const counts = ['critical', 'warning', 'info', 'unknown'].map((severity) => {
    const count = firing[severity];
    return count ? h('span', {class: 'alert-count'}, severityPill(severity), h('strong', null, count)) : null;
  });
  return panel(t('alerts.events.activeTitle'), {action: link('/alerts', t('alerts.events.viewAlerts', {count: firing.total}))},
    firing.total ? [h('div', {class: 'alert-counts'}, counts),
      h('ul', {class: 'rows'}, sortActiveEvents(newest).map((event) => h('li', null,
        h('div', {class: 'grow'}, h('div', {class: 'title'}, link(`/alerts?event=${encodeURIComponent(event.id)}#history`, event.rule_name || t('alerts.deletedRule')), event.target ? h('div', null, h('code', {class: 'probe-target mono'}, event.target)) : null),
          h('div', {class: 'sub'}, eventSummary(event))),
        severityPill(event.severity))))] : h('p', {class: 'text-note'}, t('alerts.events.noActive')));
}
