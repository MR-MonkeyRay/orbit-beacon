import {h, icon} from '../../shared/dom.js';
import {confirmDiscard} from './dialogs.js';
import {getNavigationSignal} from '../api.js';

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
  let lifecycle;
  let attached = false;
  const panelFor = (tab, animate) => {
    if (attached) lifecycle?.detach?.();
    attached = false;
    const built = tab.build();
    lifecycle = built?.element instanceof Node ? built : null;
    return h('div', {class: animate ? 'tabpanel' : null, role: 'tabpanel', id: `panel-${tab.id}`, 'aria-labelledby': `tab-${tab.id}`}, lifecycle ? lifecycle.element : built);
  };
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
  const root = h('div', null, list, holder);
  const signal = getNavigationSignal();
  let mounted = false;
  let disposed = false;
  const revealSelected = () => {
    const selected = buttons.find((element) => element.getAttribute('aria-selected') === 'true');
    if (root.isConnected && selected) reveal(selected);
  };
  const sync = () => {
    if (disposed) return;
    if (root.isConnected) {
      mounted = true;
      if (!attached) { attached = true; lifecycle?.attach?.(); }
    } else if (mounted) {
      dispose();
    }
  };
  const observer = new MutationObserver(sync);
  const dispose = () => {
    if (disposed) return;
    disposed = true;
    if (attached) lifecycle?.detach?.();
    attached = false;
    observer.disconnect();
    window.removeEventListener('resize', revealSelected);
    signal?.removeEventListener('abort', dispose);
  };
  observer.observe(document.body, {childList: true, subtree: true});
  queueMicrotask(sync);
  window.addEventListener('resize', revealSelected);
  signal?.addEventListener('abort', dispose, {once: true});
  if (signal?.aborted) dispose();
  return root;
}
