import {h} from '../../shared/dom.js';
import {t, getLocale} from '../../shared/i18n.js';
import {errorText, fieldErrorText} from '../../shared/errors.js';
import {api, getNavigationSignal} from '../api.js';
import {when} from '../format.js';
import {onNotificationTick} from '../notifications.js';
import {button, callout, panel} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toastError} from '../ui/feedback.js';
import {field, input, onSubmit, select, validateBytes} from '../ui/forms.js';
import {showTask} from './backup-task.js';
import {categoryNames, taskState} from './backup-task.js';

const CATEGORIES = [
  ['configuration', 'data.category.configuration', 'data.category.configurationHint'],
  ['monitoring', 'data.category.monitoring', 'data.category.monitoringHint'],
  ['events', 'data.category.events', 'data.category.eventsHint']
];
const PROVIDERS = [['s3_compatible', 'data.provider.compatible'], ['cos', 'data.provider.cos'], ['oss', 'data.provider.oss'], ['r2', 'data.provider.r2'], ['s3', 'data.provider.s3']];
const STATES = {queued: 'data.state.queued', running: 'data.state.running', succeeded: 'data.state.succeeded', failed: 'data.state.failed'};
const PHASES = {snapshot: 'data.phase.snapshot', packaging: 'data.phase.packaging', storing: 'data.phase.storing'};
const labels = categoryNames;
const stateLabel = (task) => `${taskState(task)}${task.state === 'running' && PHASES[task.phase] ? ` · ${t(PHASES[task.phase])}` : ''}`;
const taskStatus = (task) => h('span', {class: `data-state data-state-${Object.hasOwn(STATES, task.state) ? task.state : 'unknown'}`}, stateLabel(task));

function categoriesField(selected = ['configuration']) {
  const controls = CATEGORIES.map(([value]) => input({type: 'checkbox', name: 'categories', value, checked: selected.includes(value)}));
  controls[0].dataset.field = 'categories';
  const check = () => controls[0].setCustomValidity(controls.some((control) => control.checked) ? '' : t('data.category.required'));
  controls.forEach((control) => control.addEventListener('change', () => {
    check();
    if (controls[0].validity.valid) controls[0].showError?.('');
  }));
  check();
  const choices = h('div', {class: 'data-category-choices'}, CATEGORIES.map(([, title, hint], index) =>
    h('label', {class: 'data-check'}, controls[index], h('span', null, h('strong', null, t(title)), h('small', null, t(hint))))));
  return {element: field(t('data.categories'), controls[0], {inner: choices}), value: () => controls.filter((control) => control.checked).map((control) => control.value)};
}

function checkField(label, name, checked, hint) {
  const control = input({type: 'checkbox', name, checked});
  return {control, element: h('label', {class: 'data-check'}, control, h('span', null, h('strong', null, label), hint ? h('small', null, hint) : null))};
}

function timezoneText(info) {
  const offset = info.utc_offset_seconds;
  const sign = offset < 0 ? '−' : '+';
  const minutes = Math.abs(offset) / 60;
  return `${info.timezone_name} · UTC${sign}${String(Math.floor(minutes / 60)).padStart(2, '0')}:${String(Math.floor(minutes % 60)).padStart(2, '0')}`;
}

function localRun(run) {
  return h('time', {datetime: run.at, title: `UTC ${run.at}`}, run.local);
}

function planLocation(plan) {
  const destination = plan.destination;
  if (plan.kind === 'local') return destination.path;
  return `${destination.endpoint} / ${destination.bucket}${destination.prefix ? ` / ${destination.prefix}` : ''}`;
}

