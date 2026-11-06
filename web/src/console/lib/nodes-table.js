import {formatList, t} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {displayStatus, absoluteWhen, when, formatPercent} from '../format.js';
import {link, patchStatusBadge, statusBadge} from '../ui/components.js';

export function highlightNodeMatch(value, query) {
  const index = query ? value.toLowerCase().indexOf(query) : -1;
  if (index < 0) return value;
  return [value.slice(0, index), h('mark', null, value.slice(index, index + query.length)), value.slice(index + query.length)];
}

// 离线节点很多时，整列“不可用”比占位符更吵。
export const metricText = (value) => Number.isFinite(value) ? formatPercent(value) : '—';

export function tombstoneNodeRow(refs) {
  refs.element.classList.add('node-deleted');
  patchStatusBadge(refs.badge, 'deleted');
  refs.checkbox.checked = false;
  refs.checkbox.disabled = true;
  refs.name.removeAttribute('href');
  delete refs.name.dataset.link;
  refs.name.setAttribute('aria-disabled', 'true');
  refs.name.setAttribute('tabindex', '-1');
}

export function createNodeRow({node, query, selected, deleted, groupNames, rowRefs, onSelectionChange}) {
  const checkbox = h('input', {type: 'checkbox', checked: selected.has(node.id), disabled: deleted.has(node.id),
    dataset: {nodeId: node.id}, 'aria-label': t('nodes.selectNode', {name: node.name}), onchange: (event) => {
      if (deleted.has(node.id)) return;
      if (event.currentTarget.checked) selected.add(node.id);
      else selected.delete(node.id);
      onSelectionChange();
    }});
  const name = link(`/nodes/${node.id}`, highlightNodeMatch(node.name, query));
  const badge = statusBadge(displayStatus(node));
  const lastSeen = h('td', {class: 'hide-compact muted'}, node.last_seen ? absoluteWhen(node.last_seen) : t('nodes.never'));
  const cpu = h('td', {class: 'num'}, metricText(node.cpu_percent));
  const memory = h('td', {class: 'num'}, metricText(node.memory_used_percent));
  const version = h('td', {class: 'hide-compact'}, node.agent_version);
  const element = h('tr', null, h('td', {class: 'selection-cell'}, h('label', {class: 'selection-hit'}, checkbox)), h('td', {class: 'name'}, name),
    h('td', null, badge), cpu, memory, version, lastSeen,
    h('td', {class: 'hide-compact'}, node.group_id ? groupNames.get(node.group_id) || h('span', {class: 'muted'}, t('common.status.unknown')) : h('span', {class: 'muted'}, t('nodes.none'))),
    h('td', {class: 'hide-compact'}, formatList([node.region, node.provider].filter(Boolean)) || h('span', {class: 'muted'}, t('nodes.none'))),
    h('td', {class: 'hide-compact'}, h('div', {class: 'cell-tags'}, node.tags.slice(0, 3).map((tag) => h('span', {class: 'tag static'}, tag)),
      node.tags.length > 3 ? h('span', {class: 'muted'}, `+${node.tags.length - 3}`) : null)),
    h('td', {class: 'hide-compact muted'}, when(node.created_at)));
  const refs = {element, checkbox, name, badge, lastSeen, cpu, memory, version};
  rowRefs.set(node.id, refs);
  if (deleted.has(node.id)) tombstoneNodeRow(refs);
  return element;
}

export function patchNodeSortHeaders(headers, filter, direction) {
  for (const header of headers) {
    if (header.dataset.sort === filter.sort) header.setAttribute('aria-sort', direction === 'asc' ? 'ascending' : 'descending');
    else header.removeAttribute('aria-sort');
  }
}
