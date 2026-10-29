import {h, icon, reducedMotion} from '../../shared/dom.js';
import {allNodes, api, ensureSession, getNavigationSignal, groupsList, invalidate} from '../api.js';
import {absoluteWhen, STATUS, STATUS_ORDER, when} from '../format.js';
import {connectConsoleLive, readConsoleFleet} from '../live.js';
import {busy, button, emptyState, link, liveStatus, pageHead, patchStatusBadge, statusBadge} from '../ui/components.js';
import {confirmDialog, modal} from '../ui/dialogs.js';
import {toastError} from '../ui/feedback.js';
import {input, select} from '../ui/forms.js';
import {addNodeButton} from './shared.js';

const PAGE_SIZE = 50;
const SORTS = ['name', 'status', 'created'];

// Filter state lives in the query string so it survives reloads and back/forward.
function readFilter() {
  const params = new URLSearchParams(location.search);
  return {
    q: params.get('q') || '',
    status: STATUS[params.get('status')] ? params.get('status') : '',
    group: params.get('group') || '',
    sort: SORTS.includes(params.get('sort')) ? params.get('sort') : 'created',
    dir: ['asc', 'desc'].includes(params.get('dir')) ? params.get('dir') : '',
    page: Math.max(0, Number.parseInt(params.get('page') || '0', 10) || 0)
  };
}

function writeFilter(filter) {
  const params = new URLSearchParams();
  if (filter.q) params.set('q', filter.q);
  if (filter.status) params.set('status', filter.status);
  if (filter.group) params.set('group', filter.group);
  if (filter.sort !== 'created') params.set('sort', filter.sort);
  if (filter.dir) params.set('dir', filter.dir);
  if (filter.page) params.set('page', String(filter.page));
  const query = params.toString();
  const target = `/nodes${query ? `?${query}` : ''}`;
  if (target !== location.pathname + location.search) history.replaceState(history.state, '', target);
}

function highlight(value, query) {
  const index = query ? value.toLowerCase().indexOf(query) : -1;
  if (index < 0) return value;
  return [value.slice(0, index), h('mark', null, value.slice(index, index + query.length)), value.slice(index + query.length)];
}

