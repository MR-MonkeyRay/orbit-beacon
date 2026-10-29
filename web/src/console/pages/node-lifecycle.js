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
    action(node.status === 'disabled' ? 'Enable' : 'Disable', {}, () => {
      disabling = node.status !== 'disabled';
      const verb = disabling ? 'Disable' : 'Enable';
      return confirmDialog({
        title: `${verb} ${node.name}?`, confirm: `${verb} node`, danger: disabling,
        message: disabling
          ? 'A disabled node’s agent is rejected with 403 node_disabled until the node is re-enabled. The asset and its credential are kept.'
          : 'The agent will be accepted again with its existing credential.'
      });
    }, async () => {
      const {results} = await api('/nodes/batch', {method: 'POST', body: {node_ids: [node.id], action: disabling ? 'disable' : 'enable'}});
      const result = results.find((item) => item.node_id === node.id);
      if (result?.status !== 'succeeded') throw new Error(`${node.name}: ${result?.status || 'failed'} (${result?.error_code || 'missing_result'}).`);
    }, async () => { await refresh(); toast(`${node.name} ${disabling ? 'disabled' : 'enabled'}.`); }),
    action('Revoke agent credential', {variant: 'danger', className: 'btn-quiet'}, () => confirmDialog({
      title: `Revoke ${node.name}’s agent credential?`, confirm: 'Revoke credential', danger: true,
      message: 'The asset, metadata, traffic billing and public settings are kept. Last reported settings, system details and probe observations are cleared. The agent stops being accepted (401), and this node disappears from the public page. A new enrollment ticket is needed to enroll the agent again.'
    }), () => api(`${path}/credential`, {method: 'DELETE'}), async () => { await refresh(); toast('Agent credential revoked. Issue a new enrollment ticket to enroll again.'); }),
    action('Delete node', {variant: 'danger', className: 'btn-quiet'}, () => typedConfirmation({
      title: `Delete ${node.name}?`, expected: node.name, confirm: 'Delete node',
      message: 'This permanently deletes the asset and revokes its agent identity. This cannot be undone.'
    }), () => api(`${path}?confirm=true`, {method: 'DELETE'}), async () => { await go('/nodes', {force: true}); toast(`Deleted ${node.name}.`); }));
  element.snapshot = () => {
    actions[0].textContent = node.status === 'disabled' ? 'Enable' : 'Disable';
    if (node.deleted) actions.forEach((control) => { control.disabled = true; });
  };
  return element;
}
