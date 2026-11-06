import {h} from '../../shared/dom.js';
import {copyElementText} from '../../shared/clipboard.js';
import {t} from '../i18n.js';
import {api, getNavigationSignal, updateUser} from '../api.js';
import {button, facts, panel} from './components.js';
import {formDialog, modal} from './dialogs.js';
import {field, input, passwordInput, validateBytes} from './forms.js';
import {toastError} from './feedback.js';

export function totpQR({size, modules}) {
  if (!Number.isInteger(size) || size < 21 || size > 177 || !/^[01]+$/.test(modules) || modules.length !== size * size) throw new Error('Invalid QR');
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', `0 0 ${size + 8} ${size + 8}`);
  svg.setAttribute('width', '256'); svg.setAttribute('height', '256');
  svg.setAttribute('class', 'totp-qr'); svg.setAttribute('role', 'img'); svg.setAttribute('aria-label', t('account.totpQR'));
  svg.setAttribute('shape-rendering', 'crispEdges');
  const background = document.createElementNS(ns, 'rect');
  background.setAttribute('width', String(size + 8)); background.setAttribute('height', String(size + 8)); background.setAttribute('fill', '#fff');
  const path = document.createElementNS(ns, 'path');
  let d = '';
  for (let i = 0; i < modules.length; i++) if (modules[i] === '1') d += `M${i % size + 4} ${Math.floor(i / size) + 4}h1v1h-1z`;
  path.setAttribute('d', d); path.setAttribute('fill', '#000');
  svg.append(background, path);
  return svg;
}

function recoveryDialog(codes) {
  const value = h('pre', {class: 'recovery-secret', tabindex: '0'}, codes.join('\n'));
  const {closed} = modal(t('account.recoveryCodes'), (close) => h('div', {class: 'dialog-body'}, value,
    h('div', {class: 'dialog-foot'}, button(t('account.copyRecoveryCodes'), {onClick: () => copyElementText(value)}),
      button(t('account.recoverySaved'), {onClick: () => close('done')}))));
  void closed.then(() => { value.textContent = ''; codes.fill(''); });
}

export function totpCredentials({title, secondFactor = true, action}) {
  let password;
  let otp;
  const dialog = formDialog({title, submit: title, build(form) {
    password = passwordInput({name: 'password', autocomplete: 'current-password', required: true, dataset: {transient: ''}});
    validateBytes(password, 8, 1024, t('account.password'));
    form.append(field(t('account.currentPassword'), password, {inner: password.wrapper}));
    if (secondFactor) {
      otp = input({name: 'otp', autocomplete: 'one-time-code', required: true, autocapitalize: 'none', dataset: {transient: ''}});
      validateBytes(otp, 1, 64, t('account.otp'));
      form.append(field(t('account.otp'), otp));
    }
  }, action: () => action({password: password.value, ...(otp ? {otp: otp.value} : {})})});
  void dialog.closed.then(() => { password.value = ''; if (otp) otp.value = ''; });
  return dialog;
}

export function totpPanel() {
  const body = h('div', {class: 'stack'});
  const navigation = getNavigationSignal();
  const alive = () => !navigation?.aborted;
  const load = async () => {
    const status = await api('/account/totp');
    if (!alive()) return;
    updateUser({totp_enabled: status.enabled});
    body.replaceChildren(facts([
      [t('account.totp'), t(status.enabled ? 'account.totpEnabled' : 'account.totpDisabled')],
      [t('account.recoveryCodes'), String(status.recovery_codes_remaining)]
    ]));
    const change = (disable) => {
      let codes;
      const credentials = totpCredentials({title: t(disable ? 'account.totpDisable' : 'account.totpRegenerate'), action: async (body) => {
        const result = await api(`/account/totp/${disable ? 'disable' : 'recovery-codes'}`, {method: 'POST', body});
        if (!alive()) return;
        codes = result?.recovery_codes;
        updateUser({totp_enabled: !disable});
        void load().catch(toastError);
      }});
      void credentials.closed.then((result) => { if (result === 'done' && codes && alive()) recoveryDialog(codes); });
    };
    body.append(h('div', {class: 'head-actions'}, status.enabled
      ? [button(t('account.totpDisable'), {onClick: () => change(true)}), button(t('account.totpRegenerate'), {onClick: () => change(false)})]
      : button(t('account.totpEnable'), {onClick: () => enable()})));
  };
  const enable = () => {
    let setup;
    const credentials = totpCredentials({title: t('account.totpEnable'), secondFactor: false, action: async (body) => {
      setup = await api('/account/totp/setup', {method: 'POST', body});
    }});
    void credentials.closed.then((result) => {
      if (result !== 'done' || !setup || !alive()) { setup = null; return; }
      let code;
      let codes;
      let secret;
      const confirm = formDialog({title: t('account.totpEnable'), submit: t('account.totpConfirm'), build(form) {
        secret = h('code', {class: 'recovery-secret', tabindex: '0'}, setup.secret);
        code = input({name: 'code', autocomplete: 'one-time-code', required: true, dataset: {transient: ''}});
        validateBytes(code, 6, 64, t('account.totpCode'));
        form.append(totpQR(setup.qr), secret, field(t('account.totpCode'), code));
      }, action: async () => {
        const result = await api('/account/totp/confirm', {method: 'POST', body: {code: code.value}});
        codes = result.recovery_codes;
        updateUser({totp_enabled: true});
        void load().catch(toastError);
      }});
      setup = null;
      void confirm.closed.then((result) => {
        code.value = ''; secret.textContent = '';
        if (result === 'done' && codes && alive()) recoveryDialog(codes);
      });
    });
  };
  void load().catch(() => { if (alive()) body.replaceChildren(h('div', {class: 'form-error', role: 'alert'}, t('errors.generic'))); });
  return panel(t('account.totp'), {}, body);
}
