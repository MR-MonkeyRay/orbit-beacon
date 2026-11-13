import {t} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {getNavigationSignal, getSessionEpoch} from '../api.js';
import {displayStatus} from '../format.js';
import {busy, button} from '../ui/components.js';
import {toastError} from '../ui/feedback.js';
import {menuButton} from '../ui/menu.js';
import {issueEnrollment} from '../pages/enrollments.js';

// 部署凭据属于当前导航和会话；越过边界后不再把焦点还给触发按钮。
export async function deployToGroup(item, trigger) {
  const navigation = getNavigationSignal();
  const epoch = getSessionEpoch();
  busy(trigger, true);
  try { await issueEnrollment({group_id: item.id}, t('nodes.deployAgentFor', {name: item.name})); }
  catch (error) { toastError(error); }
  finally {
    busy(trigger, false);
    if (trigger.isConnected && !trigger.disabled && !navigation?.aborted && navigation === getNavigationSignal() && epoch === getSessionEpoch()) trigger.focus({preventScroll: true});
  }
}

export function groupCounts(nodes) {
  const total = new Map();
  const offline = new Set();
  for (const node of nodes) {
    const key = node.group_id || 'none';
    total.set(key, (total.get(key) || 0) + 1);
    if (displayStatus(node) === 'offline') offline.add(key);
  }
  return {total, offline};
}

// 节点页分组标签：全部、各分组（服务器顺序）、未分组。标签元素按键复用，更新不抢焦点。
// 宽屏标签换行，选中分组的操作菜单紧跟其后；手机宽度标签单行横向滚动，
// 菜单移到滚动区外（否则会被裁切），与管理按钮并排。
export function nodeGroupBar({onSelect, onManage, menuItems, signal}) {
  const chips = h('div', {class: 'group-chips', role: 'group', 'aria-label': t('nodes.filterGroup')});
  // 手机宽度下只留图标，文字仍是按钮的无障碍名称。
  const manage = button(h('span', {class: 'group-filter-label'}, t('nodes.manageGroups')), {small: true, icon: 'folder', onClick: onManage, className: 'group-filter-manage'});
  manage.title = t('nodes.manageGroups');
  const slot = h('span', {class: 'group-filter-menu'});
  const root = h('div', {class: 'group-filter'}, chips, slot, manage);
  const entries = new Map();
  const compact = globalThis.matchMedia?.('(max-width: 767px)');
  let menu = null;
  let shown = null;
  // 跨断点移动菜单会让其中的焦点丢失，移动后还给原控件。
  const place = () => {
    const target = compact?.matches ? slot : entries.get(menu?.id)?.wrap;
    if (!menu || !target || menu.root.parentElement === target) return;
    const focused = menu.root.contains(document.activeElement) ? document.activeElement : null;
    target.append(menu.root);
    focused?.focus();
  };
  compact?.addEventListener?.('change', place, {signal});
  const entry = (key) => {
    let item = entries.get(key);
    if (item) return item;
    const name = h('span', {class: 'group-chip-name'});
    const dot = h('span', {class: 'group-chip-alert', 'aria-hidden': 'true', hidden: true});
    const count = h('span', {class: 'count'});
    const alert = h('span', {class: 'sr', hidden: true}, t('nodes.groupHasOffline'));
    const chip = h('button', {type: 'button', class: 'group-chip', onclick: () => onSelect(key)}, name, dot, count, alert);
    item = {chip, name, dot, alert, count, wrap: h('span', {class: 'group-chip-wrap', dataset: {group: key || 'all'}}, chip)};
    entries.set(key, item);
    return item;
  };
  root.update = ({groups, nodes, active}) => {
    // 删除分组会先移除其标签（菜单在其中），浏览器随即把焦点移到 body，所以要在任何移除之前记录。
    const hadFocus = Boolean(menu?.root.contains(document.activeElement));
    const {total, offline} = groupCounts(nodes);
    const list = [['', t('nodes.all'), nodes.length, false],
      ...groups.map((group) => [group.id, group.name, total.get(group.id) || 0, offline.has(group.id)])];
    if (total.get('none') || active === 'none') list.push(['none', t('nodes.ungrouped'), total.get('none') || 0, offline.has('none')]);
    const keys = new Set(list.map(([key]) => key));
    for (const [key, item] of entries) if (!keys.has(key)) { item.wrap.remove(); entries.delete(key); }
    const wraps = list.map(([key, label, count, down]) => {
      const item = entry(key);
      if (item.name.textContent !== label) item.name.textContent = label;
      if (item.count.textContent !== String(count)) item.count.textContent = String(count);
      item.dot.hidden = item.alert.hidden = !down;
      item.chip.setAttribute('aria-pressed', String(key === active));
      return item.wrap;
    });
    // 顺序不变时不移动节点，移动会让焦点中的标签失焦。
    if (wraps.some((wrap, index) => chips.children[index] !== wrap)) chips.replaceChildren(...wraps);
    const group = groups.find((item) => item.id === active);
    const signature = group ? `${group.id}\n${group.name}` : '';
    if (menu?.signature !== signature) {
      // 重命名会重建菜单、删除会移除菜单；焦点原在菜单上时交给新菜单或当前标签。
      menu?.root.remove();
      menu = null;
      if (group) {
        const root = menuButton({label: t('nodes.groupActions', {name: group.name}), icon: 'more', iconOnly: true, items: menuItems(group, () => root.querySelectorAll('button'))});
        root.classList.add('group-chip-menu');
        menu = {id: group.id, signature, root};
        place();
      }
      if (hadFocus) (menu ? menu.root.querySelector('button') : entries.get(active)?.chip)?.focus();
    }
    // 切换分组后让选中的标签进入单行滚动区的可见范围；首次渲染时标签条尚未挂载，延后一帧。
    if (shown !== active) {
      shown = active;
      const chip = entries.get(active)?.chip;
      if (compact?.matches && chip) requestAnimationFrame(() => { if (chip.isConnected) chip.scrollIntoView({block: 'nearest', inline: 'nearest'}); });
    }
  };
  return root;
}
