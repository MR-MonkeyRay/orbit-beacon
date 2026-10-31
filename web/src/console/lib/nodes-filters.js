import {getLocale} from '../i18n.js';
import {STATUS, STATUS_ORDER} from '../format.js';

export const NODE_PAGE_SIZE = 50;

const SORTS = ['name', 'status', 'created'];

export function readNodeFilter(search = location.search) {
  const params = new URLSearchParams(search);
  return {
    q: params.get('q') || '',
    status: STATUS[params.get('status')] ? params.get('status') : '',
    group: params.get('group') || '',
    sort: SORTS.includes(params.get('sort')) ? params.get('sort') : 'created',
    dir: ['asc', 'desc'].includes(params.get('dir')) ? params.get('dir') : '',
    page: Math.max(0, Number.parseInt(params.get('page') || '0', 10) || 0)
  };
}

export function writeNodeFilter(filter) {
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

export function nodeSortDirection(filter) {
  return filter.dir || (filter.sort === 'created' ? 'desc' : 'asc');
}

export function scopedNodes(indexed, filter) {
  const query = filter.q.trim().toLowerCase();
  return indexed.filter(({node, text}) => (!query || text.includes(query))
    && (!filter.group || (filter.group === 'none' ? !node.group_id : node.group_id === filter.group))).map(({node}) => node);
}

export function sortedNodes(nodes, filter) {
  const sign = nodeSortDirection(filter) === 'asc' ? 1 : -1;
  return nodes.filter((node) => !filter.status || node.presence === filter.status).sort((a, b) => {
    const order = filter.sort === 'name' ? a.name.localeCompare(b.name, getLocale(), {numeric: true, sensitivity: 'base'})
      : filter.sort === 'status' ? STATUS_ORDER.indexOf(a.presence) - STATUS_ORDER.indexOf(b.presence)
        : a.created_at.localeCompare(b.created_at);
    return order * sign || a.id.localeCompare(b.id);
  });
}
