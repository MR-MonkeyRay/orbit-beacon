import {t} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {errorText} from '../../shared/errors.js';
import {api} from '../api.js';
import {busy, button} from '../ui/components.js';
import {confirmDialog, modal} from '../ui/dialogs.js';
import {field, onSubmit} from '../ui/forms.js';
import {toastError} from '../ui/feedback.js';
import {SERVICE_GROUPS, groupSelect} from './groups.js';

export const BATCH_ACTIONS = ['move', 'pause', 'resume', 'delete'];
const label = (action) => t(`services.batch.${action}`);

// 移动的目标分组；返回 {value}（'' 表示未分组），取消返回 null。
async function chooseGroup(groups) {
  let value;
  const dialog = modal(label('move'), (close) => {
    const control = groupSelect(groups, '', {name: 'group_id', dataset: {autofocus: ''}}, {kind: SERVICE_GROUPS});
    const submit = button(label('move'), {type: 'submit', variant: 'primary'});
    const form = h('form', {class: 'form'}, field(t('nodes.group'), control),
      h('div', {class: 'dialog-foot'}, button(t('controls.cancel'), {onClick: () => close()}), submit));
    onSubmit(form, submit, () => { value = control.value; close('confirm'); });
    return h('div', {class: 'dialog-body'}, form);
  });
  return await dialog.closed === 'confirm' ? {value} : null;
}

function showFailures(failed, names) {
  return modal(t('services.batch.failed', {count: failed.length}), (close) => h('div', null,
    h('div', {class: 'dialog-body'}, h('ul', null, failed.map((result) => h('li', null,
      h('strong', null, names.get(result.service_id) || result.service_id), ' — ', errorText(result.error || {code: result.error_code}))))),
    h('div', {class: 'dialog-foot'}, button(t('nodes.close'), {variant: 'primary', onClick: () => close('done')})))).closed;
}

// 每个监测在服务端各自一个事务；成功的不再提示，表格刷新就是反馈，只列出失败项。
// 请求完成后先调用 onCompleted（更新选择和焦点），再弹出失败列表，关闭后焦点回到新位置。
// 返回 true 表示请求已完成，取消或请求失败返回 false。
export async function runServiceBatch({action, ids, services, groups, signal, controls = [], onCompleted}) {
  if (!ids.length || signal?.aborted) return false;
  let groupID;
  if (action === 'move') {
    const choice = await chooseGroup(groups);
    if (!choice) return false;
    groupID = choice.value || null;
  }
  if (action === 'delete' && !await confirmDialog({title: t('services.batch.deleteTitle', {count: ids.length}),
    message: t('services.deleteHint'), confirm: label('delete'), danger: true})) return false;
  if (signal?.aborted) return false;
  controls.forEach((control) => busy(control, true));
  try {
    const response = await api('/services/batch', {method: 'POST', signal, body: {service_ids: ids, action,
      ...(action === 'move' ? {group_id: groupID} : {}), ...(action === 'delete' ? {confirm: true} : {})}});
    if (signal?.aborted) return true;
    if (!Array.isArray(response?.results)) throw Object.assign(new Error('Invalid batch response'), {code: 'unreadable_response'});
    const failed = response.results.filter((result) => result.status !== 'succeeded');
    onCompleted?.();
    if (failed.length) void showFailures(failed, new Map(services.map((item) => [item.id, item.name])));
    return true;
  } catch (error) {
    if (!signal?.aborted && error.name !== 'AbortError') toastError(error);
    return false;
  } finally { controls.forEach((control) => busy(control, false)); }
}
