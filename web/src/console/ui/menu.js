import {h} from '../../shared/dom.js';
import {getNavigationSignal} from '../api.js';
import {button, uid} from './components.js';
import {dismissPopover} from './popover.js';

// Native buttons supply click activation; arrow navigation stays inside the menu.
export function menuButton({label, icon, items}) {
  const trigger = button(label, {icon});
  trigger.setAttribute('aria-haspopup', 'menu');
  trigger.setAttribute('aria-expanded', 'false');
  const controls = items.map((item, index) => button(item.label, {disabled: item.disabled,
    className: `menu-item${item.danger ? ' menu-danger' : ''}`, onClick: () => {
      if (controls[index].disabled) return;
      close();
      trigger.focus();
      item.onSelect?.();
    }}));
  controls.forEach((control) => {
    control.setAttribute('role', 'menuitem');
    control.tabIndex = -1;
  });
  const menu = h('div', {class: 'menu-options', role: 'menu', id: uid('menu'), hidden: true,
    'aria-labelledby': trigger.id}, controls);
  trigger.setAttribute('aria-controls', menu.id);
  const root = h('div', {class: 'menu-button'}, trigger, menu);
  const close = dismissPopover(root, {trigger, panel: menu, signal: getNavigationSignal(), closeOnTab: true});
  const open = () => {
    if (trigger.disabled) return;
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
