import {h, icon} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {button, uid} from '../ui/components.js';
import {select} from '../ui/forms.js';
import {dismissPopover} from '../ui/popover.js';

export const NODE_COLUMNS = [
  ['agent_version', 'nodes.agentVersion'], ['last_seen', 'nodes.lastSeen'], ['group', 'nodes.group'],
  ['location', 'nodes.location'], ['tags', 'nodes.tags'], ['created', 'nodes.added']
];
const STORAGE_KEY = 'orbitbeacon-node-columns';
const defaults = () => new Set(['last_seen', 'group', 'location', 'tags']);

export function readNodeColumns() {
  try {
    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY));
    if (Array.isArray(stored)) return new Set(stored.filter((key) => NODE_COLUMNS.some(([id]) => id === key)));
  } catch {}
  return defaults();
}

// “视图”浮层：排序与可选列放在一起。手机卡片固定字段，列选择在手机上由样式隐藏，排序仍可用。
// sort 为 {keys: [[key, label]], read: () => ({sort, dir}), write: (sort, dir) => void}。
export function nodeViewMenu({columns, onChange, sort, signal}) {
  const sortKey = sort ? select(sort.keys, sort.read().sort, {class: 'input view-sort-key', 'aria-label': t('nodes.sort')}) : null;
  const directions = sort ? ['asc', 'desc'].map((dir) => h('button', {type: 'button', dataset: {dir},
    onclick: () => sort.write(sortKey.value, dir)}, icon('up'), t(dir === 'asc' ? 'nodes.ascending' : 'nodes.descending'))) : [];
  sortKey?.addEventListener('change', () => sort.write(sortKey.value, ''));
  const syncSort = () => {
    if (!sort) return;
    const current = sort.read();
    sortKey.value = current.sort;
    for (const control of directions) control.setAttribute('aria-pressed', String(control.dataset.dir === current.dir));
  };
  const sortHeading = uid('view-sort');
  const columnsHeading = uid('view-columns');
  const panel = h('div', {class: 'view-options', id: uid('view'), hidden: true},
    sort ? h('div', {class: 'view-section', role: 'group', 'aria-labelledby': sortHeading},
      h('div', {class: 'view-heading', id: sortHeading}, t('nodes.sort')),
      sortKey, h('div', {class: 'segmented view-direction'}, directions)) : null,
    h('div', {class: 'view-section view-columns', role: 'group', 'aria-labelledby': columnsHeading}, h('div', {class: 'view-heading', id: columnsHeading}, t('nodes.columns')),
      NODE_COLUMNS.map(([key, label]) => h('label', {class: 'choice-option'}, h('input', {type: 'checkbox', checked: columns.has(key), dataset: {column: key}, onchange: (event) => {
        if (event.currentTarget.checked) columns.add(key); else columns.delete(key);
        try { localStorage.setItem(STORAGE_KEY, JSON.stringify([...columns])); } catch {}
        onChange();
      }}), t(label)))));
  const trigger = button(t('nodes.view'), {icon: 'sliders', onClick: () => {
    panel.hidden = !panel.hidden;
    trigger.setAttribute('aria-expanded', String(!panel.hidden));
  }});
  trigger.setAttribute('aria-controls', panel.id);
  trigger.setAttribute('aria-expanded', 'false');
  const root = h('div', {class: 'view-menu'}, trigger, panel);
  root.syncSort = syncSort;
  syncSort();
  dismissPopover(root, {trigger, panel, signal});
  return root;
}

export function patchNodeColumns(table, columns) {
  for (const cell of table.querySelectorAll('th,td')) {
    if (cell.dataset.column) cell.classList.toggle('column-hidden', !columns.has(cell.dataset.column));
  }
}
