import {h, reducedMotion} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {onSession} from '../api.js';
import {button, iconButton, uid} from './components.js';
import {onSubmit} from './forms.js';

const dialogs = new Set();
let dialogEpoch = 0;
const activeActions = [];

// Body-level secrets obey the same boundary even while routing is awaiting data.
onSession((_session, detail) => { if (detail.boundary && detail.previousUser) closeDialogs(); });

// Navigation removes body-level dialogs before the view is replaced.
export function closeDialogs() {
  dialogEpoch += 1;
  let focusID = '';
  for (const entry of dialogs) {
    focusID = focusID || entry.opener?.id || '';
    entry.cancel();
  }
  return focusID;
}

export const dialogActionStale = () => activeActions.some((epoch) => epoch !== dialogEpoch);

export const hasUnsavedDialogChanges = () => [...dialogs].some((entry) => entry.dirty());

// Native modal <dialog> with enter/exit animation, Escape and backdrop close.
// Resolves `closed` with the return value once the dialog is gone.
export function modal(title, build) {
  const titleID = uid('dialog');
  const dialog = h('dialog', {'aria-labelledby': titleID});
  const opener = document.activeElement;
  let resolve;
  let finished = false;
  let timer = 0;
  let confirming = false;
  const closed = new Promise((done) => { resolve = done; });
  const finish = (value) => {
    if (finished) return;
    finished = true;
    clearTimeout(timer);
    dialogs.delete(entry);
    if (dialog.open) dialog.close(value);
    dialog.remove();
    const target = opener?.isConnected && !opener.disabled ? opener : document.querySelector('#view h1');
    if (target) target.focus({preventScroll: true});
    resolve(value);
  };
  const close = (value = 'cancel') => {
    if (!dialog.open || dialog.classList.contains('closing')) return;
    if (value === 'cancel') { dismiss(); return; }
    if (reducedMotion.matches) { finish(value); return; }
    dialog.classList.add('closing');
    dialog.addEventListener('animationend', () => finish(value), {once: true});
    timer = setTimeout(() => finish(value), 260);
  };
  const entry = {opener, cancel: () => finish('cancel'), dirty: () => snapshot() !== initial};
  let initial;
  const snapshot = () => JSON.stringify([...dialog.querySelectorAll('input,select,textarea,[data-tags]')]
    .map((control) => control.dataset.tags !== undefined ? control.getValue() : control.multiple ? [...control.selectedOptions].map((option) => option.value)
      : ['checkbox', 'radio'].includes(control.type) ? control.checked : control.value));
  const dismiss = async () => {
    if (finished || confirming || dialog.querySelector('button.busy')) return;
    if (snapshot() !== initial) {
      // Only one discard prompt may sit above this form at a time.
      confirming = true;
      const discard = await confirmDiscard();
      confirming = false;
      if (!discard || finished || dialog.querySelector('button.busy')) return;
    }
    close('dismiss');
  };
  dialog.addEventListener('cancel', (event) => { event.preventDefault(); dismiss(); });
  let pressedBackdrop = false;
  dialog.addEventListener('pointerdown', (event) => { pressedBackdrop = event.target === dialog; });
  dialog.addEventListener('click', (event) => { if (event.target === dialog && pressedBackdrop) dismiss(); });
  dialog.addEventListener('close', () => finish(dialog.returnValue));
  dialog.append(h('div', {class: 'dialog-head'}, h('h2', {id: titleID}, title), iconButton('x', t('controls.dialog.close'), () => close())), build(close));
  initial = snapshot();
  dialogs.add(entry);
  document.body.append(dialog);
  dialog.showModal();
  const preferred = dialog.querySelector('[data-autofocus]');
  if (preferred) preferred.focus();
  return {close, closed};
}

export function confirmDialog({title, message, confirm, danger = false}) {
  const {closed} = modal(title, (close) => h('div', null,
    h('div', {class: 'dialog-body'}, h('p', null, message)),
    h('div', {class: 'dialog-foot'},
      h('button', {type: 'button', class: 'btn', onclick: () => close(), dataset: {autofocus: ''}}, t('controls.cancel')),
      button(confirm, {variant: danger ? 'danger' : 'primary', className: danger ? 'solid' : '', onClick: () => close('confirm')}))));
  return closed.then((value) => value === 'confirm');
}

export const confirmDiscard = () => confirmDialog({
  title: t('controls.discard.title'), message: t('controls.discard.message'), confirm: t('controls.discard'), danger: true
});

// Form in a dialog. build(form) adds fields; action(form) runs on submit and the
// dialog closes when it resolves. Errors stay inside the dialog.
export function formDialog({title, intro, submit, build, action}) {
  return modal(title, (close) => {
    const form = h('form', {class: 'form'});
    const errorBox = h('div', {class: 'form-error', role: 'alert', hidden: true});
    const submitButton = button(submit, {type: 'submit', variant: 'primary'});
    form.append(errorBox);
    build(form);
    form.append(h('div', {class: 'dialog-foot'}, button(t('controls.cancel'), {onClick: () => close()}), submitButton));
    onSubmit(form, submitButton, async () => {
      const epoch = dialogEpoch;
      activeActions.push(epoch);
      try { await action(form); close('done'); }
      catch (error) { if (form.isConnected) throw error; }
      finally { activeActions.splice(activeActions.indexOf(epoch), 1); }
    }, errorBox);
    const first = form.querySelector('input,select,textarea');
    if (first) first.dataset.autofocus = '';
    return h('div', {class: 'dialog-body'}, intro ? h('p', {class: 'lede'}, intro) : null, form);
  });
}
