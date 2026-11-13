import {t} from '../i18n.js';
import {h, icon} from '../../shared/dom.js';
import {api, getNavigationSignal, getSessionEpoch, invalidate} from '../api.js';
import {displayStatus} from '../format.js';
import {busy, button, emptyState, iconButton} from '../ui/components.js';
import {confirmDialog, formDialog, modal} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, select, validateBytes} from '../ui/forms.js';
import {menuButton} from '../ui/menu.js';
import {issueEnrollment} from '../pages/enrollments.js';
import {groupCommandItems} from './group-commands.js';

const NEW_GROUP = '__new';

// 新建（item 为空）或重命名；保存成功返回服务器上的分组，取消返回 null。
export async function editGroup(item) {
  let name;
  let saved = null;
  const dialog = formDialog({
    title: item ? t('nodes.renameGroupNamed', {name: item.name}) : t('nodes.addGroup'),
    submit: item ? t('nodes.rename') : t('nodes.addGroup'),
    build: (form) => {
      name = input({name: 'name', required: true, value: item?.name || '', autocomplete: 'off'});
      validateBytes(name, 1, 255, t('nodes.groupName'));
      form.append(field(t('nodes.groupName'), name));
    },
    action: async () => {
      saved = item
        ? await api(`/groups/${encodeURIComponent(item.id)}`, {method: 'PATCH', body: {name: name.value}})
        : await api('/groups', {method: 'POST', body: {name: name.value}});
      invalidate('groups');
      toast(t(item ? 'nodes.groupRenamed' : 'nodes.groupAdded', {name: saved.name}));
    }
  });
  return await dialog.closed === 'done' ? saved : null;
}

// confirmed() 在确认后、请求前调用，调用方据此锁住其它分组操作。
export async function deleteGroup(item, {confirmed} = {}) {
  if (!await confirmDialog({title: t('nodes.deleteGroupNamed', {name: item.name}),
    message: t('nodes.deleteGroupHint'), confirm: t('nodes.deleteGroup'), danger: true})) return false;
  confirmed?.();
  try { await api(`/groups/${encodeURIComponent(item.id)}`, {method: 'DELETE'}); }
  catch (error) { toastError(error); return false; }
  invalidate('groups', 'nodes');
  toast(t('nodes.groupDeleted', {name: item.name}));
  return true;
}

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

// 分组下拉：末项“新建分组…”打开新建对话框，成功后选中新分组；取消时回到原值。
export function groupSelect(groups, value, props = {}, {extra = [], onCreated} = {}) {
  const control = select([['', t('nodes.ungrouped')], ...groups.map((group) => [group.id, group.name]), ...extra,
    [NEW_GROUP, t('nodes.newGroup')]], value || '', props);
  let previous = control.value;
  // 表单重置会直接改值；获得焦点时重新记下，取消新建时才能回到正确的值。
  control.addEventListener('focus', () => { if (control.value !== NEW_GROUP) previous = control.value; });
  control.addEventListener('change', async () => {
    if (control.value !== NEW_GROUP) { previous = control.value; return; }
    control.value = previous;
    const created = await editGroup(null);
    if (!created || !control.isConnected) return;
    control.insertBefore(h('option', {value: created.id}, created.name), [...control.options].find((option) => option.value === NEW_GROUP));
    control.value = previous = created.id;
    onCreated?.(created);
    control.dispatchEvent(new Event('change', {bubbles: true}));
  });
  return control;
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

// 管理分组：顺序、重命名、删除、新建。同一时间只进行一项变更；请求和随后的页面刷新期间
// 全部控件锁定，changed({nodes}) 重新读取分组（nodes 表示节点也已改变），再按新数据重绘。
export function manageGroupsDialog({groups, counts, changed}) {
  return modal(t('nodes.manageGroups'), (close, signal) => {
    const list = h('ul', {class: 'rows group-rows'});
    const body = h('div', {class: 'dialog-body stack'});
    let pending = false;
    const lock = () => {
      pending = true;
      for (const control of [...list.querySelectorAll('button'), add]) control.disabled = true;
    };
    const settle = async (change, focus) => {
      try { await changed(change); } catch (error) { if (!signal.aborted) toastError(error); }
      pending = false;
      if (!signal.aborted) render(focus);
    };
    const move = async (index, delta) => {
      if (pending) return;
      const ids = groups().map((group) => group.id);
      const [id] = ids.splice(index, 1);
      ids.splice(index + delta, 0, id);
      lock();
      try { await api('/groups/order', {method: 'PUT', body: {ids}}); }
      catch (error) { if (!signal.aborted) toastError(error); }
      await settle({}, {id, action: delta < 0 ? 'up' : 'down'});
    };
    const edit = async (group) => {
      if (pending) return;
      const saved = await editGroup(group);
      if (!saved || signal.aborted) return;
      lock();
      await settle({}, {id: saved.id, action: 'edit'});
    };
    const remove = async (group, neighbor) => {
      if (pending) return;
      let locked = false;
      const deleted = await deleteGroup(group, {confirmed: () => { locked = true; lock(); }});
      if (locked) await settle({nodes: deleted}, deleted ? {id: neighbor, action: 'delete'} : {id: group.id, action: 'delete'});
    };
    const add = button(t('nodes.addGroup'), {icon: 'plus', small: true, onClick: () => edit(null)});
    const rows = new Map();
    function render(focus) {
      const items = groups();
      const sizes = counts();
      rows.clear();
      list.replaceChildren(...items.map((group, index) => {
        const controls = {
          up: iconButton('up', t('nodes.moveUpNamed', {name: group.name}), () => move(index, -1)),
          down: iconButton('down', t('nodes.moveDownNamed', {name: group.name}), () => move(index, 1)),
          edit: iconButton('edit', t('nodes.renameGroupNamed', {name: group.name}), () => edit(group)),
          delete: iconButton('trash', t('nodes.deleteGroupLabel', {name: group.name}), () => remove(group, items[index + 1]?.id || items[index - 1]?.id))
        };
        controls.up.disabled = index === 0;
        controls.down.disabled = index === items.length - 1;
        rows.set(group.id, controls);
        return h('li', null, icon('folder', 'muted'),
          h('div', {class: 'grow'}, h('div', {class: 'title'}, group.name), h('div', {class: 'sub'}, t('nodes.nodeCount', {count: sizes.get(group.id) || 0}))),
          h('div', {class: 'group-row-actions'}, Object.values(controls)));
      }));
      add.disabled = false;
      body.replaceChildren(items.length ? list : emptyState(t('nodes.noGroups'), null), h('div', null, add));
      if (!focus) return;
      const row = rows.get(focus.id);
      const target = row ? [focus.action, 'up', 'down', 'edit'].map((action) => row[action]).find((control) => control && !control.disabled) : add;
      target.focus();
    }
    render();
    return h('div', null, body, h('div', {class: 'dialog-foot'}, button(t('nodes.close'), {variant: 'primary', onClick: () => close('done')})));
  });
}
