import {h, icon, reducedMotion} from '../../shared/dom.js';
import {allNodes, ensureSession, groupsList} from '../api.js';
import {STATUS, STATUS_ORDER, when} from '../format.js';
import {button, emptyState, link, pageHead, statusBadge} from '../ui/components.js';
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
  await ensureSession();
  const [nodes, groups] = await Promise.all([allNodes(), groupsList()]);
  const head = pageHead('Nodes', 'Every node in inventory, including pending and disabled ones. Select a node to edit its details, public page and agent configuration.', addNodeButton());
  if (nodes.length === 0) {
    return h('div', null, head, h('div', {class: 'panel'}, emptyState('No nodes yet', 'Add a node, then enroll its agent with a credential issued through the management API.', addNodeButton())));
  }

  const filter = readFilter();
  const groupNames = new Map(groups.map((group) => [group.id, group.name]));
  const searchText = (node) => [node.name, node.region, node.provider, node.notes, groupNames.get(node.group_id) || '', ...node.tags].join('\n').toLowerCase();
  const indexed = nodes.map((node) => ({node, text: searchText(node)}));

  const search = input({type: 'search', id: 'node-search', placeholder: 'Search name, region, provider or tag', value: filter.q, 'aria-label': 'Search nodes', autocomplete: 'off'});
  let frame = 0;
  search.addEventListener('input', () => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => { filter.q = search.value; filter.page = 0; update(); });
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
  const headers = [sortable('name', 'Name'), sortable('status', 'Status'), h('th', {scope: 'col', class: 'hide-sm'}, 'Group'),
    h('th', {scope: 'col', class: 'hide-sm'}, 'Location'), h('th', {scope: 'col', class: 'hide-sm'}, 'Tags'), sortable('created', 'Added')];

  const body = h('tbody');
  const summary = h('span', {'aria-live': 'polite'});
  const turn = (delta) => {
    filter.page += delta;
    update();
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

  const row = (node, query) => h('tr', null,
    h('td', {class: 'name'}, link(`/nodes/${node.id}`, highlight(node.name, query))),
    h('td', null, statusBadge(node.status)),
    h('td', {class: 'hide-sm'}, node.group_id ? groupNames.get(node.group_id) || h('span', {class: 'muted'}, 'Unknown') : h('span', {class: 'muted'}, 'None')),
    h('td', {class: 'hide-sm'}, [node.region, node.provider].filter(Boolean).join(', ') || h('span', {class: 'muted'}, 'None')),
    h('td', {class: 'hide-sm'}, h('div', {class: 'cell-tags'}, node.tags.slice(0, 3).map((tag) => h('span', {class: 'tag static'}, tag)),
      node.tags.length > 3 ? h('span', {class: 'muted'}, `+${node.tags.length - 3}`) : null)),
    h('td', {class: 'muted'}, when(node.created_at)));

  function update() {
    const query = filter.q.trim().toLowerCase();
    const scoped = indexed.filter(({node, text}) => (!query || text.includes(query))
      && (!filter.group || (filter.group === 'none' ? !node.group_id : node.group_id === filter.group))).map(({node}) => node);
    for (const [status, control] of statusButtons) {
      const count = status ? scoped.filter((node) => node.status === status).length : scoped.length;
      control.lastElementChild.textContent = count;
      control.setAttribute('aria-pressed', String(filter.status === status));
      control.hidden = Boolean(status) && count === 0 && filter.status !== status;
    }
    const sign = direction() === 'asc' ? 1 : -1;
    const rows = scoped.filter((node) => !filter.status || node.status === filter.status).sort((a, b) => {
      const order = filter.sort === 'name' ? a.name.localeCompare(b.name, undefined, {numeric: true, sensitivity: 'base'})
        : filter.sort === 'status' ? STATUS_ORDER.indexOf(a.status) - STATUS_ORDER.indexOf(b.status)
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
    body.replaceChildren(...(visible.length ? visible.map((node) => row(node, query))
      : [h('tr', null, h('td', {colspan: 6}, emptyState('No nodes match these filters', 'Try a different search, status or group.', button('Clear filters', {onClick: clear}))))]));
    summary.textContent = rows.length === 0 ? 'No matching nodes'
      : `Showing ${start + 1}–${start + visible.length} of ${rows.length}${rows.length !== nodes.length ? ` (filtered from ${nodes.length})` : ''}`;
    previous.disabled = filter.page === 0;
    next.disabled = filter.page >= pages - 1;
    pager.hidden = pages <= 1;
    writeFilter(filter);
  }

  const table = h('div', {class: 'panel'},
    h('div', {class: 'toolbar'},
      h('div', {class: 'search'}, icon('search'), search, h('kbd', null, '/')),
      h('div', {class: 'segmented', role: 'group', 'aria-label': 'Filter by status'}, [...statusButtons.values()]),
      groupSelect),
    h('div', {class: 'table-wrap'}, h('table', {class: 'table'}, h('caption', {class: 'sr'}, 'Nodes'), h('thead', null, h('tr', null, headers)), body)),
    h('div', {class: 'table-foot'}, summary, pager));
  update();
  return h('div', null, head, table);
}
