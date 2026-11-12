import {h} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {api, session, getNavigationSignal} from '../api.js';
import {formatMilliseconds, formatPercent} from '../format.js';
import {button, dataTable, emptyState, iconButton, pageHead, panel} from '../ui/components.js';
import {pagePlaceholder, placeholderHead, placeholderPanel, placeholderTable} from '../ui/placeholders.js';
import {confirmDialog, formDialog, modal} from '../ui/dialogs.js';
import {field, input, select, toggleSwitch} from '../ui/forms.js';
import {chart} from '../ui/chart.js';
import {toastError} from '../ui/feedback.js';
import {errorText} from '../../shared/errors.js';

const numeric = ['interval_seconds', 'timeout_seconds', 'latency_threshold_ms', 'display_order'];
const switches = ['public', 'notify', 'enabled'];
export function servicePayload(controls) {
  return {name: controls.name.value.trim(), type: controls.type.value, target: controls.target.value.trim(),
    ...Object.fromEntries(numeric.map((key) => [key, Number(controls[key].value)])),
    ...Object.fromEntries(switches.map((key) => [key, controls[key].checked]))};
}
export function serviceEditor(item, {signal, saved}) {
  if (!session.user || !session.csrf || signal?.aborted) return;
  const controls = {};
  return formDialog({title: t(item ? 'services.edit' : 'services.add'), submit: t('services.save'),
    build(form) {
      const add = (key, props) => { const control = controls[key] = input({name: key, ...props}); return field(t(`services.${key}`), control); };
      const name = add('name', {value: item?.name || '', required: true, maxlength: 100});
      const type = controls.type = select([['http', 'HTTP / HTTPS'], ['tcp', 'TCP'], ['icmp', 'ICMP']], item?.type || 'http', {name: 'type'});
      const target = add('target', {value: item?.target || '', required: true, maxlength: 2048, autocomplete: 'off'});
      const sync = () => { controls.target.placeholder = {http: 'https://example.com/health', tcp: 'example.com:443', icmp: 'example.com'}[type.value]; };
      type.addEventListener('change', sync); sync();
      const numberFields = numeric.map((key) => add(key, {type: 'number', required: true,
        value: item?.[key] ?? ({interval_seconds: 60, timeout_seconds: 10, latency_threshold_ms: 0, display_order: 0})[key],
        min: key === 'display_order' ? -1000000 : key === 'interval_seconds' ? 30 : key === 'timeout_seconds' ? 1 : 0,
        max: ({interval_seconds: 3600, timeout_seconds: 30, latency_threshold_ms: 3600000, display_order: 1000000})[key], step: key === 'latency_threshold_ms' ? 'any' : 1}));
      const checkTimeout = () => controls.timeout_seconds.setCustomValidity(Number(controls.timeout_seconds.value) >= Number(controls.interval_seconds.value) ? t('services.timeoutHint') : '');
      controls.interval_seconds.addEventListener('input', checkTimeout); controls.timeout_seconds.addEventListener('input', checkTimeout);
      const toggles = switches.map((key) => { const control = controls[key] = input({name: key, type: 'checkbox', checked: item?.[key] ?? key === 'enabled'}); return toggleSwitch(t(`services.${key}`), control); });
      form.append(name, field(t('services.type'), type), target, h('div', {class: 'service-editor-grid'}, numberFields),
        h('p', {class: 'text-note'}, t('services.thresholdHint')), h('div', {class: 'service-switches'}, toggles));
    },
    async action() {
      if (signal?.aborted || !session.user || !session.csrf) throw new DOMException('Cancelled', 'AbortError');
      await api(item ? `/services/${encodeURIComponent(item.id)}` : '/services', {method: item ? 'PATCH' : 'POST', body: servicePayload(controls), signal});
      await saved();
    }
  });
}
const statusOf = (item) => item.enabled ? item.latest?.status || 'unknown' : 'disabled';
const badge = (item) => { const status = statusOf(item); return h('span', {class: `badge ${status === 'up' ? 'online' : status === 'down' ? 'offline' : status}`}, t(`services.status.${status}`)); };
const errorCodes = new Set(['timeout', 'dns', 'refused', 'denied', 'unsupported', 'http_status', 'error']);
const errorLabel = (latest) => {
  if (!errorCodes.has(latest.error)) return latest.error;
  const text = t(`services.error.${latest.error}`);
  return latest.status_code ? `${text} (${latest.status_code})` : text;
};
export const permissionHint = (item) => item.latest?.error === 'permission_denied' ? h('p', {class: 'text-note'}, t('services.permissionHint')) : null;

