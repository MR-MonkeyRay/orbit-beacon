import {h, icon} from '../../shared/dom.js';
import {allNodes, api, ensureSession, getNavigationSignal, groupsList, invalidate, nameLookup, usersList} from '../api.js';
import {absoluteWhen, STATUS, STATUS_ORDER} from '../format.js';
import {connectConsoleLive, readConsoleFleet} from '../live.js';
import {busy, button, emptyState, link, liveStatus, pageHead, panel, patchStatusBadge, statusBadge} from '../ui/components.js';
import {addNodeButton, auditItem} from './shared.js';
import {activeAlertsSummary} from './alert-events.js';
import {toastError} from '../ui/feedback.js';

function fleetPanel(nodes) {
  const total = h('strong', {class: 'num'});
  const label = h('span');
  const bar = h('div', {class: 'fleet-bar', role: 'img'});
  const legend = h('div', {class: 'legend'});
  const states = [...STATUS_ORDER, 'unknown'];
  const refs = new Map(states.map((status) => {
    const segment = h('i', {class: `c-${status}`, hidden: true});
    const count = h('strong');
    const entry = link(`/nodes?status=${status}`, h('i', {class: `c-${status}`}), STATUS[status], count);
    bar.append(segment);
    legend.append(entry);
    return [status, {segment, count, entry}];
  }));
  const content = h('div', null, h('div', {class: 'fleet-total'}, total, label), bar, legend);
  const empty = emptyState('No nodes yet', 'Add a node, then enroll its agent with a credential from the management API.', addNodeButton());
  const element = panel('Fleet', {}, content, empty);
  element.snapshot = (current) => {
    const counts = Object.fromEntries(states.map((status) => [status, 0]));
    for (const node of current) counts[node.presence] = (counts[node.presence] || 0) + 1;
    total.textContent = current.length;
    label.textContent = current.length === 1 ? 'node in inventory' : 'nodes in inventory';
    content.hidden = current.length === 0;
    empty.hidden = current.length !== 0;
    const description = [];
    for (const [status, {segment, count, entry}] of refs) {
      const value = counts[status];
      segment.hidden = entry.hidden = value === 0;
      segment.style.flex = String(value);
      segment.title = `${STATUS[status]}: ${value}`;
      count.textContent = value;
      if (value) description.push(`${value} ${STATUS[status].toLowerCase()}`);
    }
    bar.setAttribute('aria-label', description.join(', '));
  };
  element.snapshot(nodes);
  return element;
}

function newestPanel(nodes, groupNames) {
  const list = h('ul', {class: 'rows'});
  const empty = h('p', {class: 'muted'}, 'Nodes you add appear here.');
  const refs = new Map();
  const element = panel('Newest nodes', {action: link('/nodes', 'All nodes')}, list, empty);
  element.reload = (current, snapshot = current) => {
    refs.clear();
    const newest = [...current].sort((a, b) => b.created_at.localeCompare(a.created_at)).slice(0, 6);
    list.replaceChildren(...newest.map((node) => {
      const name = link(`/nodes/${node.id}`, node.name);
      const seen = h('span');
      const badge = statusBadge(node.presence);
      refs.set(node.id, {name, seen, badge});
      return h('li', null,
        h('div', {class: 'grow'}, h('div', {class: 'title'}, name), h('div', {class: 'sub'},
          node.group_id ? groupNames.get(node.group_id) || 'Unknown group' : 'Ungrouped', seen)), badge);
    }));
    list.hidden = newest.length === 0;
    empty.hidden = newest.length !== 0;
    element.snapshot(snapshot);
  };
  element.snapshot = (current) => {
    const byID = new Map(current.map((node) => [node.id, node]));
    for (const [id, {name, seen, badge}] of refs) {
      const node = byID.get(id);
      patchStatusBadge(badge, node ? node.presence : 'deleted');
      if (!node) {
        name.removeAttribute('href');
        delete name.dataset.link;
        name.setAttribute('aria-disabled', 'true');
        name.setAttribute('tabindex', '-1');
      } else seen.replaceChildren(...(node.last_seen ? [', last seen ', absoluteWhen(node.last_seen)] : [', never seen']));
    }
  };
  element.reload(nodes);
  return element;
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
  const connection = liveStatus();
  const fleet = fleetPanel(nodes);
  const newest = newestPanel(nodes, new Map(groups.map((group) => [group.id, group.name])));
  let known = new Set(nodes.map((node) => node.id));
  let metadata = nodes;
  let currentFleet = nodes;
  let live;
  const patchInventoryNotice = (current) => {
    const ids = new Set(current.map((node) => node.id));
    inventoryNotice.hidden = ids.size === known.size && [...ids].every((id) => known.has(id));
  };
  const refreshNodes = button('Refresh list', {small: true, onClick: async () => {
    if (refreshNodes.disabled) return;
    busy(refreshNodes, true);
    const previousIDs = new Set(currentFleet.map((node) => node.id));
    live.close();
    connection.update({state: 'stale'});
    try {
      invalidate('nodes');
      const fresh = await allNodes();
      if (signal?.aborted) return;
      const current = (await readConsoleFleet(signal)).nodes;
      if (signal?.aborted) return;
      const fetchedIDs = new Set(fresh.map((node) => node.id));
      const currentIDs = new Set(current.map((node) => node.id));
      const retained = metadata.filter((node) => previousIDs.has(node.id) && !currentIDs.has(node.id) && !fetchedIDs.has(node.id));
      metadata = [...fresh, ...retained];
      known = new Set(metadata.map((node) => node.id));
      currentFleet = current;
      fleet.snapshot(current);
      newest.reload(metadata, current);
      patchInventoryNotice(current);
    } catch (error) { if (!signal?.aborted) toastError(error); }
    finally {
      busy(refreshNodes, false);
      if (!signal?.aborted) live = subscribe();
    }
  }});
  const inventoryNotice = h('div', {class: 'table-foot live-change', hidden: true, 'aria-live': 'polite'}, 'Inventory changed. ', refreshNodes);
  const root = h('div', null,
    pageHead('Overview', 'Fleet inventory and the latest console activity.', connection.element, addNodeButton(), h('a', {class: 'btn', href: '/'}, icon('external'), 'Open public page')),
    fleet,
    alerts.error ? panel('Active alerts', {action: link('/alerts', 'View alerts')}, h('p', {role: 'status', class: 'muted'}, `Could not load alerts: ${alerts.error.message}`)) : activeAlertsSummary(alerts.summary),
    h('div', {class: 'grid-2'}, newest, activity), inventoryNotice,
    groupsPanel(nodes, groups));
  const subscribe = () => connectConsoleLive({signal, onState: connection.update, onSnapshot(data) {
    currentFleet = data.nodes;
    fleet.snapshot(currentFleet);
    newest.snapshot(currentFleet);
    patchInventoryNotice(currentFleet);
  }});
  live = subscribe();
  return root;
}
