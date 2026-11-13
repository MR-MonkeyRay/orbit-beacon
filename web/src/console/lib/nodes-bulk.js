import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {byteLength, h} from '../../shared/dom.js';
import {api} from '../api.js';
import {busy, button} from '../ui/components.js';
import {confirmDialog, modal} from '../ui/dialogs.js';
import {typedConfirmation} from '../ui/typed-confirmation.js';
import {field, input, onSubmit} from '../ui/forms.js';
import {groupSelect} from './node-groups.js';
import {toastError} from '../ui/feedback.js';

export const confirmNodeDeletion = (count) => typedConfirmation({
  title: t('nodes.deleteBatchTitle', {count}), expected: t('nodes.deletePhrase', {count}),
  confirm: t('nodes.deleteNodes'), message: t('nodes.deleteIntro')
});

const actionKey = (action) => ({disable: 'nodes.disable', enable: 'nodes.enable', delete: 'nodes.delete', group: 'nodes.setGroup', tag: 'nodes.addTag'})[action];

async function batchMetadataInput(action, groups) {
  let value;
  const dialog = modal(t(actionKey(action)), (close) => {
    const control = action === 'group' ? groupSelect(groups, '') : input({required: true});
    const submit = button(t(actionKey(action)), {type: 'submit', variant: 'primary'});
    const form = h('form', {class: 'form'}, field(t(action === 'group' ? 'nodes.group' : 'nodes.tags'), control),
      h('div', {class: 'dialog-foot'}, button(t('controls.cancel'), {onClick: () => close()}), submit));
    control.dataset.autofocus = '';
    const validate = () => {
      control.setCustomValidity(action === 'tag' ? !control.value.trim() ? t('controls.validation.required')
        : byteLength(control.value) > 255 ? t('controls.tags.maximum') : '' : '');
    };
    control.addEventListener('input', validate);
    onSubmit(form, submit, () => {
      validate();
      if (!control.validity.valid) { control.showError(control.validationMessage); control.focus(); return; }
      value = control.value;
      close('confirm');
    });
    return h('div', {class: 'dialog-body'}, form);
  });
  return await dialog.closed === 'confirm' ? {value} : null;
}

// /nodes/batch 只接受状态操作；元数据逐节点 PATCH，失败继续，导航后不再发送。
export async function patchNodeBatch({action, ids, nodes, value, signal}) {
  const results = [];
  for (const id of ids) {
    if (signal?.aborted) break;
    const node = nodes.find((node) => node.id === id);
    try {
      const body = action === 'group' ? {group_id: value || null} : {tags: [...new Set([...node.tags, value])]};
      await api(`/nodes/${id}`, {method: 'PATCH', body, signal});
      if (signal?.aborted) break;
      results.push({node_id: id, status: 'succeeded'});
    } catch (error) {
      if (signal?.aborted) break;
      results.push({node_id: id, status: 'failed', error_code: error.code || 'internal_error'});
    }
  }
  return {results};
}

export function showNodeBatchResults(action, results, names) {
  const succeeded = results.filter((result) => result.status === 'succeeded').length;
  const failed = results.length - succeeded;
  const label = t(actionKey(action));
  return modal(t('nodes.batchResults', {action: label}), (close) => h('div', null,
    h('div', {class: 'dialog-body'},
      h('p', null, t('nodes.resultCounts', {count: succeeded, failed})),
      h('ul', null, results.map((result) => h('li', null,
        h('strong', null, names.get(result.node_id) || result.node_id),
        ' — ', t(result.status === 'succeeded' ? 'nodes.result.succeeded' : 'nodes.result.failed'),
        result.error_code ? ` (${errorText({code: result.error_code})})` : '')))),
    h('div', {class: 'dialog-foot'}, button(t('nodes.close'), {variant: 'primary', onClick: () => close('done')})))).closed;
}

export async function runNodeBatch({action, selected, nodes, groups = [], signal, controls, reloadNodes, onCompleted}) {
  const ids = [...selected];
  if (ids.length === 0) return;
  const metadata = action === 'group' || action === 'tag';
  const choice = metadata ? await batchMetadataInput(action, groups) : null;
  const confirmed = metadata ? Boolean(choice) : action === 'delete' ? await confirmNodeDeletion(ids.length) : await confirmDialog({
    title: t(action === 'disable' ? 'nodes.disableBatchTitle' : 'nodes.enableBatchTitle', {count: ids.length}),
    message: action === 'disable' ? t('nodes.batchDisableIntro') : t('nodes.batchEnableIntro'),
    confirm: action === 'disable' ? t('nodes.disableNodes') : t('nodes.enableNodes')
  });
  if (!confirmed || signal?.aborted) return;
  controls.forEach((control) => busy(control, true));
  const names = new Map(nodes.map((node) => [node.id, node.name]));
  try {
    const response = metadata ? await patchNodeBatch({action, ids, nodes, value: choice.value, signal})
      : await api('/nodes/batch', {method: 'POST', signal, body: {node_ids: ids, action, ...(action === 'delete' ? {confirm: true} : {})}});
    if (signal?.aborted) return;
    // 逐节点事务完成后重新加载权威状态；
    // 批量接口在启用节点时可能保留先前状态。
    try { await reloadNodes(); } catch {}
    if (signal?.aborted) return;
    onCompleted();
    await showNodeBatchResults(action, response.results, names);
  } catch (error) { if (!signal?.aborted) toastError(error); }
  finally { controls.forEach((control) => busy(control, false)); }
}