export function serviceDetail(item, signal) {
  return modal(item.name, (_close, dialogSignal) => {
    const charts = h('div', {class: 'service-charts'});
    const status = h('p', {class: 'text-note', role: 'status'});
    let range = '24h', request;
    const ranges = ['1h', '24h', '7d', '30d'];
    const controls = ranges.map((value) => h('button', {type: 'button', 'aria-pressed': String(value === range), onclick: () => {
      range = value; controls.forEach((button, i) => button.setAttribute('aria-pressed', String(ranges[i] === range))); load();
    }}, t(`services.range.${value}`)));
    const retry = button(t('common.retry'), {small: true, onClick: () => load()}); retry.hidden = true;
    const stop = () => request?.abort(); signal?.addEventListener('abort', stop, {once: true});
    dialogSignal.addEventListener('abort', () => { stop(); signal?.removeEventListener('abort', stop); }, {once: true});
    async function load() {
      request?.abort(); if (dialogSignal.aborted || signal?.aborted) return;
      const current = request = new AbortController(); status.textContent = t('services.loading'); retry.hidden = true;
      try {
        const data = await api(`/services/${encodeURIComponent(item.id)}/history?range=${range}`, {signal: current.signal});
        if (current.signal.aborted || dialogSignal.aborted || signal?.aborted) return;
        if (!Array.isArray(data.buckets)) throw Object.assign(new Error('Invalid history'), {code: 'unreadable_response'});
        const latency = data.buckets.filter((p) => p.avg_latency_ms !== null).map((p) => ({t: p.time, avg: p.avg_latency_ms, max: p.max_latency_ms}));
        const availability = data.buckets.filter((p) => p.availability !== null).map((p) => ({t: p.time, avg: p.availability, max: p.availability}));
        charts.replaceChildren(...[[latency, 'ms', 'services.latency'], [availability, 'percent', 'services.availability']].filter(([points]) => points.length).map(([points, unit, label]) =>
          h('section', {class: 'history-metric'}, h('h3', null, t(label)), chart({title: t(label), unit, id: `service-${unit}`, series: [{label: t(label), stepSeconds: data.step_seconds, points}]}))));
        status.textContent = availability.length ? '' : t('services.noHistory');
      } catch (error) { if (!current.signal.aborted) { status.textContent = errorText(error); retry.hidden = false; } }
    }
    load();
    return h('div', {class: 'dialog-body stack'}, h('div', {class: 'head-actions'}, badge(item), h('span', {class: 'mono service-target'}, item.target)),
      permissionHint(item), item.latest?.error && item.latest.error !== 'permission_denied' ? h('p', {class: 'text-note'}, errorLabel(item.latest)) : null,
      h('div', {class: 'segmented', role: 'group', 'aria-label': t('services.historyRange')}, controls), status, retry, charts);
  });
}

export function servicesPlaceholder() {
  const columns = ['name', 'type', 'target', 'status', 'latency', 'availability24h', 'visibility', 'actions'].map((key) => t(`services.${key}`));
  return pagePlaceholder(h('div', {class: 'services-page'}, placeholderHead(t('services.title'), {status: false}),
    placeholderPanel(null, placeholderTable(columns, {rows: 4, className: 'services-table'}))));
}

export async function servicesPage() {
  const signal = getNavigationSignal();
  const content = h('div');
  let polling, request;
  const add = () => serviceEditor(null, {signal, saved: load});
  const root = h('div', {class: 'services-page'}, pageHead(t('services.title'), button(t('services.add'), {variant: 'primary', icon: 'plus', onClick: add})), content);
  async function remove(item) {
    if (!(await confirmDialog({title: t('services.deleteNamed', {name: item.name}), message: t('services.deleteHint'), confirm: t('services.delete'), danger: true})) || signal?.aborted) return;
    try { await api(`/services/${encodeURIComponent(item.id)}`, {method: 'DELETE', signal}); await load(); }
    catch (error) { if (!signal?.aborted) toastError(error); }
  }
  async function load() {
    clearTimeout(polling); request?.abort(); if (signal?.aborted) return;
    const current = request = new AbortController();
    try {
      const data = await api('/services', {signal: current.signal});
      if (current.signal.aborted || signal?.aborted) return;
      if (!Array.isArray(data.services)) throw Object.assign(new Error('Invalid services'), {code: 'unreadable_response'});
      const rows = data.services.map((item) => {
        const detail = () => serviceDetail(item, signal);
        const name = button(item.name, {className: 'service-name', onClick: detail}); name.id = `service-${item.id}`;
        const edit = iconButton('edit', t('services.editNamed', {name: item.name}), () => serviceEditor(item, {signal, saved: load})); edit.id = `service-edit-${item.id}`;
        const removeButton = iconButton('trash', t('services.deleteNamed', {name: item.name}), () => remove(item)); removeButton.id = `service-delete-${item.id}`;
        const row = h('tr', {onclick: (event) => { if (!event.target.closest('button,a')) detail(); }},
          h('td', {class: 'service-name-cell'}, name), h('td', {class: 'service-type'}, item.type.toUpperCase()), h('td', {class: 'service-target'}, item.target, permissionHint(item)),
          h('td', {class: 'service-status'}, badge(item)), h('td', {class: 'num service-latency', 'data-label': t('services.latency')}, formatMilliseconds(item.latest?.latency_ms)),
          h('td', {class: 'num service-availability', 'data-label': t('services.availability24h')}, formatPercent(item.availability_24h)),
          h('td', {class: 'service-visibility'}, [item.public ? t('services.public') : '', item.notify ? t('services.notify') : ''].filter(Boolean).join(' · ') || '—'),
          h('td', {class: 'actions'}, edit, removeButton));
        return row;
      });
      const focusID = content.contains(document.activeElement) ? document.activeElement.id : null;
      content.replaceChildren(data.services.length ? panel(null, {}, dataTable({caption: t('services.title'), className: 'services-table', columns: ['name', 'type', 'target', 'status', 'latency', 'availability24h', 'visibility', 'actions'].map((key) => t(`services.${key}`)), body: h('tbody', null, rows)}))
        : emptyState(t('services.empty'), null, button(t('services.add'), {variant: 'primary', onClick: add})));
      if (focusID) document.getElementById(focusID)?.focus({preventScroll: true});
    } catch (error) {
      if (!current.signal.aborted && !signal?.aborted) {
        const failure = h('div', {class: 'callout', role: 'alert'}, errorText(error), button(t('common.retry'), {onClick: load}));
        if (!content.children.length) content.append(failure); else { content.querySelector('[role="alert"]')?.remove(); content.prepend(failure); }
      }
    } finally { if (!current.signal.aborted && !signal?.aborted) polling = setTimeout(load, 10000); }
  }
  signal?.addEventListener('abort', () => { clearTimeout(polling); request?.abort(); }, {once: true});
  await load(); return root;
}
