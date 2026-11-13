import {h, reducedMotion} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {onSession} from '../api.js';
import {button, iconButton, uid} from './components.js';
import {onSubmit} from './forms.js';

const dialogs = new Set();
let dialogEpoch = 0;
const activeActions = [];

// 同一个 live region 随最上层 modal 移动，保持提示可交互、可被辅助技术读取。
function hostToasts() {
  const toasts = document.getElementById('toasts');
  const host = [...dialogs].findLast((entry) => entry.dialog.open)?.dialog || document.body;
  if (toasts && toasts.parentElement !== host) host.append(toasts);
}

// body 级的秘密信息遵守同一边界，即使路由正在等待数据。
onSession((_session, detail) => { if (detail.boundary && detail.previousUser) closeDialogs(); });

// 导航在视图替换前移除 body 级对话框。
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

// 原生模态 <dialog>，带进入/退出动画，支持 Escape 和点击背景关闭。
// 对话框关闭后，closed Promise 以对话框返回值完成。
export function modal(title, build) {
  const controller = new AbortController();
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
    controller.abort();
    clearTimeout(timer);
    dialogs.delete(entry);
    hostToasts();
    if (dialog.open) dialog.close(value);
    dialog.remove();
    // 列表在对话框打开期间重绘时入口会被同 id 的新元素替换，按 id 找回。
    // 视口跨越 767px 时抽屉会把入口所在容器置为 inert，焦点必须改落到页面标题。
    const current = opener?.isConnected ? opener : opener?.id ? document.getElementById(opener.id) : null;
    const target = current && !current.disabled && !current.closest?.('[inert]') ? current : document.querySelector('#view h1');
    if (target) target.focus({preventScroll: true});
    resolve(value);
  };
  const close = (value = 'cancel') => {
    if (!dialog.open || dialog.classList.contains('closing')) return;
    if (value === 'cancel') { dismiss(); return; }
    controller.abort();
    if (reducedMotion.matches) { finish(value); return; }
    dialog.classList.add('closing');
    dialog.addEventListener('animationend', (event) => { if (event.target === dialog) finish(value); });
    timer = setTimeout(() => finish(value), 260);
  };
  const entry = {dialog, opener, cancel: () => finish('cancel'), dirty: () => snapshot() !== initial};
  let initial;
  // 确认输入不是用户的工作成果，取消时无需再次确认。
  const snapshot = () => JSON.stringify([...dialog.querySelectorAll('input:not([data-transient]),select,textarea,[data-tags]')]
    .map((control) => control.dataset.tags !== undefined ? control.getValue() : control.multiple ? [...control.selectedOptions].map((option) => option.value)
      : ['checkbox', 'radio'].includes(control.type) ? control.checked : control.value));
  const dismiss = async () => {
    if (finished || confirming || dialog.querySelector('button.busy')) return;
    if (snapshot() !== initial) {
      // 此表单上方同时只能有一个放弃提示。
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
  dialog.append(h('div', {class: 'dialog-head'}, h('h2', {id: titleID}, title), iconButton('x', t('controls.dialog.close'), () => close())), build(close, controller.signal));
  initial = snapshot();
  dialogs.add(entry);
  document.body.append(dialog);
  dialog.showModal();
  hostToasts();
  const preferred = dialog.querySelector('[data-autofocus]');
  if (preferred) preferred.focus();
  return {close, closed, signal: controller.signal};
}

export function confirmDialog({title, message, confirm, danger = false}) {
  const {closed} = modal(title, (close) => h('div', null,
    message ? h('div', {class: 'dialog-body'}, h('p', null, message)) : null,
    h('div', {class: 'dialog-foot'},
      h('button', {type: 'button', class: 'btn', onclick: () => close(), dataset: {autofocus: ''}}, t('controls.cancel')),
      button(confirm, {variant: danger ? 'danger' : 'primary', className: danger ? 'solid' : '', onClick: () => close('confirm')}))));
  return closed.then((value) => value === 'confirm');
}

export const confirmDiscard = () => confirmDialog({
  title: t('controls.discard.title'), confirm: t('controls.discard'), danger: true
});

// 对话框中的表单。build(form) 添加字段；提交时执行 action(form)，
// 成功后关闭对话框，错误则显示在对话框内。
export function formDialog({title, submit, build, action}) {
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
    return h('div', {class: 'dialog-body'}, form);
  });
}
