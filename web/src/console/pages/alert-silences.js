import {h, byteLength} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {api, session} from '../api.js';
import {button, dataTable, iconButton, panel} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toastError} from '../ui/feedback.js';
import {field, input, validateBytes} from '../ui/forms.js';
import {alertScope} from '../ui/alert-scope.js';
import {scopeLabel, validationError} from '../lib/alert-utils.js';
import {eventTime} from './alert-events.js';

export const localDateTime = (date) => {
  const pad = (n) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
};

export function silenceEditor({groups, nodes, signal, saved, scope_kind, scope_id}) {
  if (!session.user || !session.csrf || signal?.aborted) return;
  const now = new Date();
  let reason, starts, ends, scope, scopeID;
  return formDialog({title: t('alerts.silences.add'), submit: t('alerts.silences.add'),
    build: (form) => {
      reason = input({name: 'reason', required: true, autocomplete: 'off'});
      validateBytes(reason, 1, 255, t('alerts.silences.reason'));
      starts = input({name: 'starts_at', type: 'datetime-local', step: 1, required: true, value: localDateTime(now)});
      ends = input({name: 'ends_at', type: 'datetime-local', step: 1, required: true, value: localDateTime(new Date(now.getTime() + 2 * 3600000))});
      const selection = alertScope({scope_kind, scope_id}, {groups, nodes});
      ({scope, scopeID} = selection);
      // 常用时长一键设置结束时间（从开始时间起算），也可直接改时间。
      const quick = h('div', {class: 'rule-presets silence-durations', role: 'group', 'aria-label': t('alerts.silences.ends')},
        [[1, 'hours'], [2, 'hours'], [1, 'days'], [7, 'days']].map(([count, unit]) => button(t(`nodes.range.${unit}`, {count}), {small: true, onClick: () => {
          const start = new Date(starts.value);
          const base = Number.isFinite(start.getTime()) ? start : new Date();
          ends.value = localDateTime(new Date(base.getTime() + count * (unit === 'hours' ? 3600000 : 86400000)));
          ends.dispatchEvent(new Event('input', {bubbles: true}));
        }})));
      form.append(field(t('alerts.silences.reason'), reason), h('div', {class: 'rule-scope'}, ...selection.fields),
        h('div', {class: 'rule-scope'}, field(t('alerts.silences.starts'), starts), field(t('alerts.silences.ends'), ends)), quick);
    },
    action: async () => {
      if (!session.user || !session.csrf || signal?.aborted) throw new DOMException('Cancelled', 'AbortError');
      const errors = {};
      const trimmed = reason.value.trim();
      if (!trimmed) errors.reason = t('controls.validation.bytesRequired', {label: t('alerts.silences.reason')});
      else if (byteLength(trimmed) > 255) errors.reason = t('controls.validation.bytesMaximum', {label: t('alerts.silences.reason'), max: 255, size: byteLength(trimmed)});
      const start = new Date(starts.value), end = new Date(ends.value);
      if (!Number.isFinite(start.getTime()) || !Number.isFinite(end.getTime()) || end <= start || end <= new Date()) errors.ends_at = t('alerts.silences.invalidEnd');
      else if (end - start > 30 * 24 * 3600000) errors.ends_at = t('alerts.silences.tooLong');
      if (scope.value !== 'all' && !scopeID.value) errors.scope_id = t('controls.validation.required');
      if (Object.keys(errors).length) throw validationError(errors);
      await api('/alert-silences', {method: 'POST', signal, body: {reason: trimmed, scope_kind: scope.value,
        scope_id: scope.value === 'all' ? null : scopeID.value, starts_at: start.toISOString(), ends_at: end.toISOString()}});
      await saved();
    }
  });
}

export function silenceList(silences, context) {
  const {groups, nodes, signal, saved} = context;
  const remove = async (silence) => {
    if (!session.user || !session.csrf || signal?.aborted) return;
    if (!(await confirmDialog({title: t('alerts.silences.deleteTitle', {reason: silence.reason}), confirm: t('alerts.silences.delete'), danger: true}))) return;
    if (!session.user || !session.csrf || signal?.aborted) return;
    try {
      await api(`/alert-silences/${encodeURIComponent(silence.id)}`, {method: 'DELETE', signal});
      await saved();
    } catch (error) { if (!signal?.aborted) toastError(error); }
  };
  return panel(null, {action: button(t('alerts.silences.add'), {small: true, variant: 'primary', onClick: () => silenceEditor(context)})},
    silences.length ? dataTable({caption: t('alerts.silences.title'),
      columns: [t('alerts.silences.reason'), t('alerts.column.scope'), t('alerts.silences.starts'), t('alerts.silences.ends'), t('alerts.column.state'), t('alerts.column.actions')],
      body: h('tbody', null, silences.map((silence) => h('tr', null,
        h('td', null, silence.reason), h('td', null, silence.scope_name || scopeLabel(silence, groups, nodes)),
        h('td', null, eventTime(silence.starts_at)), h('td', null, eventTime(silence.ends_at)),
        h('td', null, h('span', {class: `badge ${silence.active ? 'online' : 'pending'}`}, t(silence.active ? 'alerts.silences.active' : 'alerts.silences.scheduled'))),
        h('td', {class: 'actions'}, iconButton('trash', t('alerts.silences.delete'), () => remove(silence))))))})
      : h('p', {class: 'text-note'}, t('alerts.silences.empty')));
}

export const silencesPanel = (context) => silenceList(context.silences, context);
