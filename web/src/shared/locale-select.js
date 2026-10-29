import {h} from './dom.js';
import {getLocalePreference, setLocalePreference, t} from './i18n.js';

export function localeSelect({beforeChange = async () => true, onError = () => {}, reload = () => location.reload(), signal} = {}) {
  let preference = getLocalePreference();
  let pending = false;
  let mounted = false;
  const preferenceNames = {system: t('controls.followSystem'), 'zh-CN': '中文', en: 'English'};
  const select = h('select', {class: 'input', value: preference, title: t('controls.language.preference', {preference: preferenceNames[preference]}), 'aria-label': t('controls.language')},
    h('option', {value: 'system'}, t('controls.followSystem')),
    h('option', {value: 'zh-CN'}, '中文'),
    h('option', {value: 'en'}, 'English'));
  const syncBusy = () => {
    select.disabled = pending || Boolean(document.querySelector('[data-busy="true"],button.busy'));
    if (select.isConnected) mounted = true;
    else if (mounted) observer?.disconnect();
  };
  const observer = typeof MutationObserver === 'function' ? new MutationObserver(syncBusy) : null;
  observer?.observe(document.body, {subtree: true, childList: true, attributes: true, attributeFilter: ['class', 'data-busy']});
  signal?.addEventListener('abort', () => observer?.disconnect(), {once: true});
  if (signal?.aborted) observer?.disconnect();
  select.addEventListener('change', async () => {
    if (pending) { select.value = preference; return; }
    const next = select.value;
    if (next === preference) return;
    pending = true;
    syncBusy();
    try {
      if (!(await beforeChange())) { select.value = preference; return; }
      try { setLocalePreference(next); }
      catch {
        select.value = preference;
        onError(Object.assign(new Error('Could not save language preference'), {code: 'locale_storage_failed'}));
        return;
      }
      preference = next;
      select.title = t('controls.language.preference', {preference: preferenceNames[next]});
      reload();
    } catch (error) {
      select.value = preference;
      onError(error);
    } finally {
      pending = false;
      syncBusy();
    }
  });
  syncBusy();
  return select;
}
