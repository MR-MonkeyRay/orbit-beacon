import {formatList, t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h, icon} from '../../shared/dom.js';
import {usageMeter} from '../../shared/usage.js';
import {createNumberTween} from '../../shared/tween.js';
import {allNodes, api, getNavigationSignal, groupsList, invalidate, nameLookup, usersList} from '../api.js';
import {displayStatus, STATUS, STATUS_ORDER} from '../format.js';
import {connectConsoleLive, readConsoleFleet} from '../live.js';
import {busy, button, emptyState, link, liveStatus, pageHead, panel, statusBadge} from '../ui/components.js';
import {addNodeButton} from '../lib/add-node-dialog.js';
import {auditItem} from '../lib/audit-item.js';
import {activeAlertsSummary} from './alert-events.js';
import {toastError} from '../ui/feedback.js';
import {pagePlaceholder, placeholderHead, placeholderPanel, skLines, skText, skValue} from '../ui/placeholders.js';

function fleetPanel(nodes, signal) {
  const tween = createNumberTween({signal});
  const formatTotal = (value) => String(Math.round(value));
  const total = h('strong', {class: 'num'});
  const label = h('span');
  const bar = h('div', {class: 'fleet-bar', role: 'img'});
  const legend = h('div', {class: 'legend'});
  const states = [...STATUS_ORDER, 'unknown'];
  const refs = new Map(states.map((status) => {
    const segment = h('i', {class: `c-${status === 'stopped' ? 'disabled' : status}`, hidden: true});
    const count = h('strong');
    const entry = link(`/nodes?status=${status}`, h('i', {class: `c-${status === 'stopped' ? 'disabled' : status}`}), STATUS[status], count);
    bar.append(segment);
    legend.append(entry);
    return [status, {segment, count, entry}];
  }));
  const content = h('div', null, h('div', {class: 'fleet-total'}, total, label), bar, legend);
  const empty = emptyState(t('nodes.empty'), null, addNodeButton());
  const element = panel(t('nodes.fleet'), {}, content, empty);
  element.snapshot = (current) => {
    const counts = Object.fromEntries(states.map((status) => [status, 0]));
    for (const node of current) counts[displayStatus(node)] = (counts[displayStatus(node)] || 0) + 1;
    tween.set(total, current.length, formatTotal);
    label.textContent = t('nodes.inventoryCount', {count: current.length});
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
    bar.setAttribute('aria-label', formatList(description));
  };
  element.snapshot(nodes);
  return element;
}

// 只移动排序变化的行；相同节点的链接和用量条保持原 DOM。
function patchRows(list, rows) {
  const retained = new Set(rows);
  for (const row of [...list.children]) if (!retained.has(row)) row.remove();
  rows.forEach((row, index) => { if (list.children[index] !== row) list.insertBefore(row, list.children[index] || null); });
}

export function resourcesPanel(nodes) {
  const columns = ['cpu_percent', 'memory_used_percent'].map((metric, index) => {
    const list = h('ul', {class: 'rows resource-rows'});
    const empty = h('p', {class: 'text-note'}, '—');
    return {metric, list, empty, refs: new Map(), element: h('div', null, h('h3', null, t(index ? 'nodes.memory' : 'nodes.cpu')), list, empty)};
  });
  const element = panel(t('nodes.topUsage'), {}, h('div', {class: 'resource-columns'}, columns.map((column) => column.element)));
  element.snapshot = (current) => {
    for (const {metric, list, empty, refs} of columns) {
      const top = current.filter((node) => displayStatus(node) === 'online' && Number.isFinite(node[metric]))
        .sort((a, b) => b[metric] - a[metric] || a.name.localeCompare(b.name) || a.id.localeCompare(b.id)).slice(0, 5);
      const ids = new Set(top.map((node) => node.id));
      for (const id of refs.keys()) if (!ids.has(id)) refs.delete(id);
      const rows = top.map((node) => {
        if (!refs.has(node.id)) {
          const name = link(`/nodes/${node.id}`, node.name);
          const meter = usageMeter(node[metric]);
          refs.set(node.id, {name, meter, row: h('li', null, name, meter)});
        }
        const {name, meter, row} = refs.get(node.id);
        if (name.textContent !== node.name) name.textContent = node.name;
        meter.update(node[metric]);
        return row;
      });
      patchRows(list, rows);
      list.hidden = !top.length;
      empty.hidden = Boolean(top.length);
    }
  };
  element.snapshot(nodes);
  return element;
}

