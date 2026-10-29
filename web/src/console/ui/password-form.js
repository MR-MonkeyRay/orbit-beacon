import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h} from '../../shared/dom.js';
import {api, getNavigationSignal, invalidateSession} from '../api.js';
import {button} from './components.js';
import {field, input, onSubmit, passwordInput, trackFormDirty, validateBytes} from './forms.js';

export function passwordChangeForm({disabled = false, ids = {}, hint, errorBox, extraAction, wrapActions = false} = {}) {
  const oldPassword = passwordInput({name: 'old_password', autocomplete: 'current-password', required: true, disabled,
    id: ids.old, dataset: ids.autofocus ? {autofocus: ''} : undefined});
  const newPassword = passwordInput({name: 'new_password', autocomplete: 'new-password', required: true, disabled, id: ids.new});
  const confirmPassword = input({type: 'password', name: 'confirm_password', autocomplete: 'new-password', required: true,
    disabled, id: ids.confirm});
  const controls = [oldPassword, newPassword, confirmPassword];
  validateBytes(oldPassword, 1, 1024, t('account.currentPassword'));
  validateBytes(newPassword, 8, 1024, t('account.newPassword'));
  const checkMatch = () => confirmPassword.setCustomValidity(confirmPassword.value && confirmPassword.value !== newPassword.value
    ? t('account.passwordMismatch') : '');
  confirmPassword.addEventListener('input', checkMatch);
  newPassword.addEventListener('input', checkMatch);

  const clear = () => {
    for (const control of controls) {
      control.value = '';
      if (errorBox) control.classList.remove('touched');
    }
  };
  const navigation = getNavigationSignal();
  navigation?.addEventListener('abort', clear, {once: true});
  window.addEventListener('pagehide', clear, {once: true, signal: navigation || undefined});

  const submit = button(t('account.changeAndSignOut'), {type: 'submit', variant: 'primary', disabled, id: ids.submit});
  const secondary = extraAction?.(clear);
  const actions = wrapActions ? h('div', {class: 'form-actions'}, submit, secondary) : [submit, secondary];
  const form = h('form', {class: 'form'}, errorBox,
    field(t('account.currentPassword'), oldPassword, {inner: oldPassword.wrapper}),
    field(t('account.newPassword'), newPassword, {inner: newPassword.wrapper, hint}),
    field(t('account.confirmPassword'), confirmPassword), actions);
  trackFormDirty(form);
  onSubmit(form, submit, async () => {
    if (errorBox) for (const control of controls) control.showError('');
    if (secondary) secondary.disabled = true;
    try {
      await api('/me/password', {method: 'PATCH', body: {old_password: oldPassword.value, new_password: newPassword.value}});
      clear();
      form.markClean();
      invalidateSession('password');
    } catch (error) {
      if (errorBox && error.name === 'AbortError') return;
      const current = errorBox
        ? error.code === 'invalid_old_password' || error.code === 'rate_limited'
        : (error.status === 400 && error.code === 'invalid_old_password') || (error.status === 429 && error.code === 'rate_limited');
      const unchanged = errorBox ? error.code === 'password_unchanged' : error.status === 400 && error.code === 'password_unchanged';
      if (current || unchanged) {
        const control = current ? oldPassword : newPassword;
        control.showError(errorText(error));
        control.focus();
        if (!errorBox) return;
      }
      if (errorBox) error.fields = undefined;
      throw error;
    } finally {
      clear();
      form.syncDirty();
      if (secondary) secondary.disabled = false;
    }
  }, errorBox);
  return {form, clear};
}
