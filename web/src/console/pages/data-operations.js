import {h, icon} from '../../shared/dom.js';
import {t, tParts, getLocale} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {api, getNavigationSignal} from '../api.js';
import {when} from '../format.js';
import {onNotificationTick} from '../notifications.js';
import {button, dataTable, iconButton, panel} from '../ui/components.js';
import {backupCategoriesField, backupPlanDialog, backupProviders, localRun, timezoneText} from '../ui/backup-plan-dialog.js';
import {confirmDialog} from '../ui/dialogs.js';
import {toastError} from '../ui/feedback.js';
import {onSubmit} from '../ui/forms.js';
import {cursorPath} from '../ui/pager.js';
import {categoryNames, knownTaskState, taskState} from '../lib/task-labels.js';
import {showTask} from './backup-task.js';

const PHASES = {snapshot: 'data.phase.snapshot', packaging: 'data.phase.packaging', storing: 'data.phase.storing'};
const stateLabel = (task) => task.state === 'running' && PHASES[task.phase]
  ? t('data.task.statePhase', {state: taskState(task), phase: t(PHASES[task.phase])}) : taskState(task);
const taskStatus = (task) => h('span', {class: `data-state data-state-${knownTaskState(task.state) ? task.state : 'unknown'}`}, stateLabel(task));

function planLocation(plan) {
  const destination = plan.destination;
  if (plan.kind === 'local') return destination.path;
  return `${destination.endpoint} / ${destination.bucket}${destination.prefix ? ` / ${destination.prefix}` : ''}`;
}

function taskTable(tasks, openTask) {
  const cell = (label, ...content) => h('td', {dataset: {label}}, content);
  const columns = ['data.task.created', 'data.task.source', 'data.categories', 'data.task.state'].map((key) => t(key));
  return dataTable({className: 'data-task-table', columns: [...columns, h('span', {class: 'sr'}, t('data.task.details'))],
    body: h('tbody', null, tasks.map((task) => h('tr', null,
      cell(t('data.task.created'), when(task.created_at)),
      cell(t('data.task.source'), task.destination.plan_name || t(task.kind === 'backup_export' ? 'data.task.manual' : 'data.task.deletedPlan'),
        task.destination.actor_name ? h('small', {class: 'muted'}, t('data.task.startedBy', {name: task.destination.actor_name})) : null),
      cell(t('data.categories'), categoryNames(task.categories)),
      cell(t('data.task.state'), taskStatus(task), task.warning_code ? h('small', {class: 'muted'}, t('data.task.warningDetails')) : null),
      h('td', {class: 'actions'}, button(t('data.task.view'), {small: true, onClick: () => openTask(task.id)})))))});
}

// 卡片：名称与状态、目标位置、时间表与下次运行、类别、保留、最近一次运行（可点开任务）。
function planCard(plan, {openTask, editPlan, deletePlan, zone}) {
  const last = plan.last_task;
  const next = plan.next_runs?.[0];
  const where = plan.kind === 'local' ? t('data.plan.localDirectory') : t(backupProviders.find(([id]) => id === plan.destination.provider)?.[1] || 'data.provider.unknown');
  const location = planLocation(plan);
  const fact = (label, ...value) => h('div', null, h('dt', null, label), h('dd', null, value));
  const edit = iconButton('edit', t('data.plan.edit'), () => editPlan(plan));
  const remove = iconButton('trash', t('data.plan.delete'), () => deletePlan(plan));
  edit.setAttribute('aria-label', `${t('data.plan.edit')}: ${plan.name}`);
  remove.setAttribute('aria-label', `${t('data.plan.delete')}: ${plan.name}`);
  remove.classList.add('icon-danger');
  const schedule = plan.enabled ? h('span', {class: 'muted'}, ' · ', next ? localRun(next, zone) : t('data.plan.notScheduled')) : null;
  return h('article', {class: 'data-plan'},
    h('div', {class: 'data-plan-head'},
      h('h3', null, plan.name),
      h('span', {class: `badge ${plan.enabled ? 'online' : 'disabled'}`}, t(plan.enabled ? 'data.plan.enabled' : 'data.plan.disabled')),
      h('div', {class: 'data-plan-actions'}, edit, remove)),
    h('p', {class: 'data-location', title: location}, h('span', null, where), h('code', null, location)),
    h('dl', {class: 'data-plan-facts'},
      fact(t('data.plan.schedule'), h('code', null, plan.cron), schedule),
      fact(t('data.categories'), categoryNames(plan.categories)),
      fact(t('data.plan.retention'), plan.kind === 'local' ? t('data.plan.retainArchives', {count: plan.retain_count, value: new Intl.NumberFormat(getLocale()).format(plan.retain_count)}) : t('data.plan.cloudRetention')),
      fact(t('data.plan.lastTask'), last
        ? h('button', {type: 'button', class: 'link-button', onclick: () => openTask(last.id)}, tParts(last.warning_code ? 'data.plan.lastTaskWarning' : 'data.plan.lastTaskSummary', {state: taskStatus(last), time: when(last.created_at)}))
        : t('data.plan.noTask'))));
}

