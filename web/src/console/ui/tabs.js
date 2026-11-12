import {h, icon} from '../../shared/dom.js';
import {confirmDiscard} from './dialogs.js';
import {getNavigationSignal} from '../api.js';

// ARIA 标签页（方向键、Home、End），选中状态同步到 URL hash，
// 因此刷新、/settings#audit 之类的链接和静默刷新都能保持标签页。
// 只滚动标签条，不滚动页面。选中下划线是标签条里唯一的指示条，切换时滑到新标签；
// 面板切换期间暂时保留原高度，再平滑收回，避免懒加载骨架让页面先塌后涨。
function reveal(button) {
  const strip = button.parentElement;
  if (!strip) return;
  const left = button.offsetLeft - strip.offsetLeft;
  if (left < strip.scrollLeft) strip.scrollLeft = left;
  else if (left + button.offsetWidth > strip.scrollLeft + strip.clientWidth) strip.scrollLeft = left + button.offsetWidth - strip.clientWidth;
}

export function tabSet(tabs, label) {
  const hash = location.hash.slice(1);
  const initial = Math.max(0, tabs.findIndex((tab) => tab.id === hash || tab.hashes?.[hash]));
  const holder = h('div', {class: 'tab-holder'});
  const indicator = h('span', {class: 'tab-indicator', 'aria-hidden': 'true'});
  const placeIndicator = (animate) => {
    const selected = buttons.find((element) => element.getAttribute('aria-selected') === 'true');
    if (!selected?.isConnected || !Number.isFinite(selected.offsetLeft) || !selected.offsetWidth) return;
    indicator.classList.toggle('instant', !animate);
    indicator.style.width = `${Math.max(0, selected.offsetWidth - 20)}px`;
    indicator.style.transform = `translateX(${selected.offsetLeft + 10}px)`;
  };
  let releaseTimer;
  const holdHeight = () => {
    const height = holder.offsetHeight;
    if (!Number.isFinite(height) || height <= 0) return;
    clearTimeout(releaseTimer);
    holder.style.transition = 'none';
    holder.style.minHeight = `${height}px`;
    releaseTimer = setTimeout(() => { holder.style.transition = ''; holder.style.minHeight = '0px'; }, 400);
  };
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
    holdHeight();
    holder.replaceChildren(panelFor(tabs[index], true));
    placeIndicator(true);
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
  const section = tabs[initial].hashes?.[hash];
  const target = section && holder.querySelector(`#${section}`);
  if (target) { target.dataset.autofocus = ''; target.dataset.sectionLink = ''; }
  // 窄屏下标签条可滚动；保持深链接的标签页可见。
  const list = h('div', {class: 'tabs', role: 'tablist', 'aria-label': label}, buttons, indicator);
  // 页面在路由器挂载前就已构建，因此须等待布局完成。
  const revealInitial = (frames) => {
    if (buttons[initial].isConnected) { reveal(buttons[initial]); placeIndicator(false); }
    else if (frames > 0) requestAnimationFrame(() => revealInitial(frames - 1));
  };
  requestAnimationFrame(() => revealInitial(120));
  const root = h('div', null, list, holder);
  root.selectTab = (id) => {
    const index = tabs.findIndex((tab) => tab.id === id || tab.hashes?.[id]);
    if (index < 0) return;
    return show(index, true).then(() => {
      if (buttons[index].getAttribute('aria-selected') !== 'true') return;
      const section = tabs[index].hashes?.[id];
      const target = section && holder.querySelector(`#${section}`);
      if (!target) return;
      history.replaceState(history.state, '', `${location.pathname}${location.search}#${id}`);
      target.focus({preventScroll: true});
      target.scrollIntoView({block: 'start', behavior: 'instant'});
    });
  };
  const signal = getNavigationSignal();
  let mounted = false;
  let disposed = false;
  const revealSelected = () => {
    const selected = buttons.find((element) => element.getAttribute('aria-selected') === 'true');
    if (root.isConnected && selected) { reveal(selected); placeIndicator(false); }
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
    clearTimeout(releaseTimer);
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
