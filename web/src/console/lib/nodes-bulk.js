import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {busy, button} from '../ui/components.js';
import {confirmDialog, modal} from '../ui/dialogs.js';
import {toastError} from '../ui/feedback.js';

export async function confirmNodeDeletion(count) {
  const expected = t('nodes.deletePhrase', {count});
  const confirmation = h('input', {class: 'input', autocomplete: 'off', spellcheck: 'false', 'aria-label': t('nodes.typeConfirmLabel', {value: expected})});
  const confirm = button(t('nodes.deleteNodes'), {variant: 'danger', className: 'solid', disabled: true});
  const {closed} = modal(t('nodes.deleteBatchTitle', {count}), (close) => {
    confirmation.addEventListener('input', () => { confirm.disabled = confirmation.value !== expected; });
    confirm.addEventListener('click', () => close('confirm'));
    return h('div', null,
      h('div', {class: 'dialog-body'}, h('p', null, t('nodes.deleteIntro')),
        h('p', null, t('nodes.typeConfirm', {value: expected})), confirmation),
      h('div', {class: 'dialog-foot'}, button(t('nodes.cancel'), {onClick: () => close('dismiss')}), confirm));
  });
  return await closed === 'confirm';
}

export function showNodeBatchResults(action, results, names) {
  const succeeded = results.filter((result) => result.status === 'succeeded').length;
  const failed = results.length - succeeded;
  const label = t({disable: 'nodes.disable', enable: 'nodes.enable', delete: 'nodes.delete'}[action]);
  return modal(t('nodes.batchResults', {action: label}), (close) => h('div', null,
    h('div', {class: 'dialog-body'},
      h('p', null, t('nodes.resultCounts', {count: succeeded, failed})),
      h('ul', null, results.map((result) => h('li', null,
        h('strong', null, names.get(result.node_id) || result.node_id),
        ' — ', t(result.status === 'succeeded' ? 'nodes.result.succeeded' : 'nodes.result.failed'),
        result.error_code ? ` (${errorText({code: result.error_code})})` : '')))),
    h('div', {class: 'dialog-foot'}, button(t('nodes.close'), {variant: 'primary', onClick: () => close('done')})))).closed;
}

export async function runNodeBatch({action, selected, nodes, signal, controls, reloadNodes, onCompleted}) {
  const ids = [...selected];
  if (ids.length === 0) return;
  const confirmed = action === 'delete' ? await confirmNodeDeletion(ids.length) : await confirmDialog({
    title: t(action === 'disable' ? 'nodes.disableBatchTitle' : 'nodes.enableBatchTitle', {count: ids.length}),
    message: action === 'disable' ? t('nodes.batchDisableIntro') : t('nodes.batchEnableIntro'),
    confirm: action === 'disable' ? t('nodes.disableNodes') : t('nodes.enableNodes')
  });
  if (!confirmed || signal?.aborted) return;
  controls.forEach((control) => busy(control, true));
  const names = new Map(nodes.map((node) => [node.id, node.name]));
  try {
    const response = await api('/nodes/batch', {method: 'POST', body: {node_ids: ids, action, ...(action === 'delete' ? {confirm: true} : {})}});
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