// tabSet 负责挂载。卸载的标签页没有请求和摘要订阅。
export function dataOperationsTab() {
  const navigation = getNavigationSignal();
  let controller = null;
  let zone = '';
  const openTask = (id) => showTask(id, {signal: controller?.signal});
  let unsubscribe = null;
  const taskPager = {page: 0, cursors: [null], request: 0, loading: false, json: ''};
  const planPager = {page: 0, cursors: [null], request: 0, loading: false, json: ''};
  const dialogs = new Set();
  const taskRows = h('div');
  const planRows = h('div');
  const taskNote = h('p', {class: 'text-note', role: 'status'}, t('controls.loading'));
  const planNote = h('p', {class: 'text-note', role: 'status'}, t('controls.loading'));
  const taskPrevious = button(t('data.tasks.newer'), {small: true, disabled: true, onClick: () => { taskPager.page -= 1; loadTasks(); }});
  const taskNext = button(t('data.tasks.older'), {small: true, disabled: true, onClick: () => { taskPager.page += 1; loadTasks(); }});
  const planPrevious = button(t('data.plans.newer'), {small: true, disabled: true, onClick: () => { planPager.page -= 1; loadPlans(); }});
  const planNext = button(t('data.plans.older'), {small: true, disabled: true, onClick: () => { planPager.page += 1; loadPlans(); }});
  const taskPagination = h('div', {class: 'data-pagination', hidden: true}, taskPrevious, taskNext);
  const planPagination = h('div', {class: 'data-pagination', hidden: true}, planPrevious, planNext);
  const taskRefresh = iconButton('refresh', t('data.tasks.refresh'), () => loadTasks());
  const planRefresh = iconButton('refresh', t('data.plans.refresh'), () => loadPlans());
  const addPlan = button(t('data.plan.add'), {icon: 'plus', variant: 'primary', onClick: () => editPlan()});
  const exportCategories = backupCategoriesField();
  const start = button(t('data.export.start'), {type: 'submit', variant: 'primary'});
  const exportError = h('div', {class: 'form-error', role: 'alert', hidden: true});
  const exportStatus = h('p', {class: 'text-note data-export-status', role: 'status', hidden: true});
  const exportForm = h('form', {class: 'form form-wide data-export'}, exportError,
    h('div', {class: 'data-export-row'}, exportCategories.element, start),
    h('p', {class: 'data-export-note'}, icon('alert'), h('span', null, t('data.export.warning'))),
    exportStatus);
  onSubmit(exportForm, start, async () => {
    const signal = controller?.signal;
    if (!signal || signal.aborted) return;
    const task = await api('/backups/exports', {method: 'POST', body: {categories: exportCategories.value()}, signal});
    if (signal.aborted) return;
    exportStatus.hidden = false;
    exportStatus.replaceChildren(h('span', null, t('data.export.started', {state: stateLabel(task)}), ' '),
      button(t('data.task.view'), {small: true, onClick: () => openTask(task.id)}));
    taskPager.page = 0;
    taskPager.cursors = [null];
    await loadTasks();
  }, exportError);
  const element = h('div', {class: 'data-operations stack'},
    panel(t('data.export.title'), {}, exportForm),
    panel(t('data.plans.title'), {action: h('div', {class: 'head-actions'}, planRefresh, addPlan)},
      planNote, planRows, planPagination),
    panel(t('data.tasks.title'), {action: taskRefresh},
      taskNote, taskRows, taskPagination));

  const taskView = {pager: taskPager, path: '/tasks', rows: taskRows, note: taskNote, previous: taskPrevious, next: taskNext, pagination: taskPagination, refresh: taskRefresh,
    values: (page) => page.tasks,
    render: (page) => page.tasks.length ? taskTable(page.tasks, openTask) : h('p', {class: 'empty'}, t('data.tasks.empty')),
    success: () => { taskNote.textContent = ''; },
    failure: (stale, error) => t(stale ? 'data.tasks.refreshFailedStale' : 'data.tasks.refreshFailed', {error: errorText(error)})};
  const planView = {pager: planPager, path: '/backup-plans', rows: planRows, note: planNote, previous: planPrevious, next: planNext, pagination: planPagination, refresh: planRefresh,
    values: (page) => [page.plans, page.timezone_name, page.utc_offset_seconds],
    render: (page) => {
      zone = page.timezone_name ? timezoneText(page) : '';
      return page.plans.length
        ? h('div', {class: 'data-plan-list'}, page.plans.map((plan) => planCard(plan, {openTask, editPlan, deletePlan, zone})))
        : h('p', {class: 'empty'}, t('data.plans.empty'));
    },
    success: () => { planNote.textContent = ''; },
    failure: (stale, error) => t(stale ? 'data.plans.refreshFailedStale' : 'data.plans.refreshFailed', {error: errorText(error)})};

  async function loadPage(view, {poll = false} = {}) {
    const signal = controller?.signal;
    const {pager} = view;
    if (!signal || signal.aborted || pager.loading || (poll && view.rows.contains(document.activeElement))) return;
    const epoch = ++pager.request;
    const pageIndex = pager.page;
    pager.loading = true;
    view.refresh.disabled = true;
    view.previous.disabled = true;
    view.next.disabled = true;
    try {
      const current = pager.cursors[pageIndex];
      const page = await api(cursorPath(view.path, 20, current), {signal});
      if (signal.aborted || epoch !== pager.request) return;
      const cursor = page.has_more ? page.next_cursor : null;
      const json = JSON.stringify(view.values(page));
      if (json !== pager.json) {
        view.rows.replaceChildren(view.render(page));
        pager.json = json;
      }
      view.success(page);
      view.note.classList.remove('data-stale');
      pager.cursors[pageIndex + 1] = cursor;
      view.previous.disabled = pageIndex === 0;
      view.next.disabled = !cursor;
    } catch (error) {
      if (signal.aborted || epoch !== pager.request) return;
      view.note.textContent = view.failure(Boolean(pager.json), error);
      view.note.classList.add('data-stale');
      view.previous.disabled = pageIndex === 0;
      view.next.disabled = !pager.cursors[pageIndex + 1];
    } finally {
      if (epoch === pager.request) {
        pager.loading = false;
        view.refresh.disabled = false;
        // 只有一页时不显示翻页按钮；加载中保持原状，避免闪烁。
        view.pagination.hidden = view.previous.disabled && view.next.disabled;
      }
    }
  }

  const loadTasks = (options) => loadPage(taskView, options);
  const loadPlans = () => loadPage(planView);

  async function deletePlan(plan) {
    const signal = controller?.signal;
    if (!signal || signal.aborted) return;
    const confirmed = await confirmDialog({title: t('data.plan.deleteTitle', {name: plan.name}), message: t('data.plan.deleteMessage'), confirm: t('data.plan.deleteConfirm'), danger: true});
    if (!confirmed || signal.aborted) return;
    try {
      await api(`/backup-plans/${encodeURIComponent(plan.id)}`, {method: 'DELETE', signal});
      if (!signal.aborted) { planPager.page = 0; planPager.cursors = [null]; await loadPlans(); }
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
      planPager.page = 0;
      planPager.cursors = [null];
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
    taskPager.request += 1;
    planPager.request += 1;
    taskPager.loading = false;
    planPager.loading = false;
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
