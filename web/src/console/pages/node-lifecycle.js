import {t} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {api, getNavigationSignal, getSessionEpoch, invalidate} from '../api.js';
import {go, refresh} from '../router.js';
import {busy, button} from '../ui/components.js';
import {confirmDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {menuButton} from '../ui/menu.js';
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
        invalidate('nodes');
        await after();
      } catch (error) { toastError(error); }
      finally {
        running = false;
        busy(control, false);
        actions.forEach((item) => { item.disabled = Boolean(node.deleted); });
        if (element.isConnected && !node.deleted && !navigation?.aborted && navigation === getNavigationSignal() && epoch === getSessionEpoch()) {
          const opener = options.menu ? menu.querySelector('button') : control;
          opener.focus({preventScroll: true});
        }
      }
    };
    if (options.menu) return {label, danger: true, onSelect, bind: (item) => { control = item; actions.push(item); }};
    control = button(label, {onClick: onSelect});
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
  const items = [{label: t('nodes.enrollAgent'), onSelect: async () => {
    if (running || node.deleted) return;
    const navigation = getNavigationSignal();
    const epoch = getSessionEpoch();
    running = true;
    actions.forEach((item) => { item.disabled = true; });
    try {
      await issueEnrollment({node_id: node.id}, t('nodes.enrollment.title', {name: node.name}), () => node.deleted);
    } catch (error) { toastError(error); }
    finally {
      running = false;
      element.snapshot();
      if (menu.isConnected && !node.deleted && !navigation?.aborted && navigation === getNavigationSignal() && epoch === getSessionEpoch()) menu.querySelector('button').focus({preventScroll: true});
    }
  }},
    // 吊销会清除上报身份与观测，与删除同级使用键入确认。
    action(t('nodes.revokeCredential'), {menu: true}, () => typedConfirmation({
      title: t('nodes.revokeTitle', {name: node.name}), expected: node.name, confirm: t('nodes.revokeConfirm'),
      message: t('nodes.revokeIntro')
    }), () => api(`${path}/credential`, {method: 'DELETE'}), async () => { await refresh(); toast(t('nodes.revokeDone')); }),
    action(t('nodes.deleteNode'), {menu: true}, () => typedConfirmation({
      title: t('nodes.deleteTitle', {name: node.name}), expected: node.name, confirm: t('nodes.deleteNode'),
      message: t('nodes.deleteNodeIntro')
    }), () => api(`${path}?confirm=true`, {method: 'DELETE'}), async () => { await go('/nodes', {force: true}); toast(t('nodes.deletedDone', {name: node.name})); })];
  const menu = menuButton({label: t('nodes.more'), items});
  const menuItems = menu.querySelectorAll('[role="menuitem"]');
  items.forEach((item, index) => { if (item.bind) item.bind(menuItems[index]); else actions.push(menuItems[index]); });
  actions.push(menu.querySelector('button'));
  const element = h('div', {class: 'head-actions'}, toggle, menu);
  element.snapshot = () => {
    actions[0].textContent = node.status === 'disabled' ? t('nodes.enable') : t('nodes.disable');
    actions.forEach((control) => { control.disabled = running || Boolean(node.deleted); });
  };
  element.snapshot();
  return element;
}
