import {h} from '../../shared/dom.js';
import {uid} from './components.js';

// 保持 DOM 焦点在输入框；选择框和命令面板共享过滤与键盘行为。
// persistent 列表始终可见，Esc 交给外层对话框。
export function listbox(entry, {choices = [], emptyText, onChoose, persistent = false}) {
  const list = h('div', {class: 'choice-list', id: uid('listbox'), role: 'listbox', hidden: true});
  const empty = h('div', {class: 'text-note', role: 'status'}, emptyText);
  let items = choices;
  let filtered = [];
  let active = -1;
  let query = '';
  entry.setAttribute('role', 'combobox');
  entry.setAttribute('aria-autocomplete', 'list');
  entry.setAttribute('aria-controls', list.id);
  entry.setAttribute('aria-expanded', 'false');
  const highlight = () => {
    const options = list.querySelectorAll('[role="option"]');
    options.forEach((option, index) => option.setAttribute('aria-selected', String(index === active)));
    const option = options[active];
    if (option) {
      entry.setAttribute('aria-activedescendant', option.id);
      option.scrollIntoView?.({block: 'nearest'});
    } else entry.removeAttribute('aria-activedescendant');
  };
  const close = () => {
    list.hidden = true;
    entry.setAttribute('aria-expanded', 'false');
    entry.removeAttribute('aria-activedescendant');
  };
  const choose = (index) => {
    if (entry.disabled || !filtered[index]) return;
    const item = filtered[index];
    close();
    onChoose(item);
  };
  const render = () => {
    filtered = items.filter(([, label]) => label.toLocaleLowerCase().includes(query.toLocaleLowerCase()));
    active = filtered.length ? 0 : -1;
    list.replaceChildren(...filtered.map(([id, label], index) => h('div', {
      class: 'choice-option', id: `${list.id}-${index}`, role: 'option', 'aria-selected': 'false',
      onmousedown: (event) => event.preventDefault(), onclick: () => choose(index)
    }, label)));
    if (!filtered.length) list.append(empty);
    highlight();
  };
  const open = (value = '') => {
    if (entry.disabled) return;
    query = value;
    list.hidden = false;
    entry.setAttribute('aria-expanded', 'true');
    render();
  };
  entry.addEventListener('input', (event) => { if (!event.isComposing) open(entry.value); });
  entry.addEventListener('compositionend', () => open(entry.value));
  if (!persistent) entry.addEventListener('blur', close);
  entry.addEventListener('keydown', (event) => {
    if (event.isComposing || entry.disabled) return;
    if (event.key === 'Escape' && !persistent && !list.hidden) {
      event.preventDefault();
      event.stopPropagation();
      close();
    } else if (event.key === 'Tab' && !persistent) close();
    else if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      if (list.hidden) { open(); active = event.key === 'ArrowUp' ? filtered.length - 1 : 0; }
      else active = Math.max(0, Math.min(filtered.length - 1, active + (event.key === 'ArrowDown' ? 1 : -1)));
      highlight();
    } else if (event.key === 'Enter' && !list.hidden) {
      event.preventDefault();
      choose(active);
    }
  });
  return {element: list, open, close, setChoices(next) { items = next; if (!list.hidden) render(); }};
}
