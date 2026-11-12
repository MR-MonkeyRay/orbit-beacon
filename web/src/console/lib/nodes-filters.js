import {getLocale} from '../i18n.js';
import {displayStatus, STATUS} from '../format.js';

export const NODE_PAGE_SIZE = 50;

export const SORTS = ['order', 'name', 'status', 'cpu', 'memory', 'agent_version', 'created'];
// 概览及状态筛选保留 STATUS_ORDER；节点列表优先展示需要关注的节点。
export const NODE_STATUS_ORDER = ['offline', 'never', 'pending', 'online', 'disabled', 'stopped'];

export function readNodeFilter(search = location.search) {
  const params = new URLSearchParams(search);
  return {
    q: params.get('q') || '',
    status: STATUS[params.get('status')] ? params.get('status') : '',
    group: params.get('group') || '',
    sort: SORTS.includes(params.get('sort')) ? params.get('sort') : 'status',
    dir: ['asc', 'desc'].includes(params.get('dir')) ? params.get('dir') : '',
    page: Math.max(0, Number.parseInt(params.get('page') || '0', 10) || 0)
  };
}

export function writeNodeFilter(filter) {
  const params = new URLSearchParams();
  if (filter.q) params.set('q', filter.q);
  if (filter.status) params.set('status', filter.status);
  if (filter.group) params.set('group', filter.group);
  if (filter.sort !== 'status') params.set('sort', filter.sort);
  if (filter.dir && filter.dir !== nodeSortDirection({sort: filter.sort})) params.set('dir', filter.dir);
  if (filter.page) params.set('page', String(filter.page));
  const query = params.toString();
  const target = `/nodes${query ? `?${query}` : ''}`;
  if (target !== location.pathname + location.search) history.replaceState(history.state, '', target);
}

export function nodeSortDirection(filter) {
  return filter.dir || (['created', 'order'].includes(filter.sort) ? 'desc' : 'asc');
}

export function scopedNodes(indexed, filter) {
  const query = filter.q.trim().toLowerCase();
  return indexed.filter(({node, text}) => (!query || text.includes(query))
    && (!filter.group || (filter.group === 'none' ? !node.group_id : node.group_id === filter.group))).map(({node}) => node);
}

export function sortedNodes(nodes, filter) {
  const sign = nodeSortDirection(filter) === 'asc' ? 1 : -1;
  return nodes.filter((node) => !filter.status || displayStatus(node) === filter.status).sort((a, b) => {
    const nameOrder = a.name.localeCompare(b.name, getLocale(), {numeric: true, sensitivity: 'base'});
    if (filter.sort === 'cpu' || filter.sort === 'memory') {
      const key = filter.sort === 'cpu' ? 'cpu_percent' : 'memory_used_percent';
      const av = a[key], bv = b[key];
      if (Number.isFinite(av) !== Number.isFinite(bv)) return Number.isFinite(av) ? -1 : 1;
      return (Number.isFinite(av) ? (av - bv) * sign : 0) || nameOrder || a.id.localeCompare(b.id);
    }
    const order = filter.sort === 'order' ? (a.display_order || 0) - (b.display_order || 0)
      : filter.sort === 'name' ? a.name.localeCompare(b.name, getLocale(), {numeric: true, sensitivity: 'base'})
      : filter.sort === 'status' ? NODE_STATUS_ORDER.indexOf(displayStatus(a)) - NODE_STATUS_ORDER.indexOf(displayStatus(b))
        : filter.sort === 'agent_version' ? a.agent_version.localeCompare(b.agent_version, getLocale(), {numeric: true, sensitivity: 'base'})
        : a.created_at.localeCompare(b.created_at);
    return order * sign || (['status', 'order'].includes(filter.sort) ? nameOrder : 0) || a.id.localeCompare(b.id);
  });
}
