import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {panel} from '../ui/components.js';
import {toast} from '../ui/feedback.js';
import {field, input, onSubmit, saveBar, select, trackDirty} from '../ui/forms.js';

const UINT64_MAX = 18446744073709551615n;
const QUOTA_UNITS = Object.freeze({bytes: 1n, KB: 1000n, MB: 1000n ** 2n, GB: 1000n ** 3n, TB: 1000n ** 4n});

export function quotaToBytes(value, unit = 'bytes') {
  if (typeof value === 'number' && (!Number.isSafeInteger(value) || value < 0)) return {ok: false, error: 'Enter a whole number.'};
  if (typeof value !== 'string' && typeof value !== 'number' && typeof value !== 'bigint') return {ok: false, error: 'Enter a whole number.'};
  const raw = String(value).trim();
  if (!/^\d+$/.test(raw) || !Object.hasOwn(QUOTA_UNITS, unit)) return {ok: false, error: 'Enter a whole number and choose a valid unit.'};
  const bytes = BigInt(raw) * QUOTA_UNITS[unit];
  if (bytes > UINT64_MAX) return {ok: false, error: 'The quota must be at most 18,446,744,073,709,551,615 bytes.'};
  return {ok: true, value: bytes.toString()};
}

export function trafficSettingsPanel(id, settings, onSaved, signal) {
  let saved = settings;
  const quota = input({type: 'number', name: 'quota_value', min: 0, step: 1, required: true, value: settings.quota_bytes,
    inputMode: 'numeric', autocomplete: 'off', id: 'field-quota', dataset: {field: 'quota_bytes'}});
  const quotaUnit = select([
    ['bytes', 'Bytes'], ['KB', 'KB (1,000 bytes)'], ['MB', 'MB (1,000,000 bytes)'], ['GB', 'GB (1,000,000,000 bytes)'], ['TB', 'TB (1,000,000,000,000 bytes)']
  ], 'bytes', {name: 'quota_unit', id: 'field-quota-unit', 'aria-label': 'Quota unit'});
  const checkQuota = () => {
    const result = quotaToBytes(quota.value, quotaUnit.value);
    quota.setCustomValidity(result.ok ? '' : result.error);
  };
  quota.addEventListener('input', checkQuota);
  quotaUnit.addEventListener('change', checkQuota);
  const billing = input({type: 'number', name: 'billing_day', min: 1, max: 28, step: 1, required: true,
    value: String(settings.billing_day), id: 'field-billing', inputMode: 'numeric'});
  const controls = saveBar('Save billing settings', 'billing-save');
  const form = h('form', {class: 'form form-wide'},
    field('Traffic quota', quota, {inner: h('div', {class: 'form-row'}, quota, h('div', null, quotaUnit)),
      hint: 'Decimal SI units (1 KB = 1,000 bytes); charts use binary units. Enter 0 for unlimited.'}),
    field('Billing day', billing, {hint: 'Day of the month a traffic cycle starts, 1–28.'}), controls.bar);
  const restore = () => {
    billing.value = String(saved.billing_day);
    const bytes = BigInt(saved.quota_bytes);
    let unit = 'bytes';
    for (const [name, multiplier] of Object.entries(QUOTA_UNITS).reverse()) {
      if (bytes > 0n && bytes % multiplier === 0n) { unit = name; break; }
    }
    quota.value = (bytes / QUOTA_UNITS[unit]).toString();
    quotaUnit.value = unit;
    checkQuota();
  };
  restore();
  trackDirty(form, controls);
  controls.discard.addEventListener('click', () => { restore(); form.syncDirty(); });
  onSubmit(form, controls.save, async () => {
    const result = quotaToBytes(quota.value, quotaUnit.value);
    if (!result.ok) { quota.setCustomValidity(result.error); throw new Error(result.error); }
    saved = await api(`/nodes/${encodeURIComponent(id)}/traffic-settings`, {method: 'PATCH', signal,
      body: {billing_day: Number(billing.value), quota_bytes: result.value}});
    restore();
    form.markClean();
    toast('Billing settings saved.');
    await onSaved(saved);
  });
  return panel('Traffic billing', {}, form);
}
