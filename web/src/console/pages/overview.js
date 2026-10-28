import {h, icon} from '../../shared/dom.js';
import {allNodes, api, ensureSession, getNavigationSignal, groupsList, nameLookup, usersList} from '../api.js';
import {STATUS, STATUS_ORDER, when} from '../format.js';
import {emptyState, link, note, pageHead, panel, statusBadge} from '../ui/components.js';
import {addNodeButton, auditItem} from './shared.js';
import {activeAlertsSummary} from './alert-events.js';

function fleetPanel(nodes) {
  if (nodes.length === 0) {
    return panel('Fleet', {}, emptyState('No nodes yet', 'Add a node, then enroll its agent with a credential from the management API.', addNodeButton()));
  }
  const counts = Object.fromEntries(STATUS_ORDER.map((status) => [status, nodes.filter((node) => node.presence === status).length]));
  const present = STATUS_ORDER.filter((status) => counts[status] > 0);
  return panel('Fleet', {},
    h('div', {class: 'fleet-total'}, h('strong', {class: 'num'}, nodes.length), h('span', null, nodes.length === 1 ? 'node in inventory' : 'nodes in inventory')),
    h('div', {class: 'fleet-bar', role: 'img', 'aria-label': present.map((status) => `${counts[status]} ${STATUS[status].toLowerCase()}`).join(', ')},
      present.map((status, index) => h('i', {class: `c-${status}`, style: `flex:${counts[status]};animation-delay:${index * 70}ms`, title: `${STATUS[status]}: ${counts[status]}`}))),
    h('div', {class: 'legend'}, present.map((status) => link(`/nodes?status=${status}`, h('i', {class: `c-${status}`}), STATUS[status], h('strong', null, counts[status])))),
    note('Presence is derived from accepted agent reports and may lag by about 30 seconds.'));
}

function newestPanel(nodes, groupNames) {
  const newest = [...nodes].sort((a, b) => b.created_at.localeCompare(a.created_at)).slice(0, 6);
  return panel('Newest nodes', {action: nodes.length ? link('/nodes', 'All nodes') : null},
    newest.length ? h('ul', {class: 'rows'}, newest.map((node) => h('li', null,
      h('div', {class: 'grow'}, h('div', {class: 'title'}, link(`/nodes/${node.id}`, node.name)),
        h('div', {class: 'sub'}, node.group_id ? groupNames.get(node.group_id) || 'Unknown group' : 'Ungrouped', node.last_seen ? [', last seen ', when(node.last_seen)] : ', never seen')),
      statusBadge(node.presence))))
      : h('p', {class: 'muted'}, 'Nodes you add appear here.'));
}

function groupsPanel(nodes, groups) {
  if (!groups.length) return null;
  const sizes = groups.map((group) => [group, nodes.filter((node) => node.group_id === group.id).length]).sort((a, b) => b[1] - a[1]).slice(0, 8);
  const ungrouped = nodes.filter((node) => !node.group_id).length;
  const max = Math.max(1, ungrouped, ...sizes.map(([, size]) => size));
  const row = (label, size, href, index) => h('div', {class: 'meter-row'},
    link(href, label), h('span', {class: 'num muted'}, size),
    h('div', {class: 'bar'}, h('i', {style: `width:${size / max * 100}%;animation-delay:${index * 50}ms`})));
  return panel('Groups', {action: link('/settings#groups', 'Manage groups')},
    sizes.map(([group, size], index) => row(group.name, size, `/nodes?group=${group.id}`, index)),
    row('Ungrouped', ungrouped, '/nodes?group=none', sizes.length));
}

export async function overviewPage() {
  const signal = getNavigationSignal();
  await ensureSession();
  const [nodes, groups, audit, users, alerts] = await Promise.all([allNodes(), groupsList(), api('/audit?limit=8&offset=0'), usersList(),
    api('/alerts/summary', {signal}).then((summary) => ({summary}), (error) => {
      if (signal?.aborted) throw error;
      return {error};
    })]);
  const names = nameLookup(nodes, users, groups);
  const activity = panel('Recent activity', {action: link('/settings#audit', 'Audit log')},
    audit.events.length ? h('ol', {class: 'timeline'}, audit.events.map((event) => auditItem(event, names, false)))
      : h('p', {class: 'muted'}, 'Console changes are recorded here.'));
  return h('div', null,
    pageHead('Overview', 'Fleet inventory and the latest console activity.', addNodeButton(), h('a', {class: 'btn', href: '/'}, icon('external'), 'Open public page')),
    fleetPanel(nodes),
    alerts.error ? panel('Active alerts', {action: link('/alerts', 'View alerts')}, h('p', {role: 'status', class: 'muted'}, `Could not load alerts: ${alerts.error.message}`)) : activeAlertsSummary(alerts.summary),
    h('div', {class: 'grid-2'}, newestPanel(nodes, new Map(groups.map((group) => [group.id, group.name]))), activity),
    groupsPanel(nodes, groups));
}
