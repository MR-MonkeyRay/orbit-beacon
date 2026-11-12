import {t} from '../i18n.js';
import {h, icon, reducedMotion} from '../../shared/dom.js';
import {createNumberTween} from '../../shared/tween.js';
import {allNodes, getNavigationSignal, groupsList, invalidate} from '../api.js';
import {displayStatus, STATUS, STATUS_ORDER} from '../format.js';
import {connectConsoleLive, readConsoleFleet} from '../live.js';
import {busy, button, emptyState, liveStatus, pageHead} from '../ui/components.js';
import {toastError} from '../ui/feedback.js';
import {input, select} from '../ui/forms.js';
import {runNodeBatch} from '../lib/nodes-bulk.js';
import {NODE_PAGE_SIZE, SORTS, nodeSortDirection, readNodeFilter, scopedNodes, sortedNodes, writeNodeFilter} from '../lib/nodes-filters.js';
import {patchNodesFromFleet} from '../lib/nodes-live.js';
import {createNodeRow, fadeNodeRow, patchNodeSortHeaders} from '../lib/nodes-table.js';
import {NODE_COLUMNS, nodeColumnPicker, patchNodeColumns, readNodeColumns} from '../lib/nodes-columns.js';
import {pagePlaceholder, placeholderHead, placeholderRows} from '../ui/placeholders.js';
import {addNodeButton} from '../lib/add-node-dialog.js';

// 与节点列表同构：工具栏、状态分段与用户保存的列先出现，表格行以微光条占位。
export function nodesPlaceholder() {
  const columns = readNodeColumns();
  const optional = NODE_COLUMNS.map(([key, label]) => ({label: t(label), className: `hide-compact${columns.has(key) ? '' : ' column-hidden'}`}));
  const headers = [{label: '', className: 'selection-cell'}, t('nodes.column.name'), t('nodes.presence'),
    {label: t('nodes.cpu'), className: 'num'}, {label: t('nodes.memory'), className: 'num'}, ...optional];
  return pagePlaceholder(
    placeholderHead(t('nodes.listTitle')),
    h('div', {class: 'panel'},
      h('div', {class: 'toolbar'}, h('span', {class: 'sk sk-field', 'aria-hidden': 'true'}),
        // 真实分段只列出有节点的状态，加载前只能确定“全部”。
        h('div', {class: 'segmented', 'aria-hidden': 'true'}, h('span', {class: 'segment'}, t('nodes.all'))),
        h('span', {class: 'sk sk-field short', 'aria-hidden': 'true'})),
      h('div', {class: 'table-wrap nodes-table-wrap'}, h('table', {class: 'table nodes-table'},
        h('thead', null, h('tr', null, headers.map((header) => typeof header === 'object'
          ? h('th', {scope: 'col', class: header.className}, header.label) : h('th', {scope: 'col'}, header)))),
        placeholderRows(headers, 8)))));
}

