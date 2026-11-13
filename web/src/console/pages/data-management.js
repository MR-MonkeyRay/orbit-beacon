import {h} from '../../shared/dom.js';
import {errorText} from '../../shared/errors.js';
import {api, getNavigationSignal} from '../api.js';
import {getLocale, t} from '../i18n.js';
import {formatBytes} from '../format.js';
import {busy, button, callout, facts, pageHead, panel} from '../ui/components.js';
import {modal} from '../ui/dialogs.js';
import {field, input, select} from '../ui/forms.js';
import {matchesConfirmation} from '../ui/confirmation-match.js';

const categories = ['metric_history', 'probe_history', 'availability_history', 'resolved_alerts', 'notification_history', 'delivery_history', 'audit_events'];
const periods = [7, 30, 90, 180, 365];
const label = (id) => t(`data.management.category.${id}`);
const domainLabel = (domain) => t(`data.management.domain.${domain}`);
const number = (value) => new Intl.NumberFormat(getLocale()).format(value);
// 文案承诺展示 UTC，时间必须按 UTC 格式化（node-traffic 同样先例）。
const cutoffFormat = new Intl.DateTimeFormat(getLocale(), {dateStyle: 'medium', timeStyle: 'short', timeZone: 'UTC'});

export function dataManagementPage({embedded = false} = {}) {
  const navigation = getNavigationSignal();
  const selected = new Set();
  let controller = null;
  let request = null;
  let snapshot = null;
  let fresh = false;
  let loading = false;
  let dialog = null;
  let epoch = 0;
  const checkboxes = new Map();
  const compactButtons = [];
  const usage = h('div', {class: 'grid-2 data-databases'});
  const choices = h('div', {class: 'data-cleanup-rows'});
  const status = h('p', {class: 'text-note', role: 'status'}, t('controls.loading'));
  const preview = h('p', {class: 'data-management-selection', role: 'status'});
  const cutoff = h('p', {class: 'text-note data-cutoff'});
  const outcome = h('div', {role: 'status', hidden: true});
  const days = select(periods.map((value) => [String(value), t('data.management.days', {count: value})]), '30', {name: 'keep_days'});
  const refresh = button(t('data.management.refresh'), {small: true, icon: 'refresh', onClick: () => load()});
  const clean = button(t('data.management.clean'), {variant: 'danger', disabled: true, onClick: () => openCleanup()});
  // 类别列表按表格排版：勾选、名称、所属数据库、总行数、可清理行数；整行可点。
  const columns = h('div', {class: 'data-cleanup-head', 'aria-hidden': 'true'}, h('span'), h('span', null, t('data.management.category')),
    h('span', null, t('data.management.database')), h('span', null, t('data.management.rows')), h('span', null, t('data.management.eligible')));
  const element = h('div', {class: 'data-operations data-management stack'},
    embedded ? null : pageHead(t('data.management.title')),
    status,
    panel(t('data.management.usage'), {action: refresh}, usage),
    panel(t('data.management.cleanup'), {},
      h('div', {class: 'data-management-toolbar'}, field(t('data.management.keep'), days), cutoff),
      h('div', {class: 'data-cleanup-list'}, columns, choices),
      h('div', {class: 'data-cleanup-foot'}, preview, clean), outcome));

  function updateControls() {
    const available = Boolean(controller && !controller.signal.aborted && fresh && !loading && !dialog);
    const count = snapshot?.categories.filter((item) => selected.has(item.id)).reduce((sum, item) => sum + item.eligible_rows, 0) || 0;
    clean.disabled = !available || !count;
    refresh.disabled = loading || Boolean(dialog);
    days.disabled = Boolean(dialog);
    for (const [id, checkbox] of checkboxes) {
      checkbox.checked = selected.has(id);
      checkbox.disabled = !available || !snapshot.categories.find((item) => item.id === id)?.eligible_rows;
    }
    for (const control of compactButtons) control.disabled = !available;
    // count 传原始数值用于复数选择，rows 复用格式化数值展示。
    preview.textContent = `${t('data.management.selectionCategories', {count: selected.size, rows: count})} · ${t('data.management.selectionRows', {count, rows: number(count)})}`;
  }

  function render() {
    compactButtons.length = 0;
    usage.replaceChildren(...snapshot.databases.map((database) => {
      const compact = database.compact_supported
        ? button(t('data.management.compact'), {onClick: () => openCompact(database)}) : null;
      if (compact) {
        compact.setAttribute('aria-label', `${t('data.management.compact')}: ${domainLabel(database.domain)}`);
        compactButtons.push(compact);
      }
      const reusable = database.reusable_bytes === null ? t('data.management.notReported') : formatBytes(database.reusable_bytes);
      return h('article', {class: 'data-management-database'},
        h('div', {class: 'data-database-head'}, h('h3', null, domainLabel(database.domain)), h('span', {class: 'badge'}, database.driver === 'sqlite' ? 'SQLite' : 'MySQL')),
        h('dl', {class: 'data-database-stats'},
          h('div', null, h('dt', null, t('data.management.allocated')), h('dd', null, formatBytes(database.size_bytes))),
          h('div', null, h('dt', null, t('data.management.reusable')), h('dd', null, reusable))),
        compact);
    }));
    checkboxes.clear();
    choices.replaceChildren(...snapshot.categories.filter((item) => categories.includes(item.id))
      .sort((a, b) => Number(!a.eligible_rows) - Number(!b.eligible_rows)).map((item) => {
      if (!item.eligible_rows) selected.delete(item.id);
      const checkbox = input({type: 'checkbox', name: 'categories', value: item.id, checked: selected.has(item.id)});
      checkbox.addEventListener('change', () => {
        if (checkbox.disabled) { checkbox.checked = selected.has(item.id); return; }
        if (checkbox.checked) selected.add(item.id); else selected.delete(item.id);
        updateControls();
      });
      checkboxes.set(item.id, checkbox);
      return h('label', {class: 'data-management-category'}, checkbox,
        h('span', {class: 'data-management-category-name'}, label(item.id)),
        h('span', {class: 'muted', dataset: {label: t('data.management.database')}}, domainLabel(item.domain)),
        h('span', {class: 'data-management-count', dataset: {label: t('data.management.rows')}}, number(item.rows)),
        h('span', {class: 'data-management-count', dataset: {label: t('data.management.eligible')}}, number(item.eligible_rows)));
    }));
    for (const id of selected) if (!checkboxes.has(id)) selected.delete(id);
    const cutoffDate = new Date(snapshot.before);
    cutoff.replaceChildren(t('data.management.before'), ' ', Number.isFinite(cutoffDate.getTime())
      ? h('time', {datetime: snapshot.before, title: `UTC ${snapshot.before}`}, cutoffFormat.format(cutoffDate))
      : h('code', null, snapshot.before));
  }

  async function load() {
    if (!controller || controller.signal.aborted) return;
    request?.abort();
    request = new AbortController();
    const signal = AbortSignal.any([controller.signal, request.signal]);
    const current = ++epoch;
    const keepDays = days.value;
    loading = true;
    fresh = false;
    status.textContent = t(snapshot ? 'data.management.staleLoading' : 'controls.loading');
    status.classList.toggle('data-stale', Boolean(snapshot));
    updateControls();
    const previous = snapshot;
    try {
      const value = await api(`/data-management?keep_days=${keepDays}`, {signal});
      if (signal.aborted || current !== epoch) return;
      snapshot = value;
      render();
      fresh = true;
      status.textContent = '';
      status.classList.remove('data-stale');
    } catch (error) {
      if (signal.aborted || current !== epoch) return;
      fresh = false;
      snapshot = previous;
      if (snapshot) render();
      else {
        usage.replaceChildren();
        choices.replaceChildren();
        checkboxes.clear();
        compactButtons.length = 0;
        cutoff.replaceChildren();
      }
      status.textContent = t(snapshot ? 'data.management.refreshFailedStale' : 'data.management.refreshFailed', {error: errorText(error)});
      status.classList.add('data-stale');
    } finally {
      if (current === epoch) { loading = false; updateControls(); }
    }
  }

  function openCleanup() {
    if (clean.disabled) return;
    const ids = snapshot.categories.filter((item) => selected.has(item.id)).map((item) => item.id);
    const body = {categories: ids, keep_days: Number(days.value), before: snapshot.before, confirmation: 'CLEAN'};
    openOperation('cleanup', body, h('div', {class: 'stack'},
      callout(t('data.management.cleanWarning')),
      h('ul', null, ids.map((id) => h('li', null, label(id)))),
      facts([[t('data.management.keep'), t('data.management.days', {count: body.keep_days})], [t('data.management.before'), h('code', null, body.before)]])));
  }

  function openCompact(database) {
    if (!fresh || loading || dialog || !controller || controller.signal.aborted) return;
    openOperation('compact', {domain: database.domain, confirmation: 'COMPACT'}, h('div', {class: 'stack'},
      callout(t(database.driver === 'mysql' ? 'data.management.mysqlCompactWarning' : 'data.management.compactWarning')),
      facts([[t('data.management.database'), domainLabel(database.domain)]])));
  }

  function openOperation(kind, body, summary) {
    const title = t(kind === 'cleanup' ? 'data.management.clean' : 'data.management.compact');
    const expected = body.confirmation;
    let submitted = false;
    let posting = false;
    const current = controller;
    const opened = modal(title, (close, dialogSignal) => {
      const content = h('div', {class: 'data-management-dialog'});
      const entry = input({name: 'confirmation', required: true, autocomplete: 'off', spellcheck: false, dataset: {transient: '', autofocus: ''}});
      const submit = button(title, {type: 'submit', variant: kind === 'cleanup' ? 'danger' : 'primary', className: kind === 'cleanup' ? 'solid' : '', disabled: true});
      const errorBox = h('div', {class: 'form-error', role: 'alert', hidden: true});
      const cancel = button(t('controls.cancel'), {onClick: () => close()});
      const update = () => { submit.disabled = submitted || posting || !matchesConfirmation(entry.value, expected); };
      entry.addEventListener('input', update);
      const form = h('form', {class: 'form', onsubmit: async (event) => {
        event.preventDefault();
        if (submitted || posting || current.signal.aborted || dialogSignal.aborted || !matchesConfirmation(entry.value, expected)) return;
        submitted = true;
        posting = true;
        cancel.disabled = true;
        busy(submit, true);
        const signal = AbortSignal.any([current.signal, dialogSignal]);
        try {
          const result = await api(`/data-management/${kind}`, {method: 'POST', body, signal, timeout: kind === 'compact' ? 31 * 60 * 1000 : undefined});
          if (signal.aborted) return;
          const rows = kind === 'cleanup' ? result.results.map((item) => h('li', null,
            t('data.management.deleted', {category: label(item.id), count: item.deleted_rows, rows: number(item.deleted_rows)}),
            item.has_more ? h('p', {class: 'text-note'}, t('data.management.more')) : null)) : [];
          const message = t(kind === 'cleanup' ? 'data.management.cleaned' : 'data.management.compacted');
          content.replaceChildren(h('div', {class: 'dialog-body stack', role: 'status'}, h('p', null, message),
            rows.length ? h('ul', null, rows) : null), h('div', {class: 'dialog-foot'}, button(t('controls.dialog.close'), {onClick: () => close('done')})));
          outcome.hidden = false;
          outcome.textContent = message;
          await load();
        } catch (error) {
          if (signal.aborted) return;
          fresh = false;
          status.classList.add('data-stale');
          status.textContent = t('data.management.partial');
          const unknown = !error.status || error.code === 'storage_commit_unknown' || error.code === 'unreadable_response';
          errorBox.textContent = `${unknown ? t('data.management.unknown') : errorText(error)} ${t('data.management.partial')}`;
          errorBox.hidden = false;
          cancel.textContent = t('controls.dialog.close');
          updateControls();
        } finally {
          posting = false;
          busy(submit, false);
          cancel.disabled = false;
          update();
        }
      }}, h('div', {class: 'dialog-body stack'}, summary, field(t('controls.confirmation.type', {expected}), entry), errorBox),
      h('div', {class: 'dialog-foot'}, cancel, submit));
      content.append(form);
      return content;
    });
    dialog = opened;
    updateControls();
    opened.closed.then(() => { if (dialog === opened) { dialog = null; updateControls(); } });
  }

  function detach() {
    controller?.abort();
    controller = null;
    request?.abort();
    request = null;
    epoch += 1;
    loading = false;
    fresh = false;
    dialog?.close('detach');
    dialog = null;
    updateControls();
  }
  days.addEventListener('change', () => { if (!days.disabled) load(); });
  navigation?.addEventListener('abort', detach, {once: true});
  updateControls();
  return {element, attach() {
    if (controller || navigation?.aborted) return;
    controller = new AbortController();
    load();
  }, detach};
}
