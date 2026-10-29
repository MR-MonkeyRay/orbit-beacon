import {h, icon, svg} from '../../shared/dom.js';
import {api, revalidateSession, setSession} from '../api.js';
import {go, ignoreNavigationCancellation} from '../router.js';
import {brandMark} from '../shell.js';
import {busy, button, callout} from '../ui/components.js';
import {field, input, passwordInput, validateBytes} from '../ui/forms.js';

// Rings are circles in a squashed, tilted group so satellites travel along
// ellipses. Satellites are pre-stretched and counter-rotate so they stay round.
function orbits() {
  const element = svg('svg', {class: 'login-orbits', viewBox: '0 0 1100 1100', 'aria-hidden': 'true'});
  element.innerHTML = '<g transform="rotate(-16 550 550) translate(0 320) scale(1 .42)">'
    + '<g class="ring r1"><circle class="path" cx="550" cy="550" r="300"/><ellipse class="sat" cx="850" cy="550" rx="7" ry="16.7"/><ellipse class="sat" cx="250" cy="550" rx="5" ry="11.9"/></g>'
    + '<g class="ring r2"><circle class="path" cx="550" cy="550" r="420"/><ellipse class="sat" cx="550" cy="130" rx="8" ry="19.0"/></g>'
    + '<g class="ring r3"><circle class="path" cx="550" cy="550" r="540"/><ellipse class="sat" cx="10" cy="550" rx="6" ry="14.3"/><ellipse class="sat" cx="932" cy="932" rx="5" ry="11.9"/></g></g>';
  return element;
}

function nextPath() {
  const next = new URLSearchParams(location.search).get('next');
  if (!next || !next.startsWith('/') || next.startsWith('//') || next.includes('\\')) return '/overview';
  let target;
  try { target = new URL(next, location.origin); } catch { return '/overview'; }
  const normalizedPath = target.pathname.replace(/\/$/, '') || '/';
  if (target.origin !== location.origin || normalizedPath === '/login') return '/overview';
  return `${target.pathname}${target.search}${target.hash}`;
}

export async function loginPage() {
  try {
    await revalidateSession();
    return nextPath();
  } catch { /* not signed in */ }
  const errorBox = h('div', {class: 'form-error', role: 'alert', hidden: true});
  const username = input({name: 'username', autocomplete: 'username', required: true, autocapitalize: 'none', dataset: {autofocus: ''}});
  const password = passwordInput({name: 'password', autocomplete: 'current-password', required: true});
  validateBytes(username, 1, 128, 'Username');
  validateBytes(password, 8, 1024, 'Password');
  const submit = button('Sign in', {type: 'submit', variant: 'primary'});
  const form = h('form', {class: 'form'}, errorBox, field('Username', username), field('Password', password, {inner: password.wrapper}), submit);
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    if (submit.classList.contains('busy')) return;
    errorBox.hidden = true;
    busy(submit, true);
    try {
      setSession(await api('/session', {method: 'POST', body: {username: username.value, password: password.value}, noRedirect: true}));
      void go(nextPath(), {replace: true}).catch(ignoreNavigationCancellation);
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
  return h('section', {class: 'login'}, orbits(),
    h('div', null,
      h('div', {class: 'login-card'},
        h('div', {class: 'brand'}, brandMark(), h('span', null, 'OrbitBeacon')),
        h('h1', {tabindex: '-1'}, 'Sign in to the console'),
        h('p', null, 'Manage nodes, access and the public status page for this server.'),
        changed ? callout('Password changed. Every session was signed out; sign in with your new password.', 'info') : null,
        form),
      h('p', {class: 'login-foot'}, h('a', {href: '/'}, 'View the public status page'))));
}
