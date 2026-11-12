import {h, icon} from './dom.js';
import {getLocale, getLocalePreference, setLocalePreference, t} from './i18n.js';

export function localeToggle({beforeChange = async () => true, onError = () => {}, reload = () => location.reload(), signal} = {}) {
  let pending = false;
  let mounted = false;
  const button = h('button', {type: 'button', class: 'icon-button'}, icon('languages'));
  const paint = () => {
    const preference = getLocalePreference();
    const active = preference === 'system' ? getLocale() : preference;
    button.title = t('controls.language.switch', {language: active === 'en' ? '中文' : 'English'});
    button.setAttribute('aria-label', button.title);
    return active === 'en' ? 'zh-CN' : 'en';
  };
  const syncBusy = () => {
    button.disabled = pending || Boolean(document.querySelector('[data-busy="true"],button.busy'));
    if (button.isConnected) mounted = true;
    else if (mounted) observer?.disconnect();
  };
  const observer = typeof MutationObserver === 'function' ? new MutationObserver(syncBusy) : null;
  observer?.observe(document.body, {subtree: true, childList: true, attributes: true, attributeFilter: ['class', 'data-busy']});
  signal?.addEventListener('abort', () => observer?.disconnect(), {once: true});
  if (signal?.aborted) observer?.disconnect();
  button.addEventListener('click', async () => {
    syncBusy();
    if (button.disabled || signal?.aborted) return;
    const next = paint();
    pending = true;
    syncBusy();
    try {
      if (await changeLocalePreference(next, {beforeChange, reload, onError})) paint();
    } finally {
      pending = false;
      syncBusy();
    }
  });
  paint();
  syncBusy();
  return button;
}

// 共享确认、存储与整页重载流程。
export async function changeLocalePreference(next, {beforeChange = async () => true, reload = () => location.reload(), onError = () => {}} = {}) {
  try {
    if (!(await beforeChange())) return false;
    try { setLocalePreference(next); }
    catch {
      onError(Object.assign(new Error('Could not save language preference'), {code: 'locale_storage_failed'}));
      return false;
    }
    reload();
    return true;
  } catch (error) { onError(error); return false; }
}
