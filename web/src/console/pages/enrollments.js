import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h} from '../../shared/dom.js';
import {copyElementText} from '../../shared/clipboard.js';
import {api, getNavigationSignal, getSessionEpoch, getSessionSignal} from '../api.js';
import {connectConsoleLive} from '../live.js';
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
  const copyInstall = button(t('nodes.enrollment.copyInstall'), {variant: 'primary', icon: 'copy', onClick: () => { if (!copyInstall.disabled) void copyText(installer, status); }});
  (installer.textContent ? copyInstall : copyToken).dataset.autofocus = '';
  const tick = () => {
    countdown.textContent = enrollmentCountdown(ticket.expires_at);
    const expired = !(Date.parse(ticket.expires_at) > Date.now());
    copyToken.disabled = expired;
    copyInstall.disabled = expired || !installer.textContent;
    expiry.hidden = !expired;
  };
  tick();
  const waiting = target.node_id ? h('div', {class: 'enrollment-progress', role: 'status'},
    h('span', {class: 'enrollment-spinner', 'aria-hidden': 'true'}), t('nodes.enrollment.waiting')) : null;
  const waitStep = waiting ? h('li', null, h('h3', null, t('nodes.enrollment.waitStep')), waiting) : null;
  const ticketNotes = h('div', null, h('div', {class: 'text-note'}, countdown), h('p', {class: 'text-note'}, t('nodes.enrollment.issueIntro')));
  const dialog = modal(title, (close) => h('div', null,
    h('div', {class: 'dialog-body stack'},
      h('ol', {class: 'enrollment-steps'},
        h('li', null, h('h3', null, t('nodes.enrollment.copyStep')),
          ...(installer.textContent
            ? [h('pre', {class: 'code', tabindex: '0'}, installer),
              h('p', {class: 'text-note'}, t('nodes.enrollment.secretWarning')), copyInstall]
            : [callout(t('nodes.enrollment.unreleased'))])),
        h('li', null, h('h3', null, t('nodes.enrollment.runStep'))),
        waitStep),
      ticketNotes,
      h('details', {class: 'enrollment-manual', open: !installer.textContent},
        h('summary', null, t('nodes.enrollment.manual')),
        h('pre', {class: 'code enrollment-token'}, token), copyToken),
      expiry, status),
    h('div', {class: 'dialog-foot'}, button(t('nodes.close'), {onClick: () => close('done')}))));
  let reported = false;
  let baseline;
  const live = target.node_id ? connectConsoleLive({nodeID: target.node_id, signal: dialog.signal,
    onSnapshot({node}) {
      if (reported || !active() || dialog.signal.aborted || !node) return;
      // 已注册过的节点重新签发时旧 Agent 仍在上报，无法区分新安装，不显示等待步骤。
      baseline ??= node.presence;
      if (!['pending', 'never'].includes(baseline)) { waitStep.remove(); queueMicrotask(() => live?.close()); return; }
      if (node.presence !== 'online') return;
      reported = true;
      // 票据已用掉，倒计时与使用说明不再有意义。
      ticketNotes.hidden = true;
      const view = h('a', {class: 'btn', href: `/nodes/${encodeURIComponent(target.node_id)}`,
        dataset: {link: ''}, onclick: () => dialog.close('view')}, t('nodes.enrollment.viewNode'));
      waiting.classList.add('enrollment-success');
      waiting.replaceChildren(t('nodes.enrollment.reported'), view);
    }
  }) : null;
  const cancel = () => dialog.close('cancel');
  navigation?.addEventListener('abort', cancel, {once: true});
  sessionSignal.addEventListener('abort', cancel, {once: true});
  const timer = setInterval(tick, 1000);
  try { return await dialog.closed; }
  finally {
    live?.close();
    clearInterval(timer);
    navigation?.removeEventListener('abort', cancel);
    sessionSignal.removeEventListener('abort', cancel);
    token.textContent = '';
    installer.textContent = '';
  }
}

export function enrollmentTab(node, {listOnly = false} = {}) {

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
        body: h('tbody', null, tickets.map((ticket) => {
          const active = !ticket.revoked_at && ticket.uses === 0 && Date.parse(ticket.expires_at) > Date.now();
          return h('tr', null,
            h('td', {class: 'mono', title: ticket.id}, shortID(ticket.id)),
            h('td', null, active ? when(ticket.expires_at) : '—'),
            h('td', null, enrollmentState(ticket), ticket.revoked_at ? h('div', {class: 'muted'}, when(ticket.revoked_at)) : null),
            h('td', {class: 'actions'}, !active ? null : button(t('nodes.enrollment.revoke'), {small: true, variant: 'danger', onClick: async (event) => {
              const control = event.currentTarget;
              if (node.deleted) return;
              if (!(await confirmDialog({title: t('nodes.enrollment.revokeTitle'), message: t('nodes.enrollment.revokeIntro'), confirm: t('nodes.enrollment.revokeConfirm'), danger: true})) || node.deleted) return;
              busy(control, true);
              try {
                await api(`/enrollments/${encodeURIComponent(ticket.id)}`, {method: 'DELETE'});
                if (!node.deleted) await reload();
              } catch (failure) { toastError(failure); }
              finally { busy(control, false); control.disabled = Boolean(node.deleted); }
            }})));
        }))}) : emptyState(t('nodes.enrollment.empty'), null));
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
  const content = panel(null, {action: listOnly ? null : issue}, h('div', {class: 'stack'},
    listOnly ? null : h('p', {class: 'text-note'}, t('nodes.enrollment.issueIntro')),
    node.status === 'disabled' ? callout(t('nodes.enrollment.disabled')) : null,
    retry, error, list));
  content.reload = reload;
  reload();
  return content;
}
