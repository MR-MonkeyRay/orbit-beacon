import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {shortID, when} from '../format.js';
import {busy, button, callout, emptyState, note, panel} from '../ui/components.js';
import {confirmDialog, modal} from '../ui/dialogs.js';
import {toastError} from '../ui/feedback.js';
import {enrollmentCommand, enrollmentCountdown, installerCommand, enrollmentState, nodeEnrollments} from './enrollment-utils.js';

// Same selection + execCommand fallback as the node-ID copy control.
async function copyText(element, status) {
  try {
    if (!navigator.clipboard?.writeText) throw new Error('Clipboard API unavailable');
    await navigator.clipboard.writeText(element.textContent);
    status.textContent = 'Copied.';
  } catch {
    const range = document.createRange();
    range.selectNodeContents(element);
    const selection = getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    let copied = false;
    try { copied = document.execCommand('copy'); } catch {}
    status.textContent = copied ? 'Copied using the browser fallback.' : 'The text is selected. Press Ctrl+C or Command+C to copy it.';
  }
}

export async function issueEnrollment(target, title) {
  const ticket = await api('/enrollments', {method: 'POST', body: target});
  const token = h('code', null, ticket.token);
  const installer = h('code', null, installerCommand(ticket.token, location.origin));
  const command = h('code', null, enrollmentCommand(ticket.token));
  // Plaintext is retained only by this dialog, never by ticket-list state.
  delete ticket.token;
  const countdown = h('span', {class: 'mono', role: 'timer', 'aria-live': 'off'});
  const status = h('p', {class: 'hint', role: 'status'});
  const tick = () => { countdown.textContent = enrollmentCountdown(ticket.expires_at); };
  tick();
  const {closed} = modal(title, (close) => h('div', null,
    h('div', {class: 'dialog-body stack'},
      callout('This token is shown only once. Closing this dialog loses it; it cannot be retrieved later.'),
      h('p', {class: 'lede'}, target.mode === 'batch'
        ? 'Batch enrollment ticket: valid for 30 minutes, up to 100 uses, subject to node capacity.'
        : 'Single-node enrollment ticket: valid for 15 minutes and one use.'),
      h('p', null, countdown, ' · Expires ', when(ticket.expires_at)),
      h('pre', {class: 'code enrollment-token'}, token),
      button('Copy token', {icon: 'copy', onClick: () => copyText(token, status)}),
      h('p', {class: 'lede'}, 'On a Linux host with systemd, download and inspect deploy/install-agent.sh from the release that matches this server, set VERSION to that release, then run:'),
      h('pre', {class: 'code'}, installer),
      button('Copy install command', {icon: 'copy', onClick: () => copyText(installer, status)}),
      h('p', {class: 'lede'}, 'For a manual installation, set up the installation policy and private state directory as described in docs/agent.md, then run this command as the non-root agent user specified by that policy:'),
      h('pre', {class: 'code'}, command),
      button('Copy enroll command', {icon: 'copy', onClick: () => copyText(command, status)}), status),
    h('div', {class: 'dialog-foot'}, button('Close', {onClick: () => close('done')}))));
  const timer = setInterval(tick, 1000);
  try { await closed; }
  finally {
    clearInterval(timer);
    token.textContent = '';
    installer.textContent = '';
    command.textContent = '';
  }
}

export function enrollmentTab(node, editable) {
  if (!editable) return panel('Enroll agent', {},
    note('Only administrators can issue or view enrollment tickets. Enrollment does not change the node’s desired configuration.'),
    h('p', {class: 'lede'}, 'Agent state: ', node.status, '.'));

  const list = h('div', {'aria-live': 'polite'});
  const error = h('div', {class: 'callout', role: 'alert', hidden: true});
  let loading = false;
  const reload = async () => {
    if (loading) return;
    loading = true;
    busy(retry, true);
    error.hidden = true;
    try {
      const tickets = await nodeEnrollments(node.id, api);
      list.replaceChildren(tickets.length ? h('div', {class: 'table-wrap'},
        h('table', {class: 'table'}, h('caption', {class: 'sr'}, 'Enrollment tickets for this node'),
          h('thead', null, h('tr', null, ['Ticket', 'Expires', 'Uses', 'State', 'Actions'].map((label) => h('th', {scope: 'col'}, label)))),
          h('tbody', null, tickets.map((ticket) => h('tr', null,
            h('td', {class: 'mono', title: ticket.id}, shortID(ticket.id)),
            h('td', null, when(ticket.expires_at)), h('td', null, `${ticket.uses} / ${ticket.max_uses}`),
            h('td', null, enrollmentState(ticket), ticket.revoked_at ? h('div', {class: 'muted'}, when(ticket.revoked_at)) : null),
            h('td', {class: 'actions'}, ticket.revoked_at ? null : button('Revoke', {small: true, variant: 'danger', onClick: async (event) => {
              const control = event.currentTarget;
              if (!(await confirmDialog({title: 'Revoke enrollment ticket?', message: 'This immediately blocks claims and retries using this ticket. The enrolled agent credential is kept.', confirm: 'Revoke ticket', danger: true}))) return;
              busy(control, true);
              try {
                await api(`/enrollments/${encodeURIComponent(ticket.id)}`, {method: 'DELETE'});
                await reload();
              } catch (failure) { toastError(failure); }
              finally { busy(control, false); }
            }}))))))) : emptyState('No enrollment tickets', 'Issue a ticket to enroll an agent for this node.'));
    } catch (failure) {
      if (failure.name !== 'AbortError') { error.textContent = failure.message; error.hidden = false; }
    } finally { loading = false; busy(retry, false); }
  };
  const retry = button('Refresh tickets', {small: true, icon: 'refresh', onClick: reload});
  const issue = button('Issue enrollment ticket', {variant: 'primary', onClick: async () => {
    busy(issue, true);
    try { await issueEnrollment({node_id: node.id, mode: 'single'}, `Enroll ${node.name}`); await reload(); }
    catch (failure) { toastError(failure); }
    finally { busy(issue, false); }
  }});
  const content = panel('Enroll agent', {action: issue}, h('div', {class: 'stack'},
    h('p', {class: 'lede'}, 'Issue a single-node ticket, valid for 15 minutes and one use. The token is shown only in the creation dialog.'),
    node.status === 'disabled' ? callout('This node is disabled. Enable it before enrolling its agent.') : null,
    retry, error, list));
  reload();
  return content;
}
