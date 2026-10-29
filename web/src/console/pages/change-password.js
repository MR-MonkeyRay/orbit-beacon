import {h} from '../../shared/dom.js';
import {api, getNavigationSignal, invalidateSession, signOut} from '../api.js';
import {authLayout} from '../ui/auth-layout.js';
import {busy, button} from '../ui/components.js';
import {toastError} from '../ui/feedback.js';
import {field, input, onSubmit, passwordInput, validateBytes} from '../ui/forms.js';

export async function changePasswordPage() {
  const oldPassword = passwordInput({name: 'old_password', autocomplete: 'current-password', required: true, dataset: {autofocus: ''}});
  const newPassword = passwordInput({name: 'new_password', autocomplete: 'new-password', required: true});
  const confirmPassword = input({type: 'password', name: 'confirm_password', autocomplete: 'new-password', required: true});
  validateBytes(oldPassword, 1, 1024, 'Current password');
  validateBytes(newPassword, 8, 1024, 'New password');
  const checkMatch = () => confirmPassword.setCustomValidity(confirmPassword.value && confirmPassword.value !== newPassword.value ? 'Passwords do not match.' : '');
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
  const submit = button('Change password and sign out', {type: 'submit', variant: 'primary'});
  const exit = button('Sign out', {variant: 'ghost', onClick: async () => {
    clear();
    busy(exit, true);
    try { await signOut(); } catch (error) { toastError(error); } finally { busy(exit, false); }
  }});
  const form = h('form', {class: 'form'}, errorBox,
    field('Current password', oldPassword, {inner: oldPassword.wrapper}),
    field('New password', newPassword, {inner: newPassword.wrapper, hint: '8–1024 UTF-8 bytes. Choose a different password.'}),
    field('Confirm new password', confirmPassword), submit, exit);
  onSubmit(form, submit, async () => {
    exit.disabled = true;
    for (const control of [oldPassword, newPassword, confirmPassword]) control.showError('');
    try {
      await api('/me/password', {method: 'PATCH', body: {old_password: oldPassword.value, new_password: newPassword.value}});
      clear();
      invalidateSession('password');
    } catch (error) {
      if (error.name === 'AbortError') return;
      const current = error.code === 'invalid_old_password' || error.code === 'rate_limited';
      const message = error.code === 'invalid_old_password' ? 'The current password is incorrect.'
        : error.code === 'password_unchanged' ? 'Choose a password different from your current password.'
          : error.code === 'rate_limited' ? 'Too many password change attempts. Wait a minute and try again.'
            : 'The password could not be changed. Check your connection and try again.';
      if (current || error.code === 'password_unchanged') {
        const control = current ? oldPassword : newPassword;
        control.showError(message);
        control.focus();
      }
      error.message = message;
      error.fields = undefined;
      throw error;
    } finally { clear(); exit.disabled = false; }
  }, errorBox);
  return authLayout({
    title: 'Change your password',
    description: 'You must change your initial password before accessing the console. Changing it signs out every session.',
    content: form
  });
}
