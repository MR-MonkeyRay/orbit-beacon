import {h} from '../../shared/dom.js';
import {t, getLocale} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {api, getNavigationSignal} from '../api.js';
import {when} from '../format.js';
import {onNotificationTick} from '../notifications.js';
import {button, callout, panel} from '../ui/components.js';
import {backupCategoriesField, backupPlanDialog, backupProviders, localRun, timezoneText} from '../ui/backup-plan-dialog.js';
import {confirmDialog} from '../ui/dialogs.js';
import {toastError} from '../ui/feedback.js';
import {onSubmit} from '../ui/forms.js';
import {categoryNames, showTask, taskState} from './backup-task.js';

const STATES = {queued: 'common.queued', running: 'common.running', succeeded: 'common.succeeded', failed: 'common.failed'};
const PHASES = {snapshot: 'data.phase.snapshot', packaging: 'data.phase.packaging', storing: 'data.phase.storing'};
const labels = categoryNames;
const stateLabel = (task) => `${taskState(task)}${task.state === 'running' && PHASES[task.phase] ? ` · ${t(PHASES[task.phase])}` : ''}`;
const taskStatus = (task) => h('span', {class: `data-state data-state-${Object.hasOwn(STATES, task.state) ? task.state : 'unknown'}`}, stateLabel(task));

function planLocation(plan) {
  const destination = plan.destination;
  if (plan.kind === 'local') return destination.path;
  return `${destination.endpoint} / ${destination.bucket}${destination.prefix ? ` / ${destination.prefix}` : ''}`;
}

function taskTable(tasks, openTask) {
  const cell = (label, ...content) => h('td', {dataset: {label}}, content);
  return h('div', {class: 'table-wrap'}, h('table', {class: 'table data-task-table'},
    h('thead', null, h('tr', null, ['data.task.created', 'data.task.source', 'data.categories', 'data.task.state', 'data.task.details'].map((key) => h('th', {scope: 'col'}, t(key))))),
    h('tbody', null, tasks.map((task) => h('tr', null,
      cell(t('data.task.created'), when(task.created_at)),
      cell(t('data.task.source'), task.destination.plan_name || t(task.kind === 'backup_export' ? 'data.task.manual' : 'data.task.deletedPlan'),
        task.destination.actor_name ? h('small', {class: 'data-secondary'}, t('data.task.startedBy', {name: task.destination.actor_name})) : null),
      cell(t('data.categories'), labels(task.categories)),
      cell(t('data.task.state'), taskStatus(task), task.warning_code ? h('small', {class: 'data-secondary'}, t('data.task.warningDetails')) : null),
      cell(t('data.task.details'), button(t('data.task.details'), {onClick: () => openTask(task.id)})))))));
}

function planCard(plan, {openTask, editPlan, deletePlan}) {
  const last = plan.last_task;
  const next = plan.next_runs?.[0];
  return h('article', {class: 'data-plan'},
    h('div', {class: 'data-plan-head'}, h('div', null, h('h3', null, plan.name),
      h('span', {class: 'data-secondary'}, plan.kind === 'local' ? t('data.plan.localDirectory') : t(backupProviders.find(([id]) => id === plan.destination.provider)?.[1] || 'data.provider.unknown'))),
    h('span', {class: `data-state ${plan.enabled ? 'data-state-succeeded' : 'data-state-unknown'}`}, t(plan.enabled ? 'data.plan.enabled' : 'data.plan.disabled'))),
    h('p', {class: 'data-location'}, planLocation(plan)),
    h('dl', {class: 'data-plan-facts'},
      h('div', null, h('dt', null, t('data.plan.cron')), h('dd', null, h('code', null, plan.cron))),
      h('div', null, h('dt', null, t('data.plan.nextRun')), h('dd', null, plan.enabled && next ? localRun(next) : t('data.plan.notScheduled'))),
      h('div', null, h('dt', null, t('data.categories')), h('dd', null, labels(plan.categories))),
      h('div', null, h('dt', null, t('data.plan.retention')), h('dd', null, plan.kind === 'local' ? t('data.plan.retainArchives', {count: plan.retain_count, value: new Intl.NumberFormat(getLocale()).format(plan.retain_count)}) : t('data.plan.cloudRetention'))),
      h('div', null, h('dt', null, t('data.plan.lastTask')), h('dd', null, last ? h('span', null, taskStatus(last), ' ', when(last.created_at), last.warning_code ? t('data.task.warningSuffix') : '') : t('data.plan.noTask')))),
    h('div', {class: 'data-actions'},
      button(t('data.plan.edit'), {onClick: () => editPlan(plan)}),
      button(t('data.plan.delete'), {variant: 'danger', onClick: () => deletePlan(plan)}),
      last ? button(t('data.plan.lastDetails'), {onClick: () => openTask(last.id)}) : null));
}