export async function nodesPage() {
  const signal = getNavigationSignal();
  await ensureSession();
  const [nodes, groups] = await Promise.all([allNodes(), groupsList()]);
  const connection = liveStatus();
  const head = pageHead('Nodes', 'Every node in inventory, including pending and disabled ones. Select a node to edit its details, public page and local agent settings.', connection.element, addNodeButton());

  const filter = readFilter();
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

  const search = input({type: 'search', id: 'node-search', placeholder: 'Search name, region, provider or tag', value: filter.q, 'aria-label': 'Search nodes', autocomplete: 'off'});
  let frame = 0;
  search.addEventListener('input', () => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => { filter.q = search.value; filter.page = 0; update(true); });
  });

  const statusButtons = new Map(['', ...STATUS_ORDER].map((status) => [status, h('button', {type: 'button',
    onclick: () => { filter.status = status; filter.page = 0; update(); }}, status ? STATUS[status] : 'All', h('span', {class: 'count'}))]));

  const groupChoices = [['', 'All groups'], ['none', 'Ungrouped'], ...groups.map((group) => [group.id, group.name])];
  const groupSelect = select(groupChoices, groupChoices.some(([id]) => id === filter.group) ? filter.group : '', {'aria-label': 'Filter by group'});
  groupSelect.addEventListener('change', () => { filter.group = groupSelect.value; filter.page = 0; update(); });

  const direction = () => (filter.dir || (filter.sort === 'created' ? 'desc' : 'asc'));
  const sortable = (key, label) => h('th', {scope: 'col', dataset: {sort: key}}, h('button', {type: 'button', onclick: () => {
    filter.dir = filter.sort === key ? (direction() === 'asc' ? 'desc' : 'asc') : key === 'created' ? 'desc' : 'asc';
    filter.sort = key;
    update();
  }}, label, icon('up')));
  const selectAll = h('input', {type: 'checkbox', 'aria-label': 'Select all nodes on this page'});
  const headers = [h('th', {scope: 'col', class: 'selection-cell'}, selectAll),
    sortable('name', 'Name'), sortable('status', 'Presence'), h('th', {scope: 'col', class: 'hide-sm'}, 'Last seen'), h('th', {scope: 'col', class: 'hide-sm'}, 'Group'),
    h('th', {scope: 'col', class: 'hide-sm'}, 'Location'), h('th', {scope: 'col', class: 'hide-sm'}, 'Tags'), sortable('created', 'Added')];

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
  const previous = button('Previous', {icon: 'left', small: true, onClick: () => turn(-1)});
  const next = button('Next', {trailingIcon: 'right', small: true, onClick: () => turn(1)});
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
  const disableSelected = button('Disable', {small: true, onClick: () => runBatch('disable')});
  const enableSelected = button('Enable', {small: true, onClick: () => runBatch('enable')});
  const deleteSelected = button('Delete', {small: true, variant: 'danger', onClick: () => runBatch('delete')});
  const actionBar = h('div', {class: 'bulk-actions', hidden: true}, selectionCount, disableSelected, enableSelected, deleteSelected);

  const updateSelection = () => {
    const count = selected.size;
    const text = `${count} node${count === 1 ? '' : 's'} selected`;
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

  const row = (node, query) => {
    const checkbox = h('input', {type: 'checkbox', checked: selected.has(node.id), disabled: deleted.has(node.id),
      dataset: {nodeId: node.id}, 'aria-label': `Select ${node.name}`, onchange: (event) => {
        if (deleted.has(node.id)) return;
        if (event.currentTarget.checked) selected.add(node.id);
        else selected.delete(node.id);
        updateSelection();
      }});
    const name = link(`/nodes/${node.id}`, highlight(node.name, query));
    const badge = statusBadge(node.presence);
    const lastSeen = h('td', {class: 'hide-sm muted'}, node.last_seen ? absoluteWhen(node.last_seen) : 'Never');
    const element = h('tr', null, h('td', {class: 'selection-cell'}, checkbox), h('td', {class: 'name'}, name),
      h('td', null, badge), lastSeen,
      h('td', {class: 'hide-sm'}, node.group_id ? groupNames.get(node.group_id) || h('span', {class: 'muted'}, 'Unknown') : h('span', {class: 'muted'}, 'None')),
      h('td', {class: 'hide-sm'}, [node.region, node.provider].filter(Boolean).join(', ') || h('span', {class: 'muted'}, 'None')),
      h('td', {class: 'hide-sm'}, h('div', {class: 'cell-tags'}, node.tags.slice(0, 3).map((tag) => h('span', {class: 'tag static'}, tag)),
        node.tags.length > 3 ? h('span', {class: 'muted'}, `+${node.tags.length - 3}`) : null)),
      h('td', {class: 'muted'}, when(node.created_at)));
    const refs = {element, checkbox, name, badge, lastSeen};
    rowRefs.set(node.id, refs);
    if (deleted.has(node.id)) tombstone(refs);
    return element;
  };

  function tombstone(refs) {
    refs.element.classList.add('node-deleted');
    patchStatusBadge(refs.badge, 'deleted');
    refs.checkbox.checked = false;
    refs.checkbox.disabled = true;
    refs.name.removeAttribute('href');
    delete refs.name.dataset.link;
    refs.name.setAttribute('aria-disabled', 'true');
    refs.name.setAttribute('tabindex', '-1');
  }

  function scopedNodes() {
    const query = filter.q.trim().toLowerCase();
    return indexed.filter(({node, text}) => (!query || text.includes(query))
      && (!filter.group || (filter.group === 'none' ? !node.group_id : node.group_id === filter.group))).map(({node}) => node);
  }

  function patchCounts(scoped = scopedNodes()) {
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
    const scoped = scopedNodes();
    const counts = patchCounts(scoped);
    for (const [status, control] of statusButtons) {
      control.setAttribute('aria-pressed', String(filter.status === status));
      control.hidden = Boolean(status) && !counts[status] && filter.status !== status;
    }
    const sign = direction() === 'asc' ? 1 : -1;
    const rows = scoped.filter((node) => !filter.status || node.presence === filter.status).sort((a, b) => {
      const order = filter.sort === 'name' ? a.name.localeCompare(b.name, undefined, {numeric: true, sensitivity: 'base'})
        : filter.sort === 'status' ? STATUS_ORDER.indexOf(a.presence) - STATUS_ORDER.indexOf(b.presence)
          : a.created_at.localeCompare(b.created_at);
      return order * sign || a.id.localeCompare(b.id);
    });
    for (const th of headers) {
      if (th.dataset.sort === filter.sort) th.setAttribute('aria-sort', sign === 1 ? 'ascending' : 'descending');
      else th.removeAttribute('aria-sort');
    }
    const pages = Math.max(1, Math.ceil(rows.length / PAGE_SIZE));
    filter.page = Math.min(filter.page, pages - 1);
    const start = filter.page * PAGE_SIZE;
    const visible = rows.slice(start, start + PAGE_SIZE);
    visibleIDs = new Set(visible.filter((node) => !deleted.has(node.id)).map((node) => node.id));
    const existing = new Set(nodes.filter((node) => !deleted.has(node.id)).map((node) => node.id));
    for (const id of selected) if (!existing.has(id)) selected.delete(id);
    rowRefs.clear();
    body.replaceChildren(...(visible.length ? visible.map((node) => row(node, query))
      : [h('tr', null, h('td', {colspan: 8}, nodes.length
        ? emptyState('No nodes match these filters', 'Try a different search, presence or group.', button('Clear filters', {onClick: clear}))
        : emptyState('No nodes yet', 'Add a node, then enroll its agent.', addNodeButton())))]));
    updateSelection();
    const summaryText = rows.length === 0 ? 'No matching nodes'
      : `Showing ${start + 1}–${start + visible.length} of ${rows.length}${rows.length !== nodes.length ? ` (filtered from ${nodes.length})` : ''}`;
    clearTimeout(summaryTimer);
    if (debounceSummary) summaryTimer = setTimeout(() => { summary.textContent = summaryText; }, 400);
    else summary.textContent = summaryText;
    previous.disabled = filter.page === 0;
    next.disabled = filter.page >= pages - 1;
    pager.hidden = pages <= 1;
    writeFilter(filter);
    statusChanged = false;
    paintChanges();
  }

  async function typedDelete(count) {
    const expected = `delete ${count} nodes`;
    const confirmation = h('input', {class: 'input', autocomplete: 'off', spellcheck: 'false', 'aria-label': `Type ${expected} to confirm`});
    const confirm = button('Delete nodes', {variant: 'danger', className: 'solid', disabled: true});
    const {closed} = modal(`Delete ${count} nodes?`, (close) => {
      confirmation.addEventListener('input', () => { confirm.disabled = confirmation.value !== expected; });
      confirm.addEventListener('click', () => close('confirm'));
      return h('div', null,
        h('div', {class: 'dialog-body'}, h('p', null, 'This permanently deletes the selected nodes and their recorded data.'),
          h('p', null, 'Type ', h('strong', null, expected), ' to confirm.'), confirmation),
        h('div', {class: 'dialog-foot'}, button('Cancel', {onClick: () => close('dismiss')}), confirm));
    });
    return await closed === 'confirm';
  }

  function showResults(action, results, names) {
    const succeeded = results.filter((result) => result.status === 'succeeded').length;
    const failed = results.length - succeeded;
    const label = `${action[0].toUpperCase()}${action.slice(1)}`;
    return modal(`${label} results`, (close) => h('div', null,
      h('div', {class: 'dialog-body'},
        h('p', null, `${succeeded} succeeded${failed ? `; ${failed} failed` : '.'}`),
        h('ul', null, results.map((result) => h('li', null,
          h('strong', null, names.get(result.node_id) || result.node_id),
          ` — ${result.status}${result.error_code ? ` (${result.error_code})` : ''}`)))),
      h('div', {class: 'dialog-foot'}, button('Close', {variant: 'primary', onClick: () => close('done')})))).closed;
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
    if (batchBusy) return;
    const ids = [...selected];
    if (ids.length === 0) return;
    batchBusy = true;
    const confirmed = action === 'delete' ? await typedDelete(ids.length) : await confirmDialog({
      title: `${action === 'disable' ? 'Disable' : 'Enable'} ${ids.length} node${ids.length === 1 ? '' : 's'}?`,
      message: action === 'disable'
        ? 'Disabled nodes stop accepting agent reports and credential recovery until they are enabled again.'
        : 'Selected nodes will accept their agents again.',
      confirm: action === 'disable' ? 'Disable nodes' : 'Enable nodes'
    });
    if (!confirmed || signal?.aborted) { batchBusy = false; return; }
    const controls = [disableSelected, enableSelected, deleteSelected];
    controls.forEach((control) => busy(control, true));
    const names = new Map(nodes.map((node) => [node.id, node.name]));
    try {
      const response = await api('/nodes/batch', {method: 'POST', body: {node_ids: ids, action, ...(action === 'delete' ? {confirm: true} : {})}});
      if (signal?.aborted) return;
      // Reload the authoritative statuses after per-node transactions complete;
      // the batch endpoint may preserve a prior status when enabling a node.
      try { await reloadNodes(); } catch {}
      if (signal?.aborted) return;
      selected.clear();
      update();
      await showResults(action, response.results, names);
    } catch (error) { if (!signal?.aborted) toastError(error); }
    finally { controls.forEach((control) => busy(control, false)); batchBusy = false; }
  }

  const applyFilters = button('Apply current filters', {small: true, onClick: () => update()});
  const statusChange = h('span', {class: 'live-change', hidden: true}, 'Status changed. ', applyFilters);
  const refreshList = button('Refresh list', {small: true, onClick: async () => {
    if (refreshList.disabled) return;
    busy(refreshList, true);
    try {
      if (!await reloadNodes({acknowledgeDeleted: true})) return;
      update();
    } catch (error) { if (!signal?.aborted) toastError(error); }
    finally { busy(refreshList, false); }
  }});
  const inventoryChange = h('span', {class: 'live-change', hidden: true}, 'Inventory changed. ', refreshList);
  const selectionChange = h('span', {hidden: true}, 'A deleted node was removed from the selection.');
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
      h('div', {class: 'segmented', role: 'group', 'aria-label': 'Filter by presence'}, [...statusButtons.values()]),
      groupSelect),
    h('div', {class: 'table-wrap'}, h('table', {class: 'table'}, h('caption', {class: 'sr'}, 'Nodes'), h('thead', null, h('tr', null, headers)), body)),
    h('div', {class: 'table-foot'}, summary, pager, changes), actionBar);
  update();
  function patchFleet(data) {
    const current = new Map(data.nodes.map((node) => [node.id, node]));
    const known = new Set(nodes.map((node) => node.id));
    if (data.nodes.some((node) => !known.has(node.id))) inventoryChanged = true;
    for (const node of nodes) {
      const fresh = current.get(node.id);
      const refs = rowRefs.get(node.id);
      if (!fresh) {
        deleted.add(node.id);
        inventoryChanged = true;
        if (selected.delete(node.id)) selectionRemoved = true;
        visibleIDs.delete(node.id);
        if (refs) tombstone(refs);
        continue;
      }
      if (deleted.has(node.id)) continue;
      if (fresh.presence !== node.presence && (filter.status || filter.sort === 'status')) statusChanged = true;
      const seenChanged = fresh.last_seen !== node.last_seen;
      Object.assign(node, fresh);
      if (refs) {
        patchStatusBadge(refs.badge, node.presence);
        if (seenChanged) refs.lastSeen.replaceChildren(node.last_seen ? absoluteWhen(node.last_seen) : 'Never');
      }
    }
    patchCounts();
    updateSelection();
    paintChanges();
  }
  const subscribe = () => connectConsoleLive({signal, onState: connection.update, onSnapshot: patchFleet});
  live = subscribe();
  signal?.addEventListener('abort', () => { cancelAnimationFrame(frame); clearTimeout(summaryTimer); }, {once: true});
  return h('div', null, head, table);
}