const DAY = 86400000;
export function attentionPanel(nodes) {
  const list = h('ul', {class: 'rows'});
  const refs = new Map();
  const element = panel(t('nodes.needsAttention'), {action: link('/nodes', t('nodes.allNodes'))}, list);
  // 无需关注时不占面板，只在节点总览里留一行状态。
  element.healthy = h('p', {class: 'overview-status-line', hidden: true}, link('/nodes', icon('check'), t('nodes.allHealthy')));
  element.snapshot = (current, now = Date.now()) => {
    const rows = current.map((node) => {
      const status = displayStatus(node);
      // 到期日包含当天，与 node_expiry_days 的次日 UTC 零点边界一致。
      const remaining = node.expires_on ? (Date.parse(`${node.expires_on}T00:00:00Z`) + DAY - now) / DAY : Infinity;
      const state = ['offline', 'never', 'unknown', 'deleted'].includes(status) ? status : null;
      if (!state && remaining > 7) return null;
      if (!state && !Number.isFinite(remaining)) return null;
      if (!refs.has(node.id)) {
        const name = link(`/nodes/${node.id}`, node.name);
        const reason = h('span', {class: 'attention-reasons'});
        refs.set(node.id, {name, reason, signature: '', row: h('li', null, h('div', {class: 'grow title'}, name), reason)});
      }
      const {name, reason, row} = refs.get(node.id);
      if (name.textContent !== node.name) name.textContent = node.name;
      if (status === 'deleted') {
        name.removeAttribute('href'); delete name.dataset.link;
        name.setAttribute('aria-disabled', 'true'); name.setAttribute('tabindex', '-1');
      } else {
        name.setAttribute('href', `/nodes/${node.id}`); name.dataset.link = '';
        name.removeAttribute('aria-disabled'); name.removeAttribute('tabindex');
      }
      const expiry = Number.isFinite(remaining) && remaining <= 7
        ? t(remaining <= 0 ? 'nodes.expired' : 'nodes.expiresIn', {count: Math.ceil(remaining)}) : '';
      const signature = `${state}|${expiry}`;
      const ref = refs.get(node.id);
      if (ref.signature !== signature) {
        reason.replaceChildren(...[state ? statusBadge(state) : null, expiry ? h('span', {class: 'text-note'}, expiry) : null].filter(Boolean));
        ref.signature = signature;
      }
      return row;
    }).filter(Boolean).slice(0, 8);
    const retained = new Set(rows);
    for (const [id, {row}] of refs) if (!retained.has(row)) refs.delete(id);
    patchRows(list, rows);
    element.hidden = !rows.length;
    element.healthy.hidden = Boolean(rows.length);
    element.parentElement?.classList.toggle('single', element.hidden);
  };
  element.snapshot(nodes);
  return element;
}

// 分组卡片：名称与节点数、与集群条同色的状态条、各状态计数。卡片整块是到筛选后节点列表的链接；
// 实时帧只修补数值与状态条，卡片元素保持不变，键盘焦点与悬停不会因刷新丢失。
export function groupsPanel(nodes, groups) {
  const grid = h('div', {class: 'group-grid'});
  const tiles = new Map();
  const element = panel(t('nodes.groups'), {action: link('/settings#groups', t('nodes.manageGroups'))}, grid);
  const tile = (href, ungrouped) => {
    if (tiles.has(href)) return tiles.get(href);
    const name = h('span', {class: 'group-name'});
    const total = h('span', {class: 'group-total'});
    const description = h('span', {class: 'sr'});
    const bar = h('span', {class: 'group-bar', 'aria-hidden': 'true'});
    const meta = h('span', {class: 'group-meta', 'aria-hidden': 'true'});
    const anchor = link(href, h('span', {class: 'group-title'}, name, h('span', {class: 'num'}, total, description)), bar, meta);
    anchor.className = ungrouped ? 'group-tile ungrouped' : 'group-tile';
    const ref = {anchor, name, total, description, bar, meta, signature: ''};
    tiles.set(href, ref);
    return ref;
  };
  element.snapshot = (current) => {
    const sizes = groups.map((group) => ({label: group.name, href: `/nodes?group=${group.id}`, nodes: current.filter((node) => node.group_id === group.id)}))
      .filter((group) => group.nodes.length).sort((a, b) => b.nodes.length - a.nodes.length).slice(0, 8);
    const ungrouped = current.filter((node) => !node.group_id);
    if (ungrouped.length) sizes.push({label: t('nodes.ungrouped'), href: '/nodes?group=none', nodes: ungrouped, ungrouped: true});
    const anchors = sizes.map((group) => {
      const ref = tile(group.href, group.ungrouped);
      const counts = new Map();
      for (const node of group.nodes) counts.set(displayStatus(node), (counts.get(displayStatus(node)) || 0) + 1);
      const states = [...STATUS_ORDER, 'unknown'].filter((state) => counts.get(state)).map((state) => [state, counts.get(state)]);
      const signature = `${group.label}|${states.join(';')}`;
      if (ref.signature !== signature) {
        ref.signature = signature;
        ref.name.textContent = group.label;
        ref.total.textContent = String(group.nodes.length);
        ref.description.textContent = ` · ${formatList(states.map(([state, count]) => `${STATUS[state]}: ${count}`))}`;
        const color = (state) => `c-${state === 'stopped' ? 'disabled' : state}`;
        ref.bar.replaceChildren(...states.map(([state, count]) => {
          const segment = h('i', {class: color(state)});
          segment.style.flex = String(count);
          return segment;
        }));
        ref.meta.replaceChildren(...states.map(([state, count]) => h('span', null, h('i', {class: color(state)}), h('b', null, count), ' ', STATUS[state])));
      }
      return ref.anchor;
    });
    const retained = new Set(anchors);
    for (const [href, {anchor}] of tiles) if (!retained.has(anchor)) tiles.delete(href);
    patchRows(grid, anchors);
    element.hidden = !sizes.length;
  };
  element.snapshot(nodes);
  return element;
}

