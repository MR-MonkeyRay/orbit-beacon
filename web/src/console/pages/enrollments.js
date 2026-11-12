import {t} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {copyElementText} from '../../shared/clipboard.js';
import {api, getNavigationSignal, getSessionEpoch, getSessionSignal} from '../api.js';
import {connectConsoleLive} from '../live.js';
import {button, callout} from '../ui/components.js';
import {modal} from '../ui/dialogs.js';
import {enrollmentCountdown, installerCommand} from '../lib/enrollment-utils.js';

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
  const installer = h('code', null, installerCommand(location.origin, ticket.server_version));
  // 明文只保留在此对话框中。
  delete ticket.token;
  const countdown = h('span', {class: 'mono', role: 'timer', 'aria-live': 'off'});
  const status = h('p', {class: 'text-note', role: 'status'});
  const expiry = h('p', {class: 'text-note', role: 'status', hidden: true}, t('nodes.enrollment.expiredReissue'));
  const copyToken = button(t('nodes.enrollment.copyToken'), {icon: 'copy', onClick: () => { if (!copyToken.disabled) void copyText(token, status); }});
  const copyInstall = button(t('nodes.enrollment.copyInstall'), {variant: 'primary', icon: 'copy', onClick: () => { if (!copyInstall.disabled) void copyText(installer, status); }});
  (installer.textContent ? copyInstall : copyToken).dataset.autofocus = '';
  const includeKey = h('input', {type: 'checkbox', role: 'switch', dataset: {transient: ''}, onchange: () => {
    installer.textContent = installerCommand(location.origin, ticket.server_version, includeKey.checked ? token.textContent : undefined);
    status.textContent = '';
  }});
  const toggle = h('label', {class: 'switch'}, includeKey, h('span', {class: 'switch-track', 'aria-hidden': 'true'}),
    h('span', {class: 'switch-text'}, t('nodes.enrollment.includeKey')));
  const tick = () => {
    countdown.textContent = enrollmentCountdown(ticket.expires_at);
    const expired = !(Date.parse(ticket.expires_at) > Date.now());
    copyToken.disabled = expired;
    includeKey.disabled = expired;
    copyInstall.disabled = expired || !installer.textContent;
    expiry.hidden = !expired;
  };
  tick();
  const waiting = target.node_id ? h('div', {class: 'enrollment-progress', role: 'status'},
    h('span', {class: 'enrollment-spinner', 'aria-hidden': 'true'}), t('nodes.enrollment.waiting')) : null;
  const waitStep = waiting ? h('li', null, h('h3', null, t('nodes.enrollment.waitStep')), waiting) : null;
  const keyRow = h('div', {class: 'enrollment-key stack'},
    h('div', {class: 'enrollment-key-head'}, h('strong', null, t('nodes.enrollment.key')), countdown),
    h('pre', {class: 'code enrollment-token', tabindex: '0'}, token), copyToken);
  const dialog = modal(title, (close) => h('div', null,
    h('div', {class: 'dialog-body stack'},
      h('ol', {class: 'enrollment-steps'},
        h('li', null, h('h3', null, t('nodes.enrollment.copyStep')),
          ...(installer.textContent
            ? [h('pre', {class: 'code enrollment-command', tabindex: '0'}, installer), h('div', {class: 'enrollment-actions'}, toggle, copyInstall)]
            : [callout(t('nodes.enrollment.unreleased'))])),
        h('li', null, h('h3', null, t('nodes.enrollment.runStep')), keyRow),
        waitStep),
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
      // 密钥已用掉，不再显示。
      keyRow.hidden = true;
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