// tabSet owns mounting. A detached tab has no requests or summary subscription.
export function dataOperationsTab() {
  const navigation = getNavigationSignal();
  let controller = null;
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
  const exportCategories = categoriesField();
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
      button(t('data.task.view'), {onClick: () => showTask(task.id, {signal: controller?.signal})}));
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
        taskRows.replaceChildren(page.tasks.length ? taskTable(page.tasks) : h('p', {class: 'data-empty'}, t('data.tasks.empty')));
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
        planRows.replaceChildren(page.plans.length ? h('div', {class: 'data-plan-list'}, page.plans.map(planCard)) : h('p', {class: 'data-empty'}, t('data.plans.empty')));
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

  function taskTable(tasks) {
    const cell = (label, ...content) => h('td', {dataset: {label}}, content);
    return h('div', {class: 'table-wrap'}, h('table', {class: 'table data-task-table'},
      h('thead', null, h('tr', null, ['data.task.created', 'data.task.source', 'data.categories', 'data.task.state', 'data.task.details'].map((key) => h('th', {scope: 'col'}, t(key))))),
      h('tbody', null, tasks.map((task) => h('tr', null,
        cell(t('data.task.created'), when(task.created_at)),
        cell(t('data.task.source'), task.destination.plan_name || t(task.kind === 'backup_export' ? 'data.task.manual' : 'data.task.deletedPlan'),
          task.destination.actor_name ? h('small', {class: 'data-secondary'}, t('data.task.startedBy', {name: task.destination.actor_name})) : null),
        cell(t('data.categories'), labels(task.categories)),
        cell(t('data.task.state'), taskStatus(task), task.warning_code ? h('small', {class: 'data-secondary'}, t('data.task.warningDetails')) : null),
        cell(t('data.task.details'), button(t('data.task.details'), {onClick: () => showTask(task.id, {signal: controller?.signal})})))))));
  }

  function planCard(plan) {
    const last = plan.last_task;
    const next = plan.next_runs?.[0];
    return h('article', {class: 'data-plan'},
      h('div', {class: 'data-plan-head'}, h('div', null, h('h3', null, plan.name),
        h('span', {class: 'data-secondary'}, plan.kind === 'local' ? t('data.plan.localDirectory') : t(PROVIDERS.find(([id]) => id === plan.destination.provider)?.[1] || 'data.provider.unknown'))),
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
        last ? button(t('data.plan.lastDetails'), {onClick: () => showTask(last.id, {signal: controller?.signal})}) : null));
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

function backupPlanDialog(plan, {signal, onSaved}) {
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal.addEventListener('abort', abort, {once: true});
  let previewController = null;
  let previewTimer = null;
  let previewEpoch = 0;
  let controls;
  const dialog = formDialog({
    title: plan ? t('data.plan.editTitle', {name: plan.name}) : t('data.plan.addTitle'),
    intro: t('data.plan.intro'),
    submit: t(plan ? 'data.plan.save' : 'data.plan.add'),
    build: (form) => {
      form.classList.add('data-plan-form');
      const destination = plan?.destination || {};
      const name = input({name: 'name', required: true, value: plan?.name || '', autocomplete: 'off'});
      validateBytes(name, 1, 100, t('data.plan.name'));
      const kind = select([['local', t('data.plan.localDirectory')], ['object', t('data.plan.objectStorage')]], plan?.kind || 'local', {name: 'kind', disabled: Boolean(plan)});
      const enabled = checkField(t('data.plan.enabled'), 'enabled', plan?.enabled || false, t('data.plan.enabledHint'));
      const categories = categoriesField(plan?.categories);
      const cron = input({name: 'cron', value: plan?.cron || '0 3 * * *', required: true, autocomplete: 'off', spellcheck: false});
      const preview = h('div', {class: 'data-cron-preview', role: 'status'}, t('data.preview.initial'));
      const path = input({name: 'path', 'data-field': 'destination.path', value: destination.path || '', required: true, placeholder: '/var/lib/orbitbeacon/backups', autocomplete: 'off'});
      const retain = input({name: 'retain_count', type: 'number', min: 1, max: 1000, step: 1, value: plan?.retain_count ?? 7, required: true});
      const local = h('fieldset', {class: 'data-destination'}, h('legend', null, t('data.plan.localDestination')),
        field(t('data.plan.directory'), path, {hint: t('data.plan.directoryHint')}),
        field(t('data.plan.retain'), retain, {hint: t('data.plan.retainHint')}));
      const provider = select(PROVIDERS.map(([value, key]) => [value, t(key)]), destination.provider || 's3_compatible', {name: 'provider', 'data-field': 'destination.provider'});
      const endpoint = input({name: 'endpoint', 'data-field': 'destination.endpoint', value: destination.endpoint || '', type: 'url', required: true, autocomplete: 'off'});
      const region = input({name: 'region', 'data-field': 'destination.region', value: destination.region || '', required: true, autocomplete: 'off'});
      const bucket = input({name: 'bucket', 'data-field': 'destination.bucket', value: destination.bucket || '', required: true, autocomplete: 'off'});
      const prefix = input({name: 'prefix', 'data-field': 'destination.prefix', value: destination.prefix || '', autocomplete: 'off'});
      const pathStyle = checkField(t('data.plan.pathStyle'), 'path_style', destination.path_style ?? true, t('data.plan.pathStyleHint'));
      const replace = checkField(t('data.plan.replaceCredentials'), 'replace_credentials', !plan, t('data.plan.replaceCredentialsHint'));
      const keyID = input({name: 'access_key_id', 'data-field': 'credentials.access_key_id', autocomplete: 'off', spellcheck: false});
      const secret = input({name: 'secret_access_key', 'data-field': 'credentials.secret_access_key', type: 'password', autocomplete: 'new-password'});
      const token = input({name: 'session_token', 'data-field': 'credentials.session_token', type: 'password', autocomplete: 'new-password'});
      const credentialFields = h('fieldset', {class: 'data-destination'}, h('legend', null, t('data.plan.replacementCredentials')),
        field(t('data.plan.keyID'), keyID), field(t('data.plan.secretKey'), secret), field(t('data.plan.sessionToken'), token));
      const credentialsNote = h('p', {class: 'hint'}, plan ? t('data.plan.credentialsStatus', {credentials: t(plan.credentials_set ? 'data.plan.set' : 'data.plan.notSet'), token: t(plan.session_token_set ? 'data.plan.set' : 'data.plan.notSet')}) : t('data.plan.credentialsRequired'));
      const allowPrivate = checkField(t('data.plan.allowPrivate'), 'allow_private_networks', destination.allow_private_networks || false, t('data.plan.allowPrivateHint'));
      const providerHint = h('p', {class: 'hint'});
      const recommendation = button(t('data.plan.useEndpoint'), {onClick: () => {
        const suggested = suggestedEndpoint(provider.value, region.value.trim());
        if (suggested) { endpoint.value = suggested; endpoint.dispatchEvent(new Event('input', {bubbles: true})); }
      }});
      const remote = h('fieldset', {class: 'data-destination'}, h('legend', null, t('data.plan.remoteDestination')),
        field(t('data.plan.provider'), provider), field(t('data.plan.endpoint'), endpoint, {hint: t('data.plan.endpointHint')}),
        h('div', {class: 'data-endpoint-help'}, providerHint, recommendation),
        field(t('data.plan.region'), region), field(t('data.plan.bucket'), bucket), field(t('data.plan.prefix'), prefix, {hint: t('data.plan.prefixHint')}),
        pathStyle.element, credentialsNote, replace.element, credentialFields, allowPrivate.element,
        h('p', {class: 'hint'}, t('data.plan.remoteHint')));
      const updateCredentials = () => {
        const providerChanged = Boolean(plan && provider.value !== destination.provider);
        if (providerChanged) replace.control.checked = true;
        replace.control.disabled = !plan || providerChanged;
        const active = kind.value === 'object' && replace.control.checked;
        credentialFields.disabled = !active;
        credentialFields.hidden = !active;
        keyID.required = active;
        secret.required = active;
      };
      const updateProvider = (changed = false) => {
        const value = provider.value;
        if (value === 'r2') region.value = 'auto';
        region.readOnly = value === 'r2';
        pathStyle.control.disabled = value !== 's3_compatible';
        if (value !== 's3_compatible') pathStyle.control.checked = value === 'r2';
        else if (changed) pathStyle.control.checked = true;
        endpoint.placeholder = value === 'r2' ? 'https://<account>.r2.cloudflarestorage.com' : suggestedEndpoint(value, region.value.trim()) || 'https://storage.example.com';
        const suggested = suggestedEndpoint(value, region.value.trim());
        recommendation.hidden = !['cos', 'oss', 's3'].includes(value);
        recommendation.disabled = !suggested;
        providerHint.textContent = value === 'r2' ? t('data.provider.r2Hint') : suggested ? t('data.provider.recommended', {endpoint: suggested}) : t(value === 's3_compatible' ? 'data.provider.compatibleHint' : 'data.provider.regionHint');
        if (changed && plan && value !== destination.provider) replace.control.checked = true;
        updateCredentials();
      };
      const updateKind = () => {
        const isLocal = kind.value === 'local';
        local.hidden = !isLocal;
        local.disabled = !isLocal;
        remote.hidden = isLocal;
        remote.disabled = isLocal;
        updateCredentials();
      };
      const previewCron = async () => {
        const epoch = ++previewEpoch;
        previewController?.abort();
        previewController = new AbortController();
        const previewSignal = previewController.signal;
        const cancel = () => previewController?.abort();
        controller.signal.addEventListener('abort', cancel, {once: true});
        preview.textContent = t('data.preview.checking');
        try {
          const result = await api('/backup-plans/preview', {method: 'POST', body: {cron: cron.value}, signal: previewSignal});
          if (controller.signal.aborted || previewSignal.aborted || epoch !== previewEpoch) return;
          cron.showError('');
          preview.replaceChildren(h('p', null, t('data.timezone.label', {timezone: timezoneText(result)})), h('ol', null, result.next_runs.map((run) => h('li', null, localRun(run)))));
        } catch (error) {
          if (controller.signal.aborted || previewSignal.aborted || epoch !== previewEpoch) return;
          const reason = error.fields?.cron;
          if (reason) cron.showError(fieldErrorText(reason));
          preview.textContent = reason ? t('data.preview.invalid') : t('data.preview.unavailable', {error: errorText(error)});
        } finally { controller.signal.removeEventListener('abort', cancel); }
      };
      cron.addEventListener('input', () => {
        clearTimeout(previewTimer);
        previewEpoch += 1;
        previewController?.abort();
        preview.textContent = t('data.preview.waiting');
        previewTimer = setTimeout(previewCron, 500);
      });
      provider.addEventListener('change', () => updateProvider(true));
      region.addEventListener('input', () => updateProvider());
      kind.addEventListener('change', updateKind);
      replace.control.addEventListener('change', updateCredentials);
      form.append(field(t('data.plan.name'), name), field(t('data.plan.destinationKind'), kind, {hint: plan ? t('data.plan.kindHint') : undefined}), enabled.element, categories.element,
        field(t('data.plan.cron'), cron, {hint: t('data.plan.cronHint')}), preview, local, remote);
      controls = {name, kind, enabled: enabled.control, categories, cron, path, retain, provider, endpoint, region, bucket, prefix, pathStyle: pathStyle.control, replace: replace.control, keyID, secret, token, allowPrivate: allowPrivate.control};
      updateProvider();
      updateKind();
      previewTimer = setTimeout(previewCron, 0);
    },
    action: async () => {
      const c = controls;
      const body = {name: c.name.value.trim(), enabled: c.enabled.checked, categories: c.categories.value(), cron: c.cron.value.trim()};
      if (!plan) body.kind = c.kind.value;
      if (c.kind.value === 'local') {
        body.destination = {path: c.path.value.trim()};
        body.retain_count = Number(c.retain.value);
      } else {
        body.destination = {provider: c.provider.value, endpoint: c.endpoint.value.trim(), region: c.region.value.trim(), bucket: c.bucket.value.trim(), prefix: c.prefix.value.trim(), path_style: c.pathStyle.checked, allow_private_networks: c.allowPrivate.checked};
        if (!plan || c.replace.checked) body.credentials = {access_key_id: c.keyID.value, secret_access_key: c.secret.value, ...(c.token.value ? {session_token: c.token.value} : {})};
      }
      try {
        await api(plan ? `/backup-plans/${encodeURIComponent(plan.id)}` : '/backup-plans', {method: plan ? 'PATCH' : 'POST', body, signal: controller.signal});
        if (!controller.signal.aborted) await onSaved();
      } catch (error) {
        if (error.fields?.destination) error.fields = {...error.fields, [c.kind.value === 'local' ? 'destination.path' : 'destination.endpoint']: error.fields.destination};
        if (error.fields?.credentials) error.fields = {...error.fields, 'credentials.access_key_id': error.fields.credentials};
        throw error;
      }
    }
  });
  dialog.closed.finally(() => {
    clearTimeout(previewTimer);
    previewEpoch += 1;
    previewController?.abort();
    controller.abort();
    signal.removeEventListener('abort', abort);
    if (controls) { controls.keyID.value = ''; controls.secret.value = ''; controls.token.value = ''; }
    controls = null;
  });
  return dialog;
}

function suggestedEndpoint(provider, region) {
  if (!region) return '';
  if (provider === 'cos') return `https://cos.${region}.myqcloud.com`;
  if (provider === 'oss') return `https://s3.oss-${region}.aliyuncs.com`;
  if (provider === 's3') return `https://s3.${region}.amazonaws.com`;
  return '';
}
