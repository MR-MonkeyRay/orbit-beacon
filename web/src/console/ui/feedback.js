import {h, icon} from '../../shared/dom.js';

const toasts = document.getElementById('toasts');
const bar = document.getElementById('progress');

export function toast(message, options = {}) {
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
    h('button', {type: 'button', 'aria-label': 'Dismiss notification', onclick: dismiss}, icon('x')));
  toasts.append(item);
  while (toasts.children.length > 4) toasts.firstElementChild.remove();
  timer = setTimeout(dismiss, options.error ? 9000 : 4500);
}

export function toastError(error) {
  toast(error.message, {error: true, detail: error.requestID ? `Request ${error.requestID}` : undefined});
}

// Thin bar at the top of the viewport while a page loads.
export const progress = {
  start() {
    bar.className = 'progress';
    void bar.offsetWidth;
    bar.classList.add('run');
  },
  done() { bar.classList.replace('run', 'done'); }
};
