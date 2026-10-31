import {t} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {api, invalidate} from '../api.js';
import {go, refresh} from '../router.js';
import {busy, button} from '../ui/components.js';
import {confirmDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {typedConfirmation} from '../ui/typed-confirmation.js';

export function lifecycleActions(node) {
  const path = `/nodes/${encodeURIComponent(node.id)}`;
  const actions = [];
  let running = false;
  const action = (label, options, approve, mutate, after) => {
    const control = button(label, {...options, onClick: async () => {
      if (running || node.deleted) return;
      running = true;
      actions.forEach((item) => { item.disabled = true; });
      try {
        if (!(await approve()) || node.deleted) return;
        busy(control, true);
        await mutate();
        if (node.deleted) return;
        invalidate('nodes');
        await after();
      } catch (error) { toastError(error); }
      finally {
        running = false;
        busy(control, false);
        actions.forEach((item) => { item.disabled = Boolean(node.deleted); });
      }
    }});
    actions.push(control);
    return control;
  };
  let disabling;
  const element = h('div', {class: 'head-actions'},
    action(node.status === 'disabled' ? t('nodes.enable') : t('nodes.disable'), {}, () => {
      disabling = node.status !== 'disabled';
      return confirmDialog({
        title: t(disabling ? 'nodes.disableTitle' : 'nodes.enableTitle', {name: node.name}), confirm: t(disabling ? 'nodes.disableNode' : 'nodes.enableNode'), danger: disabling,
        message: disabling
          ? t('nodes.disableIntro')
          : t('nodes.enableIntro')
      });
    }, async () => {
      const {results} = await api('/nodes/batch', {method: 'POST', body: {node_ids: [node.id], action: disabling ? 'disable' : 'enable'}});
      const result = results.find((item) => item.node_id === node.id);
      if (result?.status !== 'succeeded') throw Object.assign(new Error('Node action failed'), {code: result?.error_code});
    }, async () => { await refresh(); toast(t(disabling ? 'nodes.disabledDone' : 'nodes.enabledDone', {name: node.name})); }),
    action(t('nodes.revokeCredential'), {variant: 'danger', className: 'btn-quiet'}, () => confirmDialog({
      title: t('nodes.revokeTitle', {name: node.name}), confirm: t('nodes.revokeConfirm'), danger: true,
      message: t('nodes.revokeIntro')
    }), () => api(`${path}/credential`, {method: 'DELETE'}), async () => { await refresh(); toast(t('nodes.revokeDone')); }),
    action(t('nodes.deleteNode'), {variant: 'danger', className: 'btn-quiet'}, () => typedConfirmation({
      title: t('nodes.deleteTitle', {name: node.name}), expected: node.name, confirm: t('nodes.deleteNode'),
      message: t('nodes.deleteNodeIntro')
    }), () => api(`${path}?confirm=true`, {method: 'DELETE'}), async () => { await go('/nodes', {force: true}); toast(t('nodes.deletedDone', {name: node.name})); }));
  element.snapshot = () => {
    actions[0].textContent = node.status === 'disabled' ? t('nodes.enable') : t('nodes.disable');
    if (node.deleted) actions.forEach((control) => { control.disabled = true; });
  };
  return element;
}