// 与概览同构：节点总数、用量两列与最近动态先以占位出现，数据到达后原位填入。
export function overviewPlaceholder() {
  const legend = h('div', {class: 'legend'}, [STATUS.online, STATUS.offline].map((label) => h('span', null, label, ' ', skText('1.5em'))));
  const resourceColumn = (key) => h('div', null, h('h3', null, t(key)),
    h('ul', {class: 'rows resource-rows'}, [70, 52, 38].map((width) => h('li', null, skText(`${width}%`), h('span', {class: 'sk sk-meter', 'aria-hidden': 'true'})))));
  return pagePlaceholder(
    placeholderHead(t('nodes.overview'), {actions: 0}),
    placeholderPanel(t('nodes.fleet'), h('div', {class: 'fleet-total'}, h('strong', {class: 'num'}, skValue()), skText('7em')),
      h('div', {class: 'fleet-bar sk', 'aria-hidden': 'true'}), legend),
    placeholderPanel(t('nodes.topUsage'), h('div', {class: 'resource-columns'}, resourceColumn('nodes.cpu'), resourceColumn('nodes.memory'))),
    h('div', {class: 'grid-2 overview-bottom single'}, placeholderPanel(t('nodes.recentActivity'), skLines('46%', '30%', '52%', '28%', '40%'))));
}

export async function overviewPage() {
  const signal = getNavigationSignal();
  const [nodes, groups, audit, users, alerts] = await Promise.all([allNodes(), groupsList(), api('/audit?limit=8&offset=0'), usersList(),
    api('/alerts/summary', {signal}).then((summary) => ({summary}), (error) => {
      if (signal?.aborted) throw error;
      return {error};
    })]);
  const names = nameLookup(nodes, users, groups);
  const activity = panel(t('nodes.recentActivity'), {action: link('/settings#audit', t('nodes.auditLog'))},
    audit.events.length ? h('ol', {class: 'timeline'}, audit.events.slice(0, 8).map((event) => auditItem(event, names, false)))
      : h('p', {class: 'text-note'}, t('nodes.activityEmpty')));
  const connection = liveStatus();
  const fleet = fleetPanel(nodes, signal);
  const attention = attentionPanel(nodes);
  const resources = resourcesPanel(nodes);
  const distribution = groupsPanel(nodes, groups);
  const statusLines = h('div', {class: 'overview-status'}, attention.healthy);
  if (!alerts.error && alerts.summary.firing.total === 0) statusLines.prepend(h('p', {class: 'overview-status-line'}, link('/alerts', icon('check'), t('nodes.noAlerts'))));
  fleet.querySelector('.panel-body').append(statusLines);
  let known = new Set(nodes.map((node) => node.id));
  let metadata = nodes;
  let currentFleet = nodes;
  let live;
  const patchPanels = (current) => {
    const byID = new Map(current.map((node) => [node.id, node]));
    const merged = metadata.map((node) => byID.has(node.id) ? {...node, ...byID.get(node.id)} : {...node, presence: 'deleted', stopped: false});
    attention.snapshot(merged);
    const present = merged.filter((node) => node.presence !== 'deleted');
    resources.snapshot(present);
    distribution.snapshot(present);
  };
  const patchInventoryNotice = (current) => {
    const ids = new Set(current.map((node) => node.id));
    inventoryNotice.hidden = ids.size === known.size && [...ids].every((id) => known.has(id));
  };
  const refreshNodes = button(t('nodes.refreshList'), {small: true, onClick: async () => {
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
      patchPanels(current);
      patchInventoryNotice(current);
    } catch (error) { if (!signal?.aborted) toastError(error); }
    finally {
      busy(refreshNodes, false);
      if (!signal?.aborted) live = subscribe();
    }
  }});
  const inventoryNotice = h('div', {class: 'table-foot live-change', hidden: true, 'aria-live': 'polite'}, t('nodes.inventoryChanged'), refreshNodes);
  const root = h('div', null,
    pageHead(t('nodes.overview'), connection.element),
    fleet,
    alerts.error ? panel(t('nodes.activeAlerts'), {action: link('/alerts', t('nodes.viewAlerts'))}, h('p', {role: 'status', class: 'text-note'}, t('nodes.alertsLoadError', {error: errorText(alerts.error)}))) : alerts.summary.firing.total ? activeAlertsSummary(alerts.summary) : null,
    resources,
    h('div', {class: `grid-2 overview-bottom${attention.hidden ? ' single' : ''}`}, attention, activity), inventoryNotice,
    distribution);
  const subscribe = () => connectConsoleLive({signal, onState: connection.update, onSnapshot(data) {
    currentFleet = data.nodes;
    fleet.snapshot(currentFleet);
    patchPanels(currentFleet);
    patchInventoryNotice(currentFleet);
  }});
  live = subscribe();
  return root;
}
