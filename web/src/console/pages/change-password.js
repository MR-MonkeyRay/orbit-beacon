import {t} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {signOut} from '../api.js';
import {authLayout} from '../ui/auth-layout.js';
import {busy, button} from '../ui/components.js';
import {toastError} from '../ui/feedback.js';
import {passwordChangeForm} from '../ui/password-form.js';

export async function changePasswordPage() {
  const errorBox = h('div', {class: 'form-error', role: 'alert', hidden: true});
  let exit;
  const {form} = passwordChangeForm({hint: t('account.passwordDifferentHint'), errorBox, ids: {autofocus: true},
    extraAction: (clear) => {
      exit = button(t('common.signOut'), {variant: 'ghost', onClick: async () => {
        clear();
        form.markClean();
        busy(exit, true);
        try { await signOut(); } catch (error) { toastError(error); } finally { busy(exit, false); }
      }});
      return exit;
    }});
  return authLayout({
    title: t('account.changePasswordTitle'),
    description: t('account.changePasswordDescription'),
    content: form
  });
}
