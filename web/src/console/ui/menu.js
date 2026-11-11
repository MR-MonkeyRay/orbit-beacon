import {h} from '../../shared/dom.js';
import {getNavigationSignal} from '../api.js';
import {button, uid} from './components.js';
import {dismissPopover} from './popover.js';

// 原生按钮负责点击激活；方向键只在菜单内移动焦点。
export function menuButton({id, label, icon, items, signal = getNavigationSignal()}) {
  const trigger = button(label, {id, icon});
  trigger.setAttribute('aria-haspopup', 'menu');
  trigger.setAttribute('aria-expanded', 'false');
  const controls = items.map((item, index) => button(item.label, {id: item.id || (id ? `${id}-item-${index}` : undefined), disabled: item.disabled,
    className: `menu-item${item.danger ? ' menu-danger' : ''}`, onClick: () => {
      if (controls[index].disabled) return;
      close();
      trigger.focus();
      item.onSelect?.();
      syncChecks();
    }}));
  controls.forEach((control, index) => {
    control.setAttribute('role', items[index].checked ? 'menuitemradio' : 'menuitem');
    control.tabIndex = -1;
  });
  const menu = h('div', {class: 'menu-options', role: 'menu', id: id ? `${id}-options` : uid('menu'), hidden: true,
    'aria-labelledby': trigger.id});
  const groups = new Map();
  items.forEach((item, index) => {
    if (!item.group) { menu.append(controls[index]); return; }
    if (!groups.has(item.group)) {
      const heading = h('div', {class: 'menu-heading', id: id ? `${id}-group-${index}` : uid('menu-heading')}, item.group);
      const group = h('div', {role: 'group', 'aria-labelledby': heading.id}, heading);
      groups.set(item.group, group); menu.append(group);
    }
    groups.get(item.group).append(controls[index]);
  });
  trigger.setAttribute('aria-controls', menu.id);
  const root = h('div', {class: 'menu-button'}, trigger, menu);
  const close = dismissPopover(root, {trigger, panel: menu, signal, closeOnTab: true});
  const syncChecks = () => items.forEach((item, index) => { if (item.checked) controls[index].setAttribute('aria-checked', String(item.checked())); });
  root.syncChecks = syncChecks;
  syncChecks();
  const open = () => {
    if (trigger.disabled) return;
    syncChecks();
    menu.hidden = false;
    trigger.setAttribute('aria-expanded', 'true');
    controls.find((control) => !control.disabled)?.focus();
  };
  trigger.addEventListener('click', () => { if (menu.hidden) open(); else close(); });
  trigger.addEventListener('keydown', (event) => {
    if (!['Enter', ' ', 'ArrowDown'].includes(event.key)) return;
    event.preventDefault();
    open();
  });
  menu.addEventListener('keydown', (event) => {
    const enabled = controls.filter((control) => !control.disabled);
    if (!enabled.length) return;
    const index = enabled.indexOf(document.activeElement);
    const next = {ArrowDown: (index + 1) % enabled.length,
      ArrowUp: (index - 1 + enabled.length) % enabled.length, Home: 0, End: enabled.length - 1}[event.key];
    if (next === undefined) return;
    event.preventDefault();
    enabled[next].focus();
  });
  return root;
}
