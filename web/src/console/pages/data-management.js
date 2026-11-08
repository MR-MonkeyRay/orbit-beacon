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

export function dataManagementPage() {
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
  const usage = h('div', {class: 'grid-2'});
  const storageNotes = h('div', {class: 'data-management-storage-notes stack'});
  const choices = h('div', {class: 'data-category-choices'});
  const status = h('p', {class: 'text-note', role: 'status'}, t('controls.loading'));
  const preview = h('p', {class: 'data-management-selection', role: 'status'});
  const cutoff = h('p', {class: 'text-note'});
  const outcome = h('div', {role: 'status', hidden: true});
  const days = select(periods.map((value) => [String(value), t('data.management.days', {count: value})]), '30', {name: 'keep_days'});
  const refresh = button(t('data.management.refresh'), {icon: 'refresh', onClick: () => load()});
  const clean = button(t('data.management.clean'), {variant: 'danger', disabled: true, onClick: () => openCleanup()});
  const element = h('div', {class: 'data-operations data-management'},
    pageHead(t('data.management.title'), refresh),
    status,
    panel(t('data.management.usage'), {}, usage, h('p', {class: 'text-note'}, t('data.management.scope')), storageNotes),
    panel(t('data.management.cleanup'), {},
      h('div', {class: 'data-management-toolbar'}, field(t('data.management.keep'), days)),
      cutoff, choices, preview,
      h('p', {class: 'text-note'}, t('data.management.protected')),
      h('div', {class: 'data-actions'}, clean), outcome));

  function updateControls() {
    const available = Boolean(controller && !controller.signal.aborted && fresh && !loading && !dialog);
    const count = snapshot?.categories.filter((item) => selected.has(item.id)).reduce((sum, item) => sum + item.eligible_rows, 0) || 0;
    clean.disabled = !available || !count;
    refresh.disabled = loading || Boolean(dialog);
    days.disabled = Boolean(dialog);
    for (const [id, checkbox] of checkboxes) {
      checkbox.checked = selected.has(id);
      checkbox.disabled = !available;
    }
    for (const control of compactButtons) control.disabled = !available;
    preview.textContent = t('data.management.selection', {count: selected.size, rows: number(count)});
  }

  function render() {
    compactButtons.length = 0;
    usage.replaceChildren(...snapshot.databases.map((database) => {
      const compact = database.driver === 'sqlite' && database.compact_supported
        ? button(t('data.management.compact'), {variant: 'danger', onClick: () => openCompact(database)}) : null;
      if (compact) {
        compact.setAttribute('aria-label', `${t('data.management.compact')}: ${domainLabel(database.domain)}`);
        compactButtons.push(compact);
      }
      return h('article', {class: 'data-plan data-management-database'},
        h('div', {class: 'data-plan-head'}, h('h3', null, domainLabel(database.domain)), h('span', {class: 'badge'}, database.driver === 'sqlite' ? 'SQLite' : 'MySQL')),
        facts([[t('data.management.allocated'), formatBytes(database.size_bytes)],
          [t('data.management.reusable'), database.reusable_bytes === null ? t('data.management.notReported') : formatBytes(database.reusable_bytes)]]),
        compact ? h('div', {class: 'data-actions'}, compact) : null);
    }));
    storageNotes.replaceChildren(...[...new Set(snapshot.databases.map((database) => database.driver))]
      .map((driver) => h('p', {class: 'text-note'}, t(driver === 'mysql' ? 'data.management.mysqlNote' : 'data.management.sqliteNote'))));
    checkboxes.clear();
    choices.replaceChildren(...snapshot.categories.filter((item) => categories.includes(item.id)).map((item) => {
      const checkbox = input({type: 'checkbox', name: 'categories', value: item.id, checked: selected.has(item.id)});
      checkbox.addEventListener('change', () => {
        if (checkbox.disabled) { checkbox.checked = selected.has(item.id); return; }
        if (checkbox.checked) selected.add(item.id); else selected.delete(item.id);
        updateControls();
      });
      checkboxes.set(item.id, checkbox);
      return h('label', {class: 'check-row data-management-category'}, checkbox,
        h('span', {class: 'data-management-category-name'}, h('strong', null, label(item.id)), h('small', {class: 'muted'}, domainLabel(item.domain))),
        h('span', {class: 'data-management-count'}, t('data.management.counts', {total: number(item.rows), eligible: number(item.eligible_rows)})));
    }));
    for (const id of selected) if (!checkboxes.has(id)) selected.delete(id);
    cutoff.replaceChildren(t('data.management.before'), ' ', h('code', null, snapshot.before));
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
      callout(t('data.management.compactWarning')), facts([[t('data.management.database'), domainLabel(database.domain)]])));
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
      const submit = button(title, {type: 'submit', variant: 'danger', className: 'solid', disabled: true});
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
          const result = await api(`/data-management/${kind}`, {method: 'POST', body, signal});
          if (signal.aborted) return;
          const rows = kind === 'cleanup' ? result.results.map((item) => h('li', null,
            t('data.management.deleted', {category: label(item.id), count: item.deleted_rows}),
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
