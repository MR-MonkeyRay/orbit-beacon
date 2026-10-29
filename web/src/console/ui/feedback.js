import {h, icon} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';

export function toast(message, options = {}) {
  const toasts = document.getElementById('toasts');
  let timer = 0;
  const item = h('div', {class: `toast${options.error ? ' error' : ''}`, role: options.error ? 'alert' : null});
  const dismiss = () => {
    if (!item.isConnected || item.classList.contains('leaving')) return;
    clearTimeout(timer);
    item.classList.add('leaving');
    item.addEventListener('animationend', () => item.remove(), {once: true});
    setTimeout(() => item.remove(), 400);
  };
  item.append(icon(options.error ? 'alert' : 'check'),
    h('p', null, message, options.detail ? h('small', null, options.detail) : null),
    h('button', {type: 'button', 'aria-label': t('controls.notification.dismiss'), onclick: dismiss}, icon('x')));
  toasts.append(item);
  while (toasts.children.length > 4) toasts.firstElementChild.remove();
  timer = setTimeout(dismiss, options.error ? 9000 : 4500);
}

export function toastError(error) {
  // Navigation/session cancellation has no action result to report. A managed
  // unauthenticated response already cleared private UI at the central boundary;
  // its initiating action must not recreate a stale toast on the login page.
  if (error.name === 'AbortError' || error.status === 401 && error.code === 'unauthenticated') return;
  toast(errorText(error), {error: true});
}

// Thin bar at the top of the viewport while a page loads.
export const progress = {
  start() {
    const bar = document.getElementById('progress');
    bar.className = 'progress';
    void bar.offsetWidth;
    bar.classList.add('run');
  },
  done() {
    const bar = document.getElementById('progress');
    bar.classList.remove('run');
    bar.classList.add('done');
  }
};
