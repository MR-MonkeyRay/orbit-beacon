import {h, byteLength} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {api, session} from '../api.js';
import {button, dataTable, iconButton, panel, skeleton} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toastError} from '../ui/feedback.js';
import {field, input, select, textarea} from '../ui/forms.js';
import {validationError} from '../lib/alert-utils.js';
import {localDateTime} from './alert-silences.js';
import {eventTime} from './alert-events.js';
import {errorText} from '../../shared/errors.js';

export function announcementEditor(item, {signal, saved}) {
  if (!session.user || !session.csrf || signal?.aborted) return;
  let level, title, body, starts, ends;
  return formDialog({title: t(item ? 'announcements.edit' : 'announcements.add'),
    submit: t(item ? 'settings.save' : 'announcements.add'),
    build(form) {
      level = select(['info', 'warning', 'critical'].map((value) => [value, t(`alerts.severity.${value}`)]), item?.level || 'info', {name: 'level'});
      title = input({name: 'title', required: true, value: item?.title || '', autocomplete: 'off'});
      body = textarea({name: 'body', required: true, rows: 5}, item?.body || '');
      starts = input({name: 'starts_at', type: 'datetime-local', step: 1, required: true,
        value: localDateTime(item ? new Date(item.starts_at) : new Date())});
      ends = input({name: 'ends_at', type: 'datetime-local', step: 1,
        value: item?.ends_at ? localDateTime(new Date(item.ends_at)) : ''});
      form.append(field(t('announcements.level'), level), field(t('announcements.heading'), title),
        field(t('announcements.body'), body), field(t('alerts.silences.starts'), starts), field(t('alerts.silences.ends'), ends));
    },
    async action() {
      if (!session.user || !session.csrf || signal?.aborted) throw new DOMException('Cancelled', 'AbortError');
      const errors = {};
      const content = body.value.replace(/\r\n/g, '\n').trim();
      const heading = title.value.trim();
      if (!heading || byteLength(heading) > 100 || /[\p{Cc}]/u.test(title.value)) errors.title = t('announcements.invalidTitle');
      if (!content || byteLength(content) > 1000 || /[\p{Cc}]/u.test(body.value.replace(/\r\n/g, '\n').replace(/\n/g, ''))) errors.body = t('announcements.invalidBody');
      const start = new Date(starts.value), end = ends.value ? new Date(ends.value) : null;
      if (!Number.isFinite(start.getTime()) || end && (!Number.isFinite(end.getTime()) || end <= start || !item && end <= new Date())) errors.ends_at = t('announcements.invalidEnd');
      if (Object.keys(errors).length) throw validationError(errors);
      await api(item ? `/announcements/${encodeURIComponent(item.id)}` : '/announcements', {
        method: item ? 'PATCH' : 'POST', signal,
        body: {level: level.value, title: heading, body: content, starts_at: start.toISOString(), ends_at: end ? end.toISOString() : null}
      });
      await saved();
    }
  });
}

export function announcementList(items, context) {
  const {signal, saved} = context;
  const remove = async (item) => {
    if (!session.user || !session.csrf || signal?.aborted) return;
    if (!(await confirmDialog({title: t('announcements.delete'), confirm: t('announcements.delete'), danger: true}))) return;
    if (!session.user || !session.csrf || signal?.aborted) return;
    try {
      await api(`/announcements/${encodeURIComponent(item.id)}`, {method: 'DELETE', signal});
      await saved();
    } catch (error) { if (!signal?.aborted) toastError(error); }
  };
  const disabled = !session.user || !session.csrf;
  const add = button(t('announcements.add'), {small: true, variant: 'primary', onClick: () => announcementEditor(null, context)});
  add.disabled = disabled;
  return panel(t('announcements.title'), {action: add}, items.length ? dataTable({caption: t('announcements.title'),
    columns: [t('announcements.level'), t('announcements.heading'), t('alerts.silences.starts'), t('alerts.silences.ends'), t('alerts.column.state'), t('settings.actions')],
    body: h('tbody', null, items.map((item) => {
      const edit = iconButton('edit', t('announcements.edit'), () => announcementEditor(item, context));
      const deletion = iconButton('trash', t('announcements.delete'), () => remove(item));
      edit.disabled = deletion.disabled = disabled;
      return h('tr', null, h('td', null, t(`alerts.severity.${item.level}`)), h('td', null, item.title),
        h('td', null, eventTime(item.starts_at)), h('td', null, item.ends_at ? eventTime(item.ends_at) : t('common.none')),
        h('td', null, t(item.active ? 'alerts.silences.active' : 'alerts.silences.scheduled')), h('td', {class: 'actions'}, edit, deletion));
    }))}) : h('p', {class: 'muted'}, t('announcements.empty')));
}

export function announcementsPanel(navigation) {
  const element = h('div');
  let controller;
  const active = () => controller && !controller.signal.aborted && !navigation?.aborted;
  const load = async () => {
    if (!active()) return;
    element.replaceChildren(skeleton());
    try {
      const result = await api('/announcements', {signal: controller.signal});
      if (active()) element.replaceChildren(announcementList(result.announcements, {signal: controller.signal, saved: load}));
    } catch (error) {
      if (active()) element.replaceChildren(h('div', {class: 'form-error', role: 'alert'}, errorText(error)), button(t('common.retry'), {onClick: load}));
    }
  };
  return {element, attach() { controller = new AbortController(); void load(); }, detach() { controller?.abort(); }};
}
