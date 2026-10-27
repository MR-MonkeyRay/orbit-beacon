import {h, reducedMotion} from '../../shared/dom.js';
import {button, iconButton, uid} from './components.js';
import {onSubmit} from './forms.js';

// Native modal <dialog> with enter/exit animation, Escape and backdrop close.
// Resolves `closed` with the return value once the dialog is gone.
export function modal(title, build) {
  const titleID = uid('dialog');
  const dialog = h('dialog', {'aria-labelledby': titleID});
  let resolve;
  const closed = new Promise((done) => { resolve = done; });
  const close = (value = 'cancel') => {
    if (!dialog.open || dialog.classList.contains('closing')) return;
    const finish = () => { if (dialog.open) dialog.close(value); dialog.remove(); };
    if (reducedMotion.matches) { finish(); return; }
    dialog.classList.add('closing');
    dialog.addEventListener('animationend', finish, {once: true});
    setTimeout(finish, 260);
  };
  dialog.addEventListener('cancel', (event) => { event.preventDefault(); close(); });
  let pressedBackdrop = false;
  dialog.addEventListener('pointerdown', (event) => { pressedBackdrop = event.target === dialog; });
  dialog.addEventListener('click', (event) => { if (event.target === dialog && pressedBackdrop) close(); });
  dialog.addEventListener('close', () => resolve(dialog.returnValue));
  dialog.append(h('div', {class: 'dialog-head'}, h('h2', {id: titleID}, title), iconButton('x', 'Close dialog', () => close())), build(close));
  document.body.append(dialog);
  dialog.showModal();
  const preferred = dialog.querySelector('[data-autofocus]');
  if (preferred) preferred.focus();
  return {close, closed};
}

export function confirmDialog({title, message, confirm, danger = false}) {
  let confirmed = false;
  const {closed} = modal(title, (close) => h('div', null,
    h('div', {class: 'dialog-body'}, h('p', null, message)),
    h('div', {class: 'dialog-foot'},
      h('button', {type: 'button', class: 'btn', onclick: () => close(), dataset: {autofocus: ''}}, 'Cancel'),
      button(confirm, {variant: danger ? 'danger' : 'primary', className: danger ? 'solid' : '', onClick: () => { confirmed = true; close('confirm'); }}))));
  return closed.then(() => confirmed);
}

export const confirmDiscard = () => confirmDialog({
  title: 'Discard unsaved changes?', message: 'You have edits here that have not been saved.', confirm: 'Discard changes', danger: true
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
    form.append(h('div', {class: 'dialog-foot'}, button('Cancel', {onClick: () => close()}), submitButton));
    onSubmit(form, submitButton, async () => { await action(form); close('done'); }, errorBox);
    const first = form.querySelector('input,select,textarea');
    if (first) first.dataset.autofocus = '';
    return h('div', {class: 'dialog-body'}, intro ? h('p', {class: 'lede'}, intro) : null, form);
  });
}
