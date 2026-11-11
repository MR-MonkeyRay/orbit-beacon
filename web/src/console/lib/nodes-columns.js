import {h} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {button, uid} from '../ui/components.js';
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

export function nodeColumnPicker({columns, onChange, signal}) {
  const panel = h('div', {class: 'choice-list column-options', id: uid('columns'), hidden: true}, NODE_COLUMNS.map(([key, label]) =>
    h('label', {class: 'choice-option'}, h('input', {type: 'checkbox', checked: columns.has(key), dataset: {column: key}, onchange: (event) => {
      if (event.currentTarget.checked) columns.add(key); else columns.delete(key);
      try { localStorage.setItem(STORAGE_KEY, JSON.stringify([...columns])); } catch {}
      onChange();
    }}), t(label))));
  const trigger = button(t('nodes.columns'), {small: true, onClick: () => {
    panel.hidden = !panel.hidden;
    trigger.setAttribute('aria-expanded', String(!panel.hidden));
  }});
  trigger.setAttribute('aria-controls', panel.id);
  trigger.setAttribute('aria-expanded', 'false');
  const root = h('div', {class: 'column-picker'}, trigger, panel);
  dismissPopover(root, {trigger, panel, signal});
  return root;
}

export function patchNodeColumns(table, columns) {
  for (const cell of table.querySelectorAll('th,td')) {
    if (cell.dataset.column) cell.classList.toggle('column-hidden', !columns.has(cell.dataset.column));
  }
}
