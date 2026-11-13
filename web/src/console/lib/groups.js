import {t} from '../i18n.js';
import {h, icon} from '../../shared/dom.js';
import {api, invalidate} from '../api.js';
import {button, emptyState, iconButton} from '../ui/components.js';
import {confirmDialog, formDialog, modal} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, select, validateBytes} from '../ui/forms.js';

// 节点分组与监测分组各自独立，接口同构；通用文案共用 nodes.* 键，只有删除说明和成员计数不同。
export const NODE_GROUPS = {path: '/groups', field: 'groups', cache: ['groups'], members: 'nodes', deleteHint: 'nodes.deleteGroupHint', count: 'nodes.nodeCount'};
export const SERVICE_GROUPS = {path: '/service-groups', field: 'service_groups', cache: ['serviceGroups'], members: 'services', deleteHint: 'services.deleteGroupHint', count: 'services.count'};

const NEW_GROUP = '__new';
const groupPath = (kind, id) => `${kind.path}/${encodeURIComponent(id)}`;

// 新建（item 为空）或重命名；保存成功返回服务器上的分组，取消返回 null。
export async function editGroup(item, kind = NODE_GROUPS) {
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
        ? await api(groupPath(kind, item.id), {method: 'PATCH', body: {name: name.value}})
        : await api(kind.path, {method: 'POST', body: {name: name.value}});
      invalidate(...kind.cache);
      toast(t(item ? 'nodes.groupRenamed' : 'nodes.groupAdded', {name: saved.name}));
    }
  });
  return await dialog.closed === 'done' ? saved : null;
}

// confirmed() 在确认后、请求前调用，调用方据此锁住其它分组操作。
export async function deleteGroup(item, {confirmed, kind = NODE_GROUPS} = {}) {
  if (!await confirmDialog({title: t('nodes.deleteGroupNamed', {name: item.name}),
    message: t(kind.deleteHint), confirm: t('nodes.deleteGroup'), danger: true})) return false;
  confirmed?.();
  try { await api(groupPath(kind, item.id), {method: 'DELETE'}); }
  catch (error) { toastError(error); return false; }
  invalidate(...kind.cache, kind.members);
  toast(t('nodes.groupDeleted', {name: item.name}));
  return true;
}

// 分组下拉：末项“新建分组…”打开新建对话框，成功后选中新分组；取消时回到原值。
export function groupSelect(groups, value, props = {}, {extra = [], onCreated, kind = NODE_GROUPS} = {}) {
  const control = select([['', t('nodes.ungrouped')], ...groups.map((group) => [group.id, group.name]), ...extra,
    [NEW_GROUP, t('nodes.newGroup')]], value || '', props);
  let previous = control.value;
  // 表单重置会直接改值；获得焦点时重新记下，取消新建时才能回到正确的值。
  control.addEventListener('focus', () => { if (control.value !== NEW_GROUP) previous = control.value; });
  control.addEventListener('change', async () => {
    if (control.value !== NEW_GROUP) { previous = control.value; return; }
    control.value = previous;
    const created = await editGroup(null, kind);
    if (!created || !control.isConnected) return;
    control.insertBefore(h('option', {value: created.id}, created.name), [...control.options].find((option) => option.value === NEW_GROUP));
    control.value = previous = created.id;
    onCreated?.(created);
    control.dispatchEvent(new Event('change', {bubbles: true}));
  });
  return control;
}

// 管理分组：顺序、重命名、删除、新建。同一时间只进行一项变更；请求和随后的页面刷新期间
// 全部控件锁定，changed({members}) 重新读取分组（members 表示成员也已改变），再按新数据重绘。
export function manageGroupsDialog({groups, counts, changed, kind = NODE_GROUPS}) {
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
    // 按 ID 定位：页面轮询可能已在对话框打开期间换掉分组列表，渲染时的下标不再可靠。
    const move = async (id, delta) => {
      if (pending) return;
      const ids = groups().map((group) => group.id);
      const index = ids.indexOf(id);
      if (index < 0 || index + delta < 0 || index + delta >= ids.length) { render(); return; }
      ids.splice(index, 1);
      ids.splice(index + delta, 0, id);
      lock();
      try { await api(`${kind.path}/order`, {method: 'PUT', body: {ids}}); invalidate(...kind.cache); }
      catch (error) { if (!signal.aborted) toastError(error); }
      await settle({}, {id, action: delta < 0 ? 'up' : 'down'});
    };
    const edit = async (group) => {
      if (pending) return;
      const saved = await editGroup(group, kind);
      if (!saved || signal.aborted) return;
      lock();
      await settle({}, {id: saved.id, action: 'edit'});
    };
    const remove = async (group, neighbor) => {
      if (pending) return;
      let locked = false;
      const deleted = await deleteGroup(group, {kind, confirmed: () => { locked = true; lock(); }});
      if (locked) await settle({members: deleted}, deleted ? {id: neighbor, action: 'delete'} : {id: group.id, action: 'delete'});
    };
    const add = button(t('nodes.addGroup'), {icon: 'plus', small: true, onClick: () => edit(null)});
    const rows = new Map();
    function render(focus) {
      const items = groups();
      const sizes = counts();
      rows.clear();
      list.replaceChildren(...items.map((group, index) => {
        const controls = {
          up: iconButton('up', t('nodes.moveUpNamed', {name: group.name}), () => move(group.id, -1)),
          down: iconButton('down', t('nodes.moveDownNamed', {name: group.name}), () => move(group.id, 1)),
          edit: iconButton('edit', t('nodes.renameGroupNamed', {name: group.name}), () => edit(group)),
          delete: iconButton('trash', t('nodes.deleteGroupLabel', {name: group.name}), () => remove(group, items[index + 1]?.id || items[index - 1]?.id))
        };
        controls.up.disabled = index === 0;
        controls.down.disabled = index === items.length - 1;
        rows.set(group.id, controls);
        return h('li', null, icon('folder', 'muted'),
          h('div', {class: 'grow'}, h('div', {class: 'title'}, group.name), h('div', {class: 'sub'}, t(kind.count, {count: sizes.get(group.id) || 0}))),
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