export async function nodesPage() {
  const signal = getNavigationSignal();
  const [nodes, groups] = await Promise.all([allNodes(), groupsList()]);
  const tween = createNumberTween({signal});
  const connection = liveStatus();
  const head = pageHead(t('nodes.listTitle'), connection.element, addNodeButton());

  const filter = readNodeFilter();
  const groupNames = new Map(groups.map((group) => [group.id, group.name]));
  const searchText = (node) => [node.name, node.region || node.geo_location, node.provider, node.notes, groupNames.get(node.group_id) || '', ...node.tags].join('\n').toLowerCase();
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
  const sortLabels = {order: t('nodes.field.display_order'), name: t('nodes.column.name'), status: t('nodes.presence'), cpu: t('nodes.cpu'), memory: t('nodes.memory'), agent_version: t('nodes.agentVersion'), created: t('nodes.added')};
  const mobileSort = select(SORTS.flatMap((key) => ['asc', 'desc'].map((dir) => [`${key}:${dir}`, `${sortLabels[key]} ${dir === 'asc' ? '↑' : '↓'}`])), `${filter.sort}:${direction()}`, {class: 'input nodes-mobile-sort', 'aria-label': t('nodes.sort')});
  mobileSort.addEventListener('change', () => { [filter.sort, filter.dir] = mobileSort.value.split(':'); update(); });
  const columns = readNodeColumns();
  const orderSort = h('button', {type: 'button', class: 'btn btn-sm', onclick: () => { filter.sort = 'order'; filter.dir = 'desc'; update(); }}, t('nodes.field.display_order'));
  const columnPicker = nodeColumnPicker({columns, signal, onChange: () => patchNodeColumns(table, columns)});
  const sortable = (key, label) => h('th', {scope: 'col', dataset: {sort: key}}, h('button', {type: 'button', onclick: () => {
    filter.dir = filter.sort === key ? (direction() === 'asc' ? 'desc' : 'asc') : ['created', 'order'].includes(key) ? 'desc' : 'asc';
    filter.sort = key;
    update();
  }}, label, icon('up')));
  const selectAll = h('input', {type: 'checkbox', 'aria-label': t('nodes.selectAll')});
  const headers = [h('th', {scope: 'col', class: 'selection-cell'}, h('label', {class: 'selection-hit'}, selectAll)),
    sortable('name', t('nodes.column.name')), sortable('status', t('nodes.presence')),
    sortable('cpu', t('nodes.cpu')), sortable('memory', t('nodes.memory')),
    sortable('agent_version', t('nodes.agentVersion')), h('th', {scope: 'col', class: 'hide-compact'}, t('nodes.lastSeen')), h('th', {scope: 'col', class: 'hide-compact'}, t('nodes.group')),
    h('th', {scope: 'col', class: 'hide-compact'}, t('nodes.location')), h('th', {scope: 'col', class: 'hide-compact'}, t('nodes.tags')), sortable('created', t('nodes.added'))];

  for (const [index, key] of [[5, 'agent_version'], [6, 'last_seen'], [7, 'group'], [8, 'location'], [9, 'tags'], [10, 'created']]) {
    headers[index].dataset.column = key;
    headers[index].className = 'hide-compact';
  }
  headers[3].className = headers[4].className = 'num';

  const body = h('tbody');
  const summary = h('span', {'aria-live': 'polite'});
  let summaryTimer;
  const turn = (delta) => {
    const control = delta < 0 ? previous : next;
    const restoreFocus = document.activeElement === control;
    filter.page += delta;
    update();
    if (restoreFocus && control.disabled) (delta < 0 ? next : previous).focus();
    window.scrollTo({top: 0, behavior: reducedMotion.matches ? 'instant' : 'smooth'});
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
  const cancelSelected = button(t('nodes.cancelSelection'), {small: true, onClick: () => {
    selected.clear();
    for (const refs of rowRefs.values()) refs.checkbox.checked = false;
    updateSelection();
    selectAll.focus();
  }});
  const groupSelected = button(t('nodes.setGroup'), {small: true, onClick: () => runBatch('group')});
  const tagSelected = button(t('nodes.addTag'), {small: true, onClick: () => runBatch('tag')});
  const actionBar = h('div', {class: 'bulk-actions', hidden: true}, selectionCount, cancelSelected, groupSelected, tagSelected, disableSelected, enableSelected, deleteSelected);

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

  const row = (node, query) => createNodeRow({node, query, selected, deleted, groupNames, rowRefs, onSelectionChange: updateSelection, tween});

  function patchCounts(scoped = scopedNodes(indexed, filter)) {
    const counts = {'': 0};
    for (const node of scoped) {
      if (deleted.has(node.id)) continue;
      counts['']++;
      counts[displayStatus(node)] = (counts[displayStatus(node)] || 0) + 1;
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
    mobileSort.value = `${filter.sort}:${direction()}`;
    const pages = Math.max(1, Math.ceil(rows.length / NODE_PAGE_SIZE));
    filter.page = Math.min(filter.page, pages - 1);
    const start = filter.page * NODE_PAGE_SIZE;
    const visible = rows.slice(start, start + NODE_PAGE_SIZE);
    visibleIDs = new Set(visible.filter((node) => !deleted.has(node.id)).map((node) => node.id));
    const existing = new Set(nodes.filter((node) => !deleted.has(node.id)).map((node) => node.id));
    for (const id of selected) if (!existing.has(id)) selected.delete(id);
    const shown = new Set(rowRefs.keys());
    rowRefs.clear();
    body.replaceChildren(...(visible.length ? visible.map((node) => row(node, query))
      : [h('tr', null, h('td', {colspan: 11}, nodes.length
        ? emptyState(t('nodes.noMatch'), null, button(t('nodes.clearFilters'), {onClick: clear}))
        : emptyState(t('nodes.empty'), null, addNodeButton())))]));
    // 搜索和排序会重建表格；只有新进入的行淡入，留在表里的行不闪。
    if (shown.size) for (const [id, refs] of rowRefs) if (!shown.has(id)) fadeNodeRow(refs);
    patchNodeColumns(table, columns);
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
        // 排队中的变更后重载不得复用前一次的缓存。
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
    const controls = [disableSelected, enableSelected, deleteSelected, cancelSelected, groupSelected, tagSelected];
    try {
      await runNodeBatch({action, selected, nodes, groups, signal, controls, reloadNodes,
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
      groupSelect, mobileSort, orderSort, columnPicker),
    h('div', {class: 'table-wrap nodes-table-wrap'}, h('table', {class: 'table nodes-table'}, h('caption', {class: 'sr'}, t('nodes.listTitle')), h('thead', null, h('tr', null, headers)), body)),
    h('div', {class: 'table-foot'}, summary, pager, changes), actionBar);
  update();
  function patchFleet(data) {
    const changes = patchNodesFromFleet(data, {nodes, filter, rowRefs, deleted, selected, visibleIDs, tween});
    if (changes.resort) update();
    inventoryChanged ||= changes.inventoryChanged;
    statusChanged ||= changes.statusChanged;
    selectionRemoved ||= changes.selectionRemoved;
    patchCounts();
    updateSelection();
    paintChanges();
  }
  const subscribe = () => connectConsoleLive({signal, onState: connection.update, onSnapshot: patchFleet});
  live = subscribe();
  signal?.addEventListener('abort', () => {
    cancelAnimationFrame(frame);
    clearTimeout(summaryTimer);
    for (const refs of rowRefs.values()) refs.animation?.cancel();
  }, {once: true});
  return h('div', null, head, table);
}
