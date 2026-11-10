import {t, tParts} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h} from '../../shared/dom.js';
import {copyElementText} from '../../shared/clipboard.js';
import {api, getNavigationSignal, getSessionEpoch, getSessionSignal} from '../api.js';
import {shortID, when} from '../format.js';
import {busy, button, callout, dataTable, emptyState, panel} from '../ui/components.js';
import {confirmDialog, modal} from '../ui/dialogs.js';
import {toastError} from '../ui/feedback.js';
import {enrollmentCountdown, installerCommand, enrollmentState, nodeEnrollments} from '../lib/enrollment-utils.js';

async function copyText(element, status) {
  const result = await copyElementText(element);
  status.textContent = t(result === 'clipboard' ? 'nodes.enrollment.copied' : result === 'fallback' ? 'nodes.enrollment.copiedFallback' : 'nodes.enrollment.copyManual');
}

export async function issueEnrollment(target, title, isDeleted = () => false) {
  const navigation = getNavigationSignal();
  const epoch = getSessionEpoch();
  const sessionSignal = getSessionSignal();
  const active = () => !isDeleted() && !navigation?.aborted && navigation === getNavigationSignal() && epoch === getSessionEpoch();
  if (!active()) return;
  let ticket;
  try { ticket = await api('/enrollments', {method: 'POST', body: target}); }
  catch (error) {
    if (!active()) return;
    throw error;
  }
  if (!active()) { delete ticket.token; return; }
  const token = h('code', null, ticket.token);
  const installer = h('code', null, installerCommand(location.origin, ticket.server_version, ticket.token));
  // 明文只保留在此对话框中，绝不进入票据列表状态。
  delete ticket.token;
  const countdown = h('span', {class: 'mono', role: 'timer', 'aria-live': 'off'});
  const status = h('p', {class: 'text-note', role: 'status'});
  const expiry = h('p', {class: 'text-note', role: 'status', hidden: true}, t('nodes.enrollment.expiredReissue'));
  const copyToken = button(t('nodes.enrollment.copyToken'), {icon: 'copy', onClick: () => { if (!copyToken.disabled) void copyText(token, status); }});
  const copyInstall = button(t('nodes.enrollment.copyInstall'), {icon: 'copy', onClick: () => { if (!copyInstall.disabled) void copyText(installer, status); }});
  const tick = () => {
    countdown.textContent = enrollmentCountdown(ticket.expires_at);
    const expired = !(Date.parse(ticket.expires_at) > Date.now());
    copyToken.disabled = expired;
    copyInstall.disabled = expired || !installer.textContent;
    expiry.hidden = !expired;
  };
  tick();
  const dialog = modal(title, (close) => h('div', null,
    h('div', {class: 'dialog-body stack'},
      h('div', null, tParts('nodes.enrollment.expiry', {countdown, time: when(ticket.expires_at)})),
      h('p', {class: 'text-note'}, t('nodes.enrollment.issueIntro')),
      h('pre', {class: 'code enrollment-token'}, token), copyToken,
      h('p', {class: 'text-note'}, t('nodes.enrollment.secretWarning')),
      h('pre', {class: 'code', tabindex: '0'}, installer), copyInstall,
      !installer.textContent ? callout(t('nodes.enrollment.unreleased')) : null,
      expiry, status),
    h('div', {class: 'dialog-foot'}, button(t('nodes.close'), {onClick: () => close('done')}))));
  const cancel = () => dialog.close('cancel');
  navigation?.addEventListener('abort', cancel, {once: true});
  sessionSignal.addEventListener('abort', cancel, {once: true});
  const timer = setInterval(tick, 1000);
  try { await dialog.closed; }
  finally {
    clearInterval(timer);
    navigation?.removeEventListener('abort', cancel);
    sessionSignal.removeEventListener('abort', cancel);
    token.textContent = '';
    installer.textContent = '';
  }
}

export function enrollmentTab(node) {

  const list = h('div', {'aria-live': 'polite'});
  const error = h('div', {class: 'callout', role: 'alert', hidden: true});
  let loading = false;
  const reload = async () => {
    if (loading || node.deleted) return;
    loading = true;
    busy(retry, true);
    error.hidden = true;
    try {
      const tickets = await nodeEnrollments(node.id, api);
      if (node.deleted) return;
      list.replaceChildren(tickets.length ? dataTable({caption: t('nodes.enrollment.caption'),
        columns: [t('nodes.enrollment.ticket'), t('nodes.enrollment.expiresHeader'), t('nodes.enrollment.state'), t('nodes.enrollment.actions')],
        body: h('tbody', null, tickets.map((ticket) => h('tr', null,
            h('td', {class: 'mono', title: ticket.id}, shortID(ticket.id)),
            h('td', null, when(ticket.expires_at)),
            h('td', null, enrollmentState(ticket), ticket.revoked_at ? h('div', {class: 'muted'}, when(ticket.revoked_at)) : null),
            h('td', {class: 'actions'}, ticket.revoked_at ? null : button(t('nodes.enrollment.revoke'), {small: true, variant: 'danger', onClick: async (event) => {
              const control = event.currentTarget;
              if (node.deleted) return;
              if (!(await confirmDialog({title: t('nodes.enrollment.revokeTitle'), message: t('nodes.enrollment.revokeIntro'), confirm: t('nodes.enrollment.revokeConfirm'), danger: true})) || node.deleted) return;
              busy(control, true);
              try {
                await api(`/enrollments/${encodeURIComponent(ticket.id)}`, {method: 'DELETE'});
                if (!node.deleted) await reload();
              } catch (failure) { toastError(failure); }
              finally { busy(control, false); control.disabled = Boolean(node.deleted); }
            }})))))}) : emptyState(t('nodes.enrollment.empty'), null));
    } catch (failure) {
      if (failure.name !== 'AbortError') { error.textContent = errorText(failure); error.hidden = false; }
    } finally { loading = false; busy(retry, false); retry.disabled = Boolean(node.deleted); }
  };
  const retry = button(t('nodes.enrollment.refresh'), {small: true, icon: 'refresh', onClick: reload});
  const issue = button(t('nodes.enrollment.issue'), {variant: 'primary', onClick: async () => {
    if (node.deleted) return;
    const navigation = getNavigationSignal();
    const epoch = getSessionEpoch();
    busy(issue, true);
    try { await issueEnrollment({node_id: node.id}, t('nodes.enrollment.title', {name: node.name}), () => node.deleted); if (!node.deleted) await reload(); }
    catch (failure) { toastError(failure); }
    finally {
      busy(issue, false); issue.disabled = Boolean(node.deleted);
      if (issue.isConnected && !issue.disabled && !navigation?.aborted && navigation === getNavigationSignal() && epoch === getSessionEpoch()) issue.focus({preventScroll: true});
    }
  }});
  const content = panel(null, {action: issue}, h('div', {class: 'stack'},
    h('p', {class: 'text-note'}, t('nodes.enrollment.issueIntro')),
    node.status === 'disabled' ? callout(t('nodes.enrollment.disabled')) : null,
    retry, error, list));
  reload();
  return content;
}
