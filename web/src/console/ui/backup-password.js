import {h} from '../../shared/dom.js';
import {t, getLocale} from '../../shared/i18n.js';
import {errorText} from '../../shared/errors.js';
import {api} from '../api.js';
import {busy, button} from './components.js';

// Password material exists only inside this mounted component and is never
// prefetched by details/list/polling. Dispose on dialog close or navigation.
export function backupPassword(task, {signal} = {}) {
  const element = h('section', {class: 'backup-password', 'aria-label': t('data.password.section')});
  const status = h('p', {class: 'muted', role: 'status'});
  const value = h('code', {class: 'recovery-secret', tabindex: '0', 'aria-label': t('data.password.value'), hidden: true});
  const warning = h('p', {class: 'note'}, t('data.password.warning'));
  let secret;
  let request;
  let timer;
  let disposed = false;
  let generation = 0;
  const controls = [];
  const conceal = () => {
    generation += 1;
    request?.abort(); request = null;
    secret = undefined;
    value.textContent = ''; value.hidden = true;
    reveal.textContent = t('data.password.reveal');
    clearTimeout(timer); timer = null;
  };
  const touch = () => {
    clearTimeout(timer);
    if (secret && !disposed) timer = setTimeout(() => { conceal(); status.textContent = t('data.password.inactive'); }, 30000);
  };
  const obtain = async () => {
    if (disposed || signal?.aborted) return null;
    if (secret) { touch(); return secret; }
    if (request) return null;
    const current = new AbortController(); request = current;
    const epoch = generation;
    controls.forEach((control) => busy(control, true));
    status.textContent = t('data.password.loading');
    try {
      const result = await api(`/tasks/${encodeURIComponent(task.id)}/password`, {method: 'POST', body: {}, signal: current.signal});
      if (disposed || signal?.aborted || epoch !== generation || request !== current) return null;
      secret = result;
      status.textContent = t('data.password.availableUntil', {time: new Date(result.password_expires_at).toLocaleString(getLocale())});
      touch();
      return result;
    } catch (error) {
      if (!disposed && error.name !== 'AbortError') {
        const message = errorText(error.code === 'secret_unreadable' ? {code: 'backup_secret_unreadable', requestID: error.requestID} : error);
        status.textContent = t('data.password.unavailable', {error: message});
      }
      return null;
    } finally {
      if (request === current) request = null;
      if (!disposed) controls.forEach((control) => busy(control, false));
    }
  };
  const reveal = button(t('data.password.reveal'), {onClick: async () => {
    if (!value.hidden) { conceal(); status.textContent = t('data.password.hidden'); return; }
    const result = await obtain();
    if (!result || disposed) return;
    value.textContent = result.password; value.hidden = false; reveal.textContent = t('data.password.hide'); touch();
  }});
  const copy = button(t('data.password.copy'), {onClick: async () => {
    const result = await obtain();
    if (!result || disposed) return;
    try {
      if (!navigator.clipboard?.writeText) throw new Error('Clipboard unavailable');
      await navigator.clipboard.writeText(result.password);
      if (!disposed) status.textContent = t('data.password.copied');
    } catch {
      if (disposed) return;
      value.textContent = result.password; value.hidden = false; reveal.textContent = t('data.password.hide'); value.focus();
      const selection = window.getSelection();
      const range = document.createRange(); range.selectNodeContents(value); selection?.removeAllRanges(); selection?.addRange(range);
      status.textContent = t('data.password.clipboardUnavailable');
    }
    touch();
  }});
  const download = button(t('data.password.download'), {onClick: async () => {
    const result = await obtain();
    if (!result || disposed) return;
    const text = `${t('data.recovery.title')}\narchive_id: ${task.id}\narchive_name: ${result.archive_name}\narchive_sha256: ${result.archive_sha256}\ncreated_at: ${task.created_at}\npassword_expires_at: ${result.password_expires_at}\npassword: ${result.password}\n\n${t('data.recovery.warning')}\n`;
    const url = URL.createObjectURL(new Blob([text], {type: 'text/plain;charset=utf-8'}));
    const anchor = h('a', {href: url, download: `orbitbeacon-recovery-${task.id}.txt`, hidden: true});
    element.append(anchor); anchor.click(); anchor.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    status.textContent = t('data.password.downloadStarted');
    touch();
  }});
  controls.push(reveal, copy, download);
  const dispose = () => {
    if (disposed) return;
    disposed = true; conceal();
    signal?.removeEventListener('abort', dispose);
    element.removeEventListener('pointerdown', touch);
    element.removeEventListener('keydown', touch);
  };
  signal?.addEventListener('abort', dispose, {once: true});
  element.addEventListener('pointerdown', touch);
  element.addEventListener('keydown', touch);
  element.append(h('h3', null, t('data.password.heading')), warning);
  if (task.password_available) element.append(h('div', {class: 'head-actions'}, controls), value, status);
  else element.append(h('p', {class: 'muted'}, t(task.restored ? 'data.password.restored' : 'data.password.absent')));
  if (signal?.aborted) dispose();
  return {element, dispose};
}
