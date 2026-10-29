import {h, icon} from '../../shared/dom.js';
import {api, getNavigationSignal, revalidateSession, session, setSession} from '../api.js';
import {go, ignoreNavigationCancellation} from '../router.js';
import {authLayout} from '../ui/auth-layout.js';
import {busy, button, callout} from '../ui/components.js';
import {field, input, passwordInput, validateBytes} from '../ui/forms.js';


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
  } catch { /* not signed in */ }
  const errorBox = h('div', {class: 'form-error', role: 'alert', hidden: true});
  const username = input({name: 'username', autocomplete: 'username', required: true, autocapitalize: 'none', dataset: {autofocus: ''}});
  const password = passwordInput({name: 'password', autocomplete: 'current-password', required: true});
  validateBytes(username, 1, 128, 'Username');
  validateBytes(password, 8, 1024, 'Password');
  const submit = button('Sign in', {type: 'submit', variant: 'primary'});
  const form = h('form', {class: 'form'}, errorBox, field('Username', username), field('Password', password, {inner: password.wrapper}), submit);
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
      setSession(value);
      void go(session.user.must_change_password ? '/change-password' : nextPath(), {replace: true}).catch(ignoreNavigationCancellation);
    } catch (error) {
      if (!form.isConnected || error.name === 'AbortError') return;
      const message = error.status === 401 ? 'Username or password is incorrect.'
        : error.status === 429 ? 'Too many sign-in attempts. Wait a moment and try again.' : error.message;
      errorBox.replaceChildren(icon('alert'), h('span', null, message));
      errorBox.hidden = false;
      password.classList.remove('touched');
      password.focus();
    } finally {
      password.value = '';
      busy(submit, false);
    }
  });
  const changed = new URLSearchParams(location.search).has('password-changed');
  return authLayout({
    title: 'Sign in to the console',
    description: 'Manage nodes, access and the public status page for this server.',
    content: [changed ? callout('Password changed. Every session was signed out; sign in with your new password.', 'info') : null, form],
    footer: h('a', {href: '/'}, 'View the public status page')
  });
}
