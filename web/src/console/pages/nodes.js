import {t} from '../i18n.js';
import {h, icon, reducedMotion} from '../../shared/dom.js';
import {allNodes, getNavigationSignal, groupsList, invalidate} from '../api.js';
import {STATUS, STATUS_ORDER} from '../format.js';
import {connectConsoleLive, readConsoleFleet} from '../live.js';
import {busy, button, emptyState, liveStatus, pageHead} from '../ui/components.js';
import {toastError} from '../ui/feedback.js';
import {input, select} from '../ui/forms.js';
import {runNodeBatch} from '../lib/nodes-bulk.js';
import {NODE_PAGE_SIZE, nodeSortDirection, readNodeFilter, scopedNodes, sortedNodes, writeNodeFilter} from '../lib/nodes-filters.js';
import {patchNodesFromFleet} from '../lib/nodes-live.js';
import {createNodeRow, patchNodeSortHeaders} from '../lib/nodes-table.js';
import {addNodeButton} from '../lib/add-node-dialog.js';

export async function nodesPage() {
  const signal = getNavigationSignal();
  const [nodes, groups] = await Promise.all([allNodes(), groupsList()]);
  const connection = liveStatus();
  const head = pageHead(t('nodes.listTitle'), t('nodes.listIntro'), connection.element, addNodeButton());

  const filter = readNodeFilter();
  const groupNames = new Map(groups.map((group) => [group.id, group.name]));
  const searchText = (node) => [node.name, node.region, node.provider, node.notes, groupNames.get(node.group_id) || '', ...node.tags].join('\n').toLowerCase();
  const indexed = nodes.map((node) => ({node, text: searchText(node)}));
  const deleted = new Set();
  const rowRefs = new Map();
  let statusChanged = false;
  let inventoryChanged = false;
  let selectionRemoved = false;
  let live;
  let reloadQueue = Promise.resolve();

  const search = input({type: 'search', id: 'node-search', placeholder: t('nodes.searchPlaceholder'), value: filter.q, 'aria-label': t('nodes.search'), autocomplete: 'off'});
  let frame = 0;
  search.addEventListener('input', () => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => { filter.q = search.value; filter.page = 0; update(true); });
  });

  const statusButtons = new Map(['', ...STATUS_ORDER].map((status) => [status, h('button', {type: 'button',
    onclick: () => { filter.status = status; filter.page = 0; update(); }}, status ? STATUS[status] : t('nodes.all'), h('span', {class: 'count'}))]));

  const groupChoices = [['', t('nodes.allGroups')], ['none', t('nodes.ungrouped')], ...groups.map((group) => [group.id, group.name])];
  const groupSelect = select(groupChoices, groupChoices.some(([id]) => id === filter.group) ? filter.group : '', {'aria-label': t('nodes.filterGroup')});
  groupSelect.addEventListener('change', () => { filter.group = groupSelect.value; filter.page = 0; update(); });

  const direction = () => nodeSortDirection(filter);
  const sortable = (key, label) => h('th', {scope: 'col', dataset: {sort: key}}, h('button', {type: 'button', onclick: () => {
    filter.dir = filter.sort === key ? (direction() === 'asc' ? 'desc' : 'asc') : key === 'created' ? 'desc' : 'asc';
    filter.sort = key;
    update();
  }}, label, icon('up')));
  const selectAll = h('input', {type: 'checkbox', 'aria-label': t('nodes.selectAll')});
  const headers = [h('th', {scope: 'col', class: 'selection-cell'}, selectAll),
    sortable('name', t('nodes.column.name')), sortable('status', t('nodes.presence')), h('th', {scope: 'col', class: 'hide-compact'}, t('nodes.lastSeen')), h('th', {scope: 'col', class: 'hide-compact'}, t('nodes.group')),
    h('th', {scope: 'col', class: 'hide-compact'}, t('nodes.location')), h('th', {scope: 'col', class: 'hide-compact'}, t('nodes.tags')), sortable('created', t('nodes.added'))];

  const body = h('tbody');
  const summary = h('span', {'aria-live': 'polite'});
  let summaryTimer;
  const turn = (delta) => {
    const control = delta < 0 ? previous : next;
    const restoreFocus = document.activeElement === control;
    filter.page += delta;
    update();
    if (restoreFocus && control.disabled) (delta < 0 ? next : previous).focus();
    window.scrollTo({top: 0, behavior: reducedMotion.matches ? 'auto' : 'smooth'});
  };
  const previous = button(t('nodes.previous'), {icon: 'left', small: true, onClick: () => turn(-1)});
  const next = button(t('nodes.next'), {trailingIcon: 'right', small: true, onClick: () => turn(1)});
  const pager = h('div', {class: 'pager'}, previous, next);
  const clear = () => {
    Object.assign(filter, {q: '', status: '', group: '', page: 0});
    search.value = '';
    groupSelect.value = '';
    update();
    search.focus();
  };

  const selected = new Set();
  let visibleIDs = new Set();
  let batchBusy = false;
  const selectionCount = h('span', {class: 'selection-count', 'aria-live': 'polite'});
  const disableSelected = button(t('nodes.disable'), {small: true, onClick: () => runBatch('disable')});
  const enableSelected = button(t('nodes.enable'), {small: true, onClick: () => runBatch('enable')});
  const deleteSelected = button(t('nodes.delete'), {small: true, variant: 'danger', onClick: () => runBatch('delete')});
  const actionBar = h('div', {class: 'bulk-actions', hidden: true}, selectionCount, disableSelected, enableSelected, deleteSelected);

  const updateSelection = () => {
    const count = selected.size;
    const text = t('nodes.selected', {count});
    if (selectionCount.textContent !== text) selectionCount.textContent = text;
    actionBar.hidden = count === 0;
    const visibleSelected = [...visibleIDs].filter((id) => selected.has(id)).length;
    selectAll.checked = visibleIDs.size > 0 && visibleSelected === visibleIDs.size;
    selectAll.indeterminate = visibleSelected > 0 && visibleSelected < visibleIDs.size;
    for (const [id, refs] of rowRefs) refs.element.classList.toggle('selected', selected.has(id));
  };
  selectAll.addEventListener('change', () => {
    for (const id of visibleIDs) {
      if (selectAll.checked) selected.add(id);
      else selected.delete(id);
    }
    for (const checkbox of body.querySelectorAll('input[data-node-id]')) checkbox.checked = selected.has(checkbox.dataset.nodeId);
    updateSelection();
  });

  const row = (node, query) => createNodeRow({node, query, selected, deleted, groupNames, rowRefs, onSelectionChange: updateSelection});

  function patchCounts(scoped = scopedNodes(indexed, filter)) {
    const counts = {'': 0};
    for (const node of scoped) {
      if (deleted.has(node.id)) continue;
      counts['']++;
      counts[node.presence] = (counts[node.presence] || 0) + 1;
    }
    for (const [status, control] of statusButtons) {
      control.lastElementChild.textContent = counts[status] || 0;
      if (counts[status]) control.hidden = false;
    }
    return counts;
  }

  function update(debounceSummary = false) {
    const query = filter.q.trim().toLowerCase();
    const scoped = scopedNodes(indexed, filter);
    const counts = patchCounts(scoped);
    for (const [status, control] of statusButtons) {
      control.setAttribute('aria-pressed', String(filter.status === status));
      control.hidden = Boolean(status) && !counts[status] && filter.status !== status;
    }
    const rows = sortedNodes(scoped, filter);
    patchNodeSortHeaders(headers, filter, direction());
    const pages = Math.max(1, Math.ceil(rows.length / NODE_PAGE_SIZE));
    filter.page = Math.min(filter.page, pages - 1);
    const start = filter.page * NODE_PAGE_SIZE;
    const visible = rows.slice(start, start + NODE_PAGE_SIZE);
    visibleIDs = new Set(visible.filter((node) => !deleted.has(node.id)).map((node) => node.id));
    const existing = new Set(nodes.filter((node) => !deleted.has(node.id)).map((node) => node.id));
    for (const id of selected) if (!existing.has(id)) selected.delete(id);
    rowRefs.clear();
    body.replaceChildren(...(visible.length ? visible.map((node) => row(node, query))
      : [h('tr', null, h('td', {colspan: 8}, nodes.length
        ? emptyState(t('nodes.noMatch'), t('nodes.filterHint'), button(t('nodes.clearFilters'), {onClick: clear}))
        : emptyState(t('nodes.empty'), t('nodes.emptyEnroll'), addNodeButton())))]));
    updateSelection();
    const summaryText = rows.length === 0 ? t('nodes.noMatching')
      : t(rows.length !== nodes.length ? 'nodes.showingFiltered' : 'nodes.showing', {start: start + 1, end: start + visible.length, count: rows.length, total: nodes.length});
    clearTimeout(summaryTimer);
    if (debounceSummary) summaryTimer = setTimeout(() => { summary.textContent = summaryText; }, 400);
    else summary.textContent = summaryText;
    previous.disabled = filter.page === 0;
    next.disabled = filter.page >= pages - 1;
    pager.hidden = pages <= 1;
    writeNodeFilter(filter);
    statusChanged = false;
    paintChanges();
  }

  function reloadNodes({acknowledgeDeleted = false} = {}) {
    const pending = reloadQueue.then(async () => {
      if (signal?.aborted) return false;
      const priorDeleted = acknowledgeDeleted ? new Set(deleted) : new Set();
      live.close();
      connection.update({state: 'stale'});
      try {
        // A queued post-mutation reload must not reuse its predecessor's cache.
        invalidate('nodes');
        const fresh = await allNodes();
        const currentFleet = await readConsoleFleet(signal);
        if (signal?.aborted) return false;
        const ids = new Set(fresh.map((node) => node.id));
        const current = [...fresh, ...nodes.filter((node) => !priorDeleted.has(node.id) && !ids.has(node.id))];
        nodes.splice(0, nodes.length, ...current);
        indexed.splice(0, indexed.length, ...current.map((node) => ({node, text: searchText(node)})));
        if (acknowledgeDeleted) {
          deleted.clear();
          selectionRemoved = false;
        }
        inventoryChanged = deleted.size > 0;
        patchFleet(currentFleet);
        return true;
      } finally {
        if (!signal?.aborted) live = subscribe();
      }
    });
    reloadQueue = pending.catch(() => {});
    return pending;
  }

  async function runBatch(action) {
    if (batchBusy || selected.size === 0) return;
    batchBusy = true;
    const controls = [disableSelected, enableSelected, deleteSelected];
    try {
      await runNodeBatch({action, selected, nodes, signal, controls, reloadNodes,
        onCompleted: () => { selected.clear(); update(); }});
    } finally { batchBusy = false; }
  }

  const applyFilters = button(t('nodes.applyFilters'), {small: true, onClick: () => update()});
  const statusChange = h('span', {class: 'live-change', hidden: true}, t('nodes.statusChanged'), applyFilters);
  const refreshList = button(t('nodes.refreshList'), {small: true, onClick: async () => {
    if (refreshList.disabled) return;
    busy(refreshList, true);
    try {
      if (!await reloadNodes({acknowledgeDeleted: true})) return;
      update();
    } catch (error) { if (!signal?.aborted) toastError(error); }
    finally { busy(refreshList, false); }
  }});
  const inventoryChange = h('span', {class: 'live-change', hidden: true}, t('nodes.inventoryChanged'), refreshList);
  const selectionChange = h('span', {hidden: true}, t('nodes.selectionRemoved'));
  const changes = h('div', {class: 'live-changes', 'aria-live': 'polite', hidden: true}, statusChange, inventoryChange, selectionChange);
  function paintChanges() {
    statusChange.hidden = !statusChanged;
    inventoryChange.hidden = !inventoryChanged;
    selectionChange.hidden = !selectionRemoved;
    changes.hidden = !statusChanged && !inventoryChanged && !selectionRemoved;
  }
  const table = h('div', {class: 'panel'},
    h('div', {class: 'toolbar'},
      h('div', {class: 'search'}, icon('search'), search, h('kbd', null, '/')),
      h('div', {class: 'segmented', role: 'group', 'aria-label': t('nodes.filterPresence')}, [...statusButtons.values()]),
      groupSelect),
    h('div', {class: 'table-wrap'}, h('table', {class: 'table'}, h('caption', {class: 'sr'}, t('nodes.listTitle')), h('thead', null, h('tr', null, headers)), body)),
    h('div', {class: 'table-foot'}, summary, pager, changes), actionBar);
  update();
  function patchFleet(data) {
    const changes = patchNodesFromFleet(data, {nodes, filter, rowRefs, deleted, selected, visibleIDs});
    inventoryChanged ||= changes.inventoryChanged;
    statusChanged ||= changes.statusChanged;
    selectionRemoved ||= changes.selectionRemoved;
    patchCounts();
    updateSelection();
    paintChanges();
  }
  const subscribe = () => connectConsoleLive({signal, onState: connection.update, onSnapshot: patchFleet});
  live = subscribe();
  signal?.addEventListener('abort', () => { cancelAnimationFrame(frame); clearTimeout(summaryTimer); }, {once: true});
  return h('div', null, head, table);
}
