import {h, icon} from '../../shared/dom.js';
import {confirmDiscard} from './dialogs.js';

// ARIA tabs (arrow keys, Home, End) whose selection is mirrored in the URL hash,
// so reloads, links such as /settings#audit and quiet refreshes keep the tab.
// Scrolls only the tab strip, never the page.
function reveal(button) {
  const strip = button.parentElement;
  if (!strip) return;
  const left = button.offsetLeft - strip.offsetLeft;
  if (left < strip.scrollLeft) strip.scrollLeft = left;
  else if (left + button.offsetWidth > strip.scrollLeft + strip.clientWidth) strip.scrollLeft = left + button.offsetWidth - strip.clientWidth;
}

export function tabSet(tabs, label) {
  const initial = Math.max(0, tabs.findIndex((tab) => `#${tab.id}` === location.hash));
  const holder = h('div');
  const buttons = tabs.map((tab) => h('button', {type: 'button', role: 'tab', id: `tab-${tab.id}`, 'aria-controls': `panel-${tab.id}`}, icon(tab.icon), tab.label));
  const panelFor = (tab, animate) => h('div', {class: animate ? 'tabpanel' : null, role: 'tabpanel', id: `panel-${tab.id}`, 'aria-labelledby': `tab-${tab.id}`}, tab.build());
  const select = (index) => buttons.forEach((element, position) => {
    element.setAttribute('aria-selected', String(position === index));
    element.tabIndex = position === index ? 0 : -1;
  });
  const show = async (index, focus) => {
    if (buttons[index].getAttribute('aria-selected') === 'true') return;
    if (holder.querySelector('form[data-dirty="true"]') && !(await confirmDiscard())) return;
    select(index);
    holder.replaceChildren(panelFor(tabs[index], true));
    history.replaceState(history.state, '', `${location.pathname}${location.search}#${tabs[index].id}`);
    reveal(buttons[index]);
    if (focus) buttons[index].focus();
  };
  buttons.forEach((element, index) => {
    element.addEventListener('click', () => show(index, false));
    element.addEventListener('keydown', (event) => {
      const target = {ArrowRight: index + 1, ArrowLeft: index - 1, Home: 0, End: tabs.length - 1}[event.key];
      if (target === undefined) return;
      event.preventDefault();
      show((target + tabs.length) % tabs.length, true);
    });
  });
  select(initial);
  holder.append(panelFor(tabs[initial], false));
  // On narrow screens the tab strip scrolls; keep a deep-linked tab visible.
  const list = h('div', {class: 'tabs', role: 'tablist', 'aria-label': label}, buttons);
  // Pages are built before the router attaches them, so wait for layout.
  const revealInitial = (frames) => {
    if (buttons[initial].isConnected) reveal(buttons[initial]);
    else if (frames > 0) requestAnimationFrame(() => revealInitial(frames - 1));
  };
  requestAnimationFrame(() => revealInitial(120));
  return h('div', null, list, holder);
}
