import {t} from '../../shared/i18n.js';
import {errorText} from '../../shared/errors.js';
import {h} from '../../shared/dom.js';
import {api, getNavigationSignal, invalidateSession, signOut} from '../api.js';
import {authLayout} from '../ui/auth-layout.js';
import {busy, button} from '../ui/components.js';
import {toastError} from '../ui/feedback.js';
import {field, input, onSubmit, passwordInput, trackFormDirty, validateBytes} from '../ui/forms.js';

export async function changePasswordPage() {
  const oldPassword = passwordInput({name: 'old_password', autocomplete: 'current-password', required: true, dataset: {autofocus: ''}});
  const newPassword = passwordInput({name: 'new_password', autocomplete: 'new-password', required: true});
  const confirmPassword = input({type: 'password', name: 'confirm_password', autocomplete: 'new-password', required: true});
  validateBytes(oldPassword, 1, 1024, t('shell.currentPassword'));
  validateBytes(newPassword, 8, 1024, t('shell.newPassword'));
  const checkMatch = () => confirmPassword.setCustomValidity(confirmPassword.value && confirmPassword.value !== newPassword.value ? t('shell.passwordMismatch') : '');
  confirmPassword.addEventListener('input', checkMatch);
  newPassword.addEventListener('input', checkMatch);
  const clear = () => {
    for (const control of [oldPassword, newPassword, confirmPassword]) {
      control.value = '';
      control.classList.remove('touched');
    }
  };
  getNavigationSignal()?.addEventListener('abort', clear, {once: true});
  window.addEventListener('pagehide', clear, {once: true, signal: getNavigationSignal() || undefined});
  const errorBox = h('div', {class: 'form-error', role: 'alert', hidden: true});
  const submit = button(t('shell.changeAndSignOut'), {type: 'submit', variant: 'primary'});
  const exit = button(t('shell.signOut'), {variant: 'ghost', onClick: async () => {
    clear();
    form.markClean();
    busy(exit, true);
    try { await signOut(); } catch (error) { toastError(error); } finally { busy(exit, false); }
  }});
  const form = h('form', {class: 'form'}, errorBox,
    field(t('shell.currentPassword'), oldPassword, {inner: oldPassword.wrapper}),
    field(t('shell.newPassword'), newPassword, {inner: newPassword.wrapper, hint: t('shell.passwordDifferentHint')}),
    field(t('shell.confirmPassword'), confirmPassword), submit, exit);
  trackFormDirty(form);
  onSubmit(form, submit, async () => {
    exit.disabled = true;
    for (const control of [oldPassword, newPassword, confirmPassword]) control.showError('');
    try {
      await api('/me/password', {method: 'PATCH', body: {old_password: oldPassword.value, new_password: newPassword.value}});
      clear();
      form.markClean();
      invalidateSession('password');
    } catch (error) {
      if (error.name === 'AbortError') return;
      const current = error.code === 'invalid_old_password' || error.code === 'rate_limited';
      const message = errorText(error);
      if (current || error.code === 'password_unchanged') {
        const control = current ? oldPassword : newPassword;
        control.showError(message);
        control.focus();
      }
      error.fields = undefined;
      throw error;
    } finally { clear(); form.syncDirty(); exit.disabled = false; }
  }, errorBox);
  return authLayout({
    title: t('shell.changePasswordTitle'),
    description: t('shell.changePasswordDescription'),
    content: form
  });
}