// tabSet owns mounting. A detached tab has no requests or summary subscription.
export function dataOperationsTab() {
  const navigation = getNavigationSignal();
  let controller = null;
  const openTask = (id) => showTask(id, {signal: controller?.signal});
  let unsubscribe = null;
  let taskPage = 0;
  let planPage = 0;
  let taskCursors = [null];
  let planCursors = [null];
  let taskRequest = 0;
  let planRequest = 0;
  let tasksLoading = false;
  let plansLoading = false;
  let taskJSON = '';
  let planJSON = '';
  const dialogs = new Set();
  const taskRows = h('div');
  const planRows = h('div');
  const taskNote = h('p', {class: 'data-load-note', role: 'status'}, t('data.tasks.initial'));
  const planNote = h('p', {class: 'data-load-note', role: 'status'}, t('data.plans.initial'));
  const timezone = h('p', {class: 'hint'}, t('data.timezone.loading'));
  const taskPrevious = button(t('data.tasks.newer'), {disabled: true, onClick: () => { taskPage -= 1; loadTasks(); }});
  const taskNext = button(t('data.tasks.older'), {disabled: true, onClick: () => { taskPage += 1; loadTasks(); }});
  const planPrevious = button(t('data.plans.newer'), {disabled: true, onClick: () => { planPage -= 1; loadPlans(); }});
  const planNext = button(t('data.plans.older'), {disabled: true, onClick: () => { planPage += 1; loadPlans(); }});
  const taskRefresh = button(t('data.tasks.refresh'), {icon: 'refresh', onClick: () => loadTasks()});
  const planRefresh = button(t('data.plans.refresh'), {icon: 'refresh', onClick: () => loadPlans()});
  const addPlan = button(t('data.plan.add'), {icon: 'plus', variant: 'primary', onClick: () => editPlan()});
  const exportCategories = backupCategoriesField();
  const start = button(t('data.export.start'), {type: 'submit', variant: 'primary'});
  const exportError = h('div', {class: 'form-error', role: 'alert', hidden: true});
  const exportStatus = h('p', {class: 'data-load-note', role: 'status', hidden: true});
  const exportForm = h('form', {class: 'form form-wide'}, exportError, exportCategories.element,
    h('div', {class: 'form-actions'}, start), exportStatus);
  onSubmit(exportForm, start, async () => {
    const signal = controller?.signal;
    if (!signal || signal.aborted) return;
    const task = await api('/backups/exports', {method: 'POST', body: {categories: exportCategories.value()}, signal});
    if (signal.aborted) return;
    exportStatus.hidden = false;
    exportStatus.replaceChildren(h('span', null, t('data.export.started', {state: stateLabel(task)}), ' '),
      button(t('data.task.view'), {onClick: () => openTask(task.id)}));
    taskPage = 0;
    taskCursors = [null];
    await loadTasks();
  }, exportError);
  const element = h('div', {class: 'data-operations'},
    panel(t('data.export.title'), {},
      h('p', {class: 'lede'}, t('data.export.intro')),
      callout(t('data.export.warning')),
      h('p', {class: 'hint'}, t('data.export.hint')),
      exportForm),
    panel(t('data.plans.title'), {action: h('div', {class: 'data-actions'}, planRefresh, addPlan)},
      h('p', {class: 'lede'}, t('data.plans.intro')),
      timezone, planNote, planRows,
      h('div', {class: 'data-pagination'}, planPrevious, planNext)),
    panel(t('data.tasks.title'), {action: taskRefresh},
      h('p', {class: 'lede'}, t('data.tasks.intro')),
      taskNote, taskRows, h('div', {class: 'data-pagination'}, taskPrevious, taskNext)));

  function requestPath(path, cursor) {
    return `${path}?limit=20${cursor ? `&before=${encodeURIComponent(cursor)}` : ''}`;
  }

  async function loadTasks({poll = false} = {}) {
    const signal = controller?.signal;
    if (!signal || signal.aborted || tasksLoading || (poll && taskRows.contains(document.activeElement))) return;
    const epoch = ++taskRequest;
    const pageIndex = taskPage;
    tasksLoading = true;
    taskRefresh.disabled = true;
    taskPrevious.disabled = true;
    taskNext.disabled = true;
    try {
      const page = await api(requestPath('/tasks', taskCursors[pageIndex]), {signal});
      if (signal.aborted || epoch !== taskRequest) return;
      const json = JSON.stringify(page.tasks);
      if (json !== taskJSON) {
        taskRows.replaceChildren(page.tasks.length ? taskTable(page.tasks, openTask) : h('p', {class: 'data-empty'}, t('data.tasks.empty')));
        taskJSON = json;
      }
      taskNote.textContent = t('data.tasks.current');
      taskNote.classList.remove('data-stale');
      taskCursors[pageIndex + 1] = page.has_more ? page.next_cursor : null;
      taskPrevious.disabled = pageIndex === 0;
      taskNext.disabled = !page.has_more || !page.next_cursor;
    } catch (error) {
      if (signal.aborted || epoch !== taskRequest) return;
      taskNote.textContent = t(taskJSON ? 'data.tasks.refreshFailedStale' : 'data.tasks.refreshFailed', {error: errorText(error)});
      taskNote.classList.add('data-stale');
      taskPrevious.disabled = pageIndex === 0;
      taskNext.disabled = !taskCursors[pageIndex + 1];
    } finally {
      if (epoch === taskRequest) { tasksLoading = false; taskRefresh.disabled = false; }
    }
  }

  async function loadPlans() {
    const signal = controller?.signal;
    if (!signal || signal.aborted || plansLoading) return;
    const epoch = ++planRequest;
    const pageIndex = planPage;
    plansLoading = true;
    planRefresh.disabled = true;
    planPrevious.disabled = true;
    planNext.disabled = true;
    try {
      const page = await api(requestPath('/backup-plans', planCursors[pageIndex]), {signal});
      if (signal.aborted || epoch !== planRequest) return;
      timezone.textContent = t('data.timezone.description', {timezone: timezoneText(page)});
      const json = JSON.stringify(page.plans);
      if (json !== planJSON) {
        planRows.replaceChildren(page.plans.length ? h('div', {class: 'data-plan-list'}, page.plans.map((plan) => planCard(plan, {openTask, editPlan, deletePlan}))) : h('p', {class: 'data-empty'}, t('data.plans.empty')));
        planJSON = json;
      }
      planNote.textContent = '';
      planNote.classList.remove('data-stale');
      planCursors[pageIndex + 1] = page.has_more ? page.next_cursor : null;
      planPrevious.disabled = pageIndex === 0;
      planNext.disabled = !page.has_more || !page.next_cursor;
    } catch (error) {
      if (signal.aborted || epoch !== planRequest) return;
      planNote.textContent = t(planJSON ? 'data.plans.refreshFailedStale' : 'data.plans.refreshFailed', {error: errorText(error)});
      planNote.classList.add('data-stale');
      planPrevious.disabled = pageIndex === 0;
      planNext.disabled = !planCursors[pageIndex + 1];
    } finally {
      if (epoch === planRequest) { plansLoading = false; planRefresh.disabled = false; }
    }
  }

  async function deletePlan(plan) {
    const signal = controller?.signal;
    if (!signal || signal.aborted) return;
    const confirmed = await confirmDialog({title: t('data.plan.deleteTitle', {name: plan.name}), message: t('data.plan.deleteMessage'), confirm: t('data.plan.deleteConfirm'), danger: true});
    if (!confirmed || signal.aborted) return;
    try {
      await api(`/backup-plans/${encodeURIComponent(plan.id)}`, {method: 'DELETE', signal});
      if (!signal.aborted) { planPage = 0; planCursors = [null]; await loadPlans(); }
    } catch (error) {
      if (signal.aborted) return;
      toastError(error);
    }
  }

  function editPlan(plan) {
    const signal = controller?.signal;
    if (!signal || signal.aborted) return;
    const dialog = backupPlanDialog(plan, {signal, onSaved: async () => {
      if (signal.aborted) return;
      planPage = 0;
      planCursors = [null];
      await loadPlans();
    }});
    dialogs.add(dialog);
    dialog.closed.finally(() => dialogs.delete(dialog));
  }

  function detach() {
    unsubscribe?.();
    unsubscribe = null;
    controller?.abort();
    controller = null;
    taskRequest += 1;
    planRequest += 1;
    tasksLoading = false;
    plansLoading = false;
    for (const dialog of dialogs) dialog.close('detach');
    dialogs.clear();
  }

  function attach() {
    if (controller || navigation?.aborted) return;
    controller = new AbortController();
    loadPlans();
    loadTasks();
    unsubscribe = onNotificationTick(() => loadTasks({poll: true}));
  }
  navigation?.addEventListener('abort', detach, {once: true});
  return {element, attach, detach};
}
