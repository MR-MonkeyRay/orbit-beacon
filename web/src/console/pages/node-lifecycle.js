import {t} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {api, getNavigationSignal, getSessionEpoch, invalidate} from '../api.js';
import {go, refresh} from '../router.js';
import {busy, button, panel} from '../ui/components.js';
import {confirmDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {issueEnrollment} from './enrollments.js';
import {typedConfirmation} from '../ui/typed-confirmation.js';

export function lifecycleActions(node) {
  const path = `/nodes/${encodeURIComponent(node.id)}`;
  const actions = [];
  let running = false;
  const action = (label, options, approve, mutate, after) => {
    let control;
    const onSelect = async () => {
      if (running || node.deleted) return;
      const navigation = getNavigationSignal();
      const epoch = getSessionEpoch();
      running = true;
      actions.forEach((item) => { item.disabled = true; });
      try {
        if (!(await approve()) || node.deleted) return;
        busy(control, true);
        await mutate();
        if (node.deleted) return;
        if (!options.enrollment) invalidate('nodes');
        await after();
      } catch (error) { toastError(error); }
      finally {
        running = false;
        busy(control, false);
        actions.forEach((item) => { item.disabled = Boolean(node.deleted); });
        if (element.isConnected && !node.deleted && !navigation?.aborted && navigation === getNavigationSignal() && epoch === getSessionEpoch()) {
          if (control.isConnected) control.focus({preventScroll: true});
        }
      }
    };
    control = button(label, {...options, onClick: onSelect});
    actions.push(control);
    return control;
  };
  let disabling;
  const toggle = action(node.status === 'disabled' ? t('nodes.enable') : t('nodes.disable'), {}, () => {
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
    }, async () => { await refresh(); toast(t(disabling ? 'nodes.disabledDone' : 'nodes.enabledDone', {name: node.name})); });
  const element = h('div', {class: 'head-actions'}, toggle);
  let management;
  element.manage = () => {
    if (management) return management;
    const install = action(t('nodes.generate'), {enrollment: true}, () => true,
      () => issueEnrollment({node_id: node.id}, t('nodes.enrollment.title', {name: node.name}), () => node.deleted), () => {});
    const revoke = action(t('nodes.revokeCredential'), {variant: 'danger'}, () => typedConfirmation({
      title: t('nodes.revokeTitle', {name: node.name}), expected: node.name, confirm: t('nodes.revokeConfirm'),
      message: t('nodes.revokeIntro')
    }), () => api(`${path}/credential`, {method: 'DELETE'}), async () => { await refresh(); toast(t('nodes.revokeDone')); });
    const remove = action(t('nodes.deleteNode'), {variant: 'danger'}, () => typedConfirmation({
      title: t('nodes.deleteTitle', {name: node.name}), expected: node.name, confirm: t('nodes.deleteNode'),
      message: t('nodes.deleteNodeIntro')
    }), () => api(`${path}?confirm=true`, {method: 'DELETE'}), async () => { await go('/nodes', {force: true}); toast(t('nodes.deletedDone', {name: node.name})); });
    const row = (title, description, control) => h('div', {class: 'node-action-row'},
      h('div', null, h('h3', null, title), description ? h('p', {class: 'text-note'}, description) : null), control);
    const danger = panel(t('nodes.dangerZone'), {},
      row(t('nodes.revokeCredential'), t('nodes.revokeIntro'), revoke),
      row(t('nodes.deleteNode'), t('nodes.deleteNodeIntro'), remove));
    danger.classList.add('node-danger');
    management = h('div', {class: 'stack'},
      panel(t('nodes.install'), {}, row(t('nodes.installCommand'), null, install)), danger);
    element.snapshot();
    return management;
  };
  element.snapshot = () => {
    const label = node.status === 'disabled' ? t('nodes.enable') : t('nodes.disable');
    if (toggle.textContent !== label) toggle.textContent = label;
    actions.forEach((control) => { control.disabled = running || Boolean(node.deleted); });
  };
  element.snapshot();
  return element;
}
