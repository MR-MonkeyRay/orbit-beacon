import {h} from '../../shared/dom.js';
import {modal} from './dialogs.js';

export function detailDialog({title, loading, loadingClass, signal, valid = true, invalid, abortBeforeValidate = false, load, render, failure}) {
  const controller = new AbortController();
  let content;
  let disposed = false;
  const body = h('div', {class: 'dialog-body stack'}, h('p', {class: loadingClass, role: 'status'}, loading));
  const dialog = modal(title, () => body);
  const dispose = () => {
    if (disposed) return;
    disposed = true;
    controller.abort();
    content?.dispose?.();
    signal?.removeEventListener('abort', stop);
  };
  const stop = () => { dispose(); dialog.close('navigation'); };
  signal?.addEventListener('abort', stop, {once: true});
  dialog.closed.then(dispose);
  if (abortBeforeValidate && signal?.aborted) { stop(); return dialog; }
  if (!valid) {
    body.replaceChildren(h('p', {role: 'alert'}, invalid));
    return dialog;
  }
  if (signal?.aborted) { stop(); return dialog; }
  load(controller.signal).then((value) => {
    if (disposed || signal?.aborted) return;
    content = render(value, controller.signal);
    const nodes = content?.nodes ?? content;
    body.replaceChildren(...(Array.isArray(nodes) ? nodes : [nodes]));
  }).catch((error) => {
    if (disposed || error.name === 'AbortError') return;
    body.replaceChildren(failure(error));
  });
  return dialog;
}
