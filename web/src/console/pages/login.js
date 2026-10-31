import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h, icon} from '../../shared/dom.js';
import {api, getNavigationSignal, revalidateSession, session, setSession} from '../api.js';
import {go, ignoreNavigationCancellation} from '../router.js';
import {authLayout} from '../ui/auth-layout.js';
import {busy, button, callout} from '../ui/components.js';
import {field, input, passwordInput, trackFormDirty, validateBytes} from '../ui/forms.js';


function nextPath() {
  const next = new URLSearchParams(location.search).get('next');
  if (!next || !next.startsWith('/') || next.startsWith('//') || next.includes('\\')) return '/overview';
  let target;
  try { target = new URL(next, location.origin); } catch { return '/overview'; }
  const normalizedPath = target.pathname.replace(/\/$/, '') || '/';
  if (target.origin !== location.origin || ['/login', '/change-password'].includes(normalizedPath)) return '/overview';
  return `${target.pathname}${target.search}${target.hash}`;
}

export async function loginPage() {
  try {
    await revalidateSession();
    return session.user.must_change_password ? '/change-password' : nextPath();
  } catch { /* 未登录 */ }
  const errorBox = h('div', {class: 'form-error', role: 'alert', hidden: true});
  const username = input({name: 'username', autocomplete: 'username', required: true, autocapitalize: 'none', dataset: {autofocus: ''}});
  const password = passwordInput({name: 'password', autocomplete: 'current-password', required: true});
  validateBytes(username, 1, 128, t('account.username'));
  validateBytes(password, 8, 1024, t('account.password'));
  const submit = button(t('shell.signIn'), {type: 'submit', variant: 'primary'});
  const form = h('form', {class: 'form'}, errorBox, field(t('account.username'), username), field(t('account.password'), password, {inner: password.wrapper}), submit);
  trackFormDirty(form);
  getNavigationSignal()?.addEventListener('abort', () => { password.value = ''; }, {once: true});
  window.addEventListener('pagehide', () => { password.value = ''; }, {once: true, signal: getNavigationSignal() || undefined});
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    if (submit.classList.contains('busy')) return;
    errorBox.hidden = true;
    busy(submit, true);
    try {
      const value = await api('/session', {method: 'POST', body: {username: username.value, password: password.value}, noRedirect: true});
      password.value = '';
      form.markClean();
      setSession(value);
      void go(session.user.must_change_password ? '/change-password' : nextPath(), {replace: true}).catch(ignoreNavigationCancellation);
    } catch (error) {
      if (!form.isConnected || error.name === 'AbortError') return;
      const message = error.status === 401 ? t('shell.invalidCredentials')
        : error.status === 429 ? t('shell.signInRateLimited') : errorText(error);
      errorBox.replaceChildren(icon('alert'), h('span', null, message));
      errorBox.hidden = false;
      password.classList.remove('touched');
      password.focus();
    } finally {
      password.value = '';
      form.syncDirty();
      busy(submit, false);
    }
  });
  const changed = new URLSearchParams(location.search).has('password-changed');
  return authLayout({
    title: t('shell.signInTitle'),
    description: t('shell.signInDescription'),
    content: [changed ? callout(t('shell.passwordChanged'), 'info') : null, form],
    footer: h('a', {href: '/'}, t('shell.viewPublic'))
  });
}
