import {h} from '../../shared/dom.js';
import {api, getNavigationSignal, isAdmin} from '../api.js';
import {when} from '../format.js';
import {onNotificationTick} from '../notifications.js';
import {button, callout, panel} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toastError} from '../ui/feedback.js';
import {field, input, onSubmit, select, validateBytes} from '../ui/forms.js';
import {showTask} from './backup-task.js';

const CATEGORIES = [
  ['configuration', 'Configuration', 'Users, Agent identities, rules, plans and stored credentials.'],
  ['monitoring', 'Monitoring', 'Recorded metrics, traffic and probe history.'],
  ['events', 'Events', 'Audit, alert and completed task history; no historical backup passwords.']
];
const PROVIDERS = [['s3_compatible', 'S3 compatible'], ['cos', 'Tencent COS'], ['oss', 'Alibaba OSS'], ['r2', 'Cloudflare R2'], ['s3', 'AWS S3']];
const STATES = {queued: 'Queued', running: 'Running', succeeded: 'Succeeded', failed: 'Failed'};
const PHASES = {snapshot: 'Snapshot', packaging: 'Compressing and encrypting', storing: 'Storing'};
const labels = (categories) => categories.map((value) => CATEGORIES.find(([id]) => id === value)?.[1] || value).join(', ');
const stateLabel = (task) => `${STATES[task.state] || 'Unknown state'}${task.state === 'running' && PHASES[task.phase] ? ` · ${PHASES[task.phase]}` : ''}`;
const taskStatus = (task) => h('span', {class: `data-state data-state-${Object.hasOwn(STATES, task.state) ? task.state : 'unknown'}`}, stateLabel(task));

function categoriesField(selected = ['configuration']) {
  const controls = CATEGORIES.map(([value]) => input({type: 'checkbox', name: 'categories', value, checked: selected.includes(value)}));
  controls[0].dataset.field = 'categories';
  const check = () => controls[0].setCustomValidity(controls.some((control) => control.checked) ? '' : 'Select at least one data category.');
  controls.forEach((control) => control.addEventListener('change', () => {
    check();
    if (controls[0].validity.valid) controls[0].showError?.('');
  }));
  check();
  const choices = h('div', {class: 'data-category-choices'}, CATEGORIES.map(([, title, hint], index) =>
    h('label', {class: 'data-check'}, controls[index], h('span', null, h('strong', null, title), h('small', null, hint)))));
  return {element: field('Data categories', controls[0], {inner: choices}), value: () => controls.filter((control) => control.checked).map((control) => control.value)};
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
  if (!isAdmin()) return {element: callout('Access denied. Data operations are available to administrators only.'), attach() {}, detach() {}};
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
  const taskNote = h('p', {class: 'data-load-note', role: 'status'}, 'Task history loads when this tab is open.');
  const planNote = h('p', {class: 'data-load-note', role: 'status'}, 'Backup plans load when this tab is open.');
  const timezone = h('p', {class: 'hint'}, 'Server timezone is loading.');
  const taskPrevious = button('Newer tasks', {disabled: true, onClick: () => { taskPage -= 1; loadTasks(); }});
  const taskNext = button('Older tasks', {disabled: true, onClick: () => { taskPage += 1; loadTasks(); }});
  const planPrevious = button('Newer plans', {disabled: true, onClick: () => { planPage -= 1; loadPlans(); }});
  const planNext = button('Older plans', {disabled: true, onClick: () => { planPage += 1; loadPlans(); }});
  const taskRefresh = button('Refresh tasks', {icon: 'refresh', onClick: () => loadTasks()});
  const planRefresh = button('Refresh plans', {icon: 'refresh', onClick: () => loadPlans()});
  const addPlan = button('Add plan', {icon: 'plus', variant: 'primary', onClick: () => editPlan()});
  const exportCategories = categoriesField();
  const start = button('Start export', {type: 'submit', variant: 'primary'});
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
    exportStatus.replaceChildren(h('span', null, `Export ${stateLabel(task).toLowerCase()}. You can leave this page; work continues on the server. Open Notifications for the result. `),
      button('View task', {onClick: () => showTask(task.id, {signal: controller?.signal})}));
    taskPage = 0;
    taskCursors = [null];
    await loadTasks();
  }, exportError);
  const element = h('div', {class: 'data-operations'},
    panel('Export', {},
      h('p', {class: 'lede'}, 'Create an encrypted .tar.gz.age archive in the background. Each category is independent; select at least one.'),
      callout('Configuration includes password hashes, persistent Agent identities and stored credentials. Keep archives private. Every archive has its own password: save it separately from task details. If the password is lost or its 90-day record expires, the archive cannot be recovered.'),
      h('p', {class: 'hint'}, 'Successful manual exports are available to download for 24 hours. Nothing downloads automatically. Only committed data is included; main and metrics snapshots are individually consistent, not one cross-database instant.'),
      exportForm),
    panel('Backup plans', {action: h('div', {class: 'data-actions'}, planRefresh, addPlan)},
      h('p', {class: 'lede'}, 'Run encrypted backups on the Server’s five-field Cron schedule. Plans have one local or object-storage destination. Restored plans are disabled.'),
      timezone, planNote, planRows,
      h('div', {class: 'data-pagination'}, planPrevious, planNext)),
    panel('Task history', {action: taskRefresh},
      h('p', {class: 'lede'}, 'Shared administrator history, retained for 90 days. Scheduled backups stay at their saved destination; only manual exports offer a browser download.'),
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
        taskRows.replaceChildren(page.tasks.length ? taskTable(page.tasks) : h('p', {class: 'data-empty'}, 'No tasks on this page.'));
        taskJSON = json;
      }
      taskNote.textContent = 'Up to date. Refreshes while this tab is open.';
      taskNote.classList.remove('data-stale');
      taskCursors[pageIndex + 1] = page.has_more ? page.next_cursor : null;
      taskPrevious.disabled = pageIndex === 0;
      taskNext.disabled = !page.has_more || !page.next_cursor;
    } catch (error) {
      if (signal.aborted || epoch !== taskRequest) return;
      taskNote.textContent = `Task history could not refresh. ${error.message}${taskJSON ? ' Previously loaded results are shown.' : ''}`;
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
      timezone.textContent = `Server timezone: ${timezoneText(page)}. Next-run times below are supplied by the Server, not converted to your browser timezone. Timezone changes require a Server restart.`;
      const json = JSON.stringify(page.plans);
      if (json !== planJSON) {
        planRows.replaceChildren(page.plans.length ? h('div', {class: 'data-plan-list'}, page.plans.map(planCard)) : h('p', {class: 'data-empty'}, 'No backup plans on this page. Add a disabled plan, review it, then enable it when ready.'));
        planJSON = json;
      }
      planNote.textContent = '';
      planNote.classList.remove('data-stale');
      planCursors[pageIndex + 1] = page.has_more ? page.next_cursor : null;
      planPrevious.disabled = pageIndex === 0;
      planNext.disabled = !page.has_more || !page.next_cursor;
    } catch (error) {
      if (signal.aborted || epoch !== planRequest) return;
      planNote.textContent = `Backup plans could not refresh. ${error.message}${planJSON ? ' Previously loaded results are shown.' : ''}`;
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
      h('thead', null, h('tr', null, ['Created', 'Source', 'Categories', 'State', 'Details'].map((text) => h('th', {scope: 'col'}, text)))),
      h('tbody', null, tasks.map((task) => h('tr', null,
        cell('Created', when(task.created_at)),
        cell('Source', task.destination.plan_name || (task.kind === 'backup_export' ? 'Manual export' : 'Deleted plan'),
          task.destination.actor_name ? h('small', {class: 'data-secondary'}, `Started by ${task.destination.actor_name}`) : null),
        cell('Categories', labels(task.categories)),
        cell('State', taskStatus(task), task.warning_code ? h('small', {class: 'data-secondary'}, 'Warning · view details') : null),
        cell('Details', button('Details', {onClick: () => showTask(task.id, {signal: controller?.signal})})))))));
  }

  function planCard(plan) {
    const last = plan.last_task;
    const next = plan.next_runs?.[0];
    return h('article', {class: 'data-plan'},
      h('div', {class: 'data-plan-head'}, h('div', null, h('h3', null, plan.name),
        h('span', {class: 'data-secondary'}, plan.kind === 'local' ? 'Local directory' : PROVIDERS.find(([id]) => id === plan.destination.provider)?.[1] || plan.destination.provider)),
      h('span', {class: `data-state ${plan.enabled ? 'data-state-succeeded' : 'data-state-unknown'}`}, plan.enabled ? 'Enabled' : 'Disabled')),
      h('p', {class: 'data-location'}, planLocation(plan)),
      h('dl', {class: 'data-plan-facts'},
        h('div', null, h('dt', null, 'Cron'), h('dd', null, h('code', null, plan.cron))),
        h('div', null, h('dt', null, 'Next run · Server local'), h('dd', null, plan.enabled && next ? localRun(next) : 'Not scheduled')),
        h('div', null, h('dt', null, 'Categories'), h('dd', null, labels(plan.categories))),
        h('div', null, h('dt', null, 'Retention'), h('dd', null, plan.kind === 'local' ? `Latest ${plan.retain_count} archives` : 'Managed by your cloud lifecycle policy')),
        h('div', null, h('dt', null, 'Last task'), h('dd', null, last ? h('span', null, taskStatus(last), ' ', when(last.created_at), last.warning_code ? ' · Warning' : '') : 'No task yet'))),
      h('div', {class: 'data-actions'},
        button('Edit', {onClick: () => editPlan(plan)}),
        button('Delete', {variant: 'danger', onClick: () => deletePlan(plan)}),
        last ? button('Last task details', {onClick: () => showTask(last.id, {signal: controller?.signal})}) : null));
  }

  async function deletePlan(plan) {
    const signal = controller?.signal;
    if (!signal || signal.aborted) return;
    const confirmed = await confirmDialog({title: `Delete ${plan.name}?`, message: 'This removes the schedule. It will not delete any saved backups or task history. A plan with a queued or running task cannot be deleted; wait for that task to finish.', confirm: 'Delete plan', danger: true});
    if (!confirmed || signal.aborted) return;
    try {
      await api(`/backup-plans/${encodeURIComponent(plan.id)}`, {method: 'DELETE', signal});
      if (!signal.aborted) { planPage = 0; planCursors = [null]; await loadPlans(); }
    } catch (error) {
      if (signal.aborted) return;
      if (error.code === 'backup_plan_busy') error.message = 'This plan has a queued or running task. Wait for it to finish before deleting the plan.';
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
    title: plan ? `Edit ${plan.name}` : 'Add backup plan',
    intro: 'Schedules use the Server timezone. Saved archives have independent passwords; save each recovery credential before its 90-day record expires. Deleting a plan does not delete its saved archives.',
    submit: plan ? 'Save plan' : 'Add plan',
    build: (form) => {
      form.classList.add('data-plan-form');
      const destination = plan?.destination || {};
      const name = input({name: 'name', required: true, value: plan?.name || '', autocomplete: 'off'});
      validateBytes(name, 1, 100, 'Plan name');
      const kind = select([['local', 'Local directory'], ['object', 'Object storage']], plan?.kind || 'local', {name: 'kind', disabled: Boolean(plan)});
      const enabled = checkField('Enabled', 'enabled', plan?.enabled || false, 'New plans are disabled by default. Enabling starts at the next Cron occurrence, not immediately.');
      const categories = categoriesField(plan?.categories);
      const cron = input({name: 'cron', value: plan?.cron || '0 3 * * *', required: true, autocomplete: 'off', spellcheck: false});
      const preview = h('div', {class: 'data-cron-preview', role: 'status'}, 'Checking the next three Server-local runs…');
      const path = input({name: 'path', 'data-field': 'destination.path', value: destination.path || '', required: true, placeholder: '/var/lib/orbitbeacon/backups', autocomplete: 'off'});
      const retain = input({name: 'retain_count', type: 'number', min: 1, max: 1000, step: 1, value: plan?.retain_count ?? 7, required: true});
      const local = h('fieldset', {class: 'data-destination'}, h('legend', null, 'Local destination'),
        field('Directory', path, {hint: 'Must already exist, be owned by the Server user, have mode 0700 and contain no symlinks. The Server does not change permissions. For paths outside the data directory, configure a systemd ReadWritePaths drop-in or a writable Docker volume.'}),
        field('Archives to retain', retain, {hint: '1–1000. Only this plan’s own oldest archives are removed after a successful backup.'}));
      const provider = select(PROVIDERS, destination.provider || 's3_compatible', {name: 'provider', 'data-field': 'destination.provider'});
      const endpoint = input({name: 'endpoint', 'data-field': 'destination.endpoint', value: destination.endpoint || '', type: 'url', required: true, autocomplete: 'off'});
      const region = input({name: 'region', 'data-field': 'destination.region', value: destination.region || '', required: true, autocomplete: 'off'});
      const bucket = input({name: 'bucket', 'data-field': 'destination.bucket', value: destination.bucket || '', required: true, autocomplete: 'off'});
      const prefix = input({name: 'prefix', 'data-field': 'destination.prefix', value: destination.prefix || '', autocomplete: 'off'});
      const pathStyle = checkField('Path-style addressing', 'path_style', destination.path_style ?? true, 'S3 compatible supports either style. R2 requires path-style; COS, OSS and AWS S3 use virtual-host addressing.');
      const replace = checkField('Replace stored credentials', 'replace_credentials', !plan, 'Stored keys are never displayed. Leave unchecked to keep them. A replacement must include both keys; an omitted session token clears the old token.');
      const keyID = input({name: 'access_key_id', 'data-field': 'credentials.access_key_id', autocomplete: 'off', spellcheck: false});
      const secret = input({name: 'secret_access_key', 'data-field': 'credentials.secret_access_key', type: 'password', autocomplete: 'new-password'});
      const token = input({name: 'session_token', 'data-field': 'credentials.session_token', type: 'password', autocomplete: 'new-password'});
      const credentialFields = h('fieldset', {class: 'data-destination'}, h('legend', null, 'Replacement credentials'),
        field('Access key ID', keyID), field('Secret access key', secret), field('Session token (optional)', token));
      const credentialsNote = h('p', {class: 'hint'}, plan ? `Stored credentials: ${plan.credentials_set ? 'set' : 'not set'}. Session token: ${plan.session_token_set ? 'set' : 'not set'}.` : 'New object-storage plans require complete credentials.');
      const allowPrivate = checkField('Allow private networks', 'allow_private_networks', destination.allow_private_networks || false, 'Explicitly allow private destination addresses. Loopback, link-local and metadata endpoints are still blocked. HTTPS certificate validation remains required.');
      const providerHint = h('p', {class: 'hint'});
      const recommendation = button('Use recommended endpoint', {onClick: () => {
        const suggested = suggestedEndpoint(provider.value, region.value.trim());
        if (suggested) { endpoint.value = suggested; endpoint.dispatchEvent(new Event('input', {bubbles: true})); }
      }});
      const remote = h('fieldset', {class: 'data-destination'}, h('legend', null, 'Object-storage destination'),
        field('Provider', provider), field('HTTPS endpoint', endpoint, {hint: 'Origin only: no path, user information, query or fragment.'}),
        h('div', {class: 'data-endpoint-help'}, providerHint, recommendation),
        field('Region', region), field('Bucket', bucket), field('Prefix (optional)', prefix, {hint: 'Relative slash-separated path; no empty components, dot segments or backslashes.'}),
        pathStyle.element, credentialsNote, replace.element, credentialFields, allowPrivate.element,
        h('p', {class: 'hint'}, 'Remote archives are retained by your cloud lifecycle policy. Passwords are never uploaded with archives. OSS custom domains must already be bucket-bound and correctly configured for TLS.'));
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
        providerHint.textContent = value === 'r2' ? 'Enter your account-specific R2 endpoint. Region is auto.' : suggested ? `Recommended: ${suggested}. Custom values are not replaced automatically.` : value === 's3_compatible' ? 'Enter your service’s HTTPS endpoint and signing region.' : 'Enter a region to see the recommended endpoint. COS buckets include the APPID.';
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
        preview.textContent = 'Checking schedule…';
        try {
          const result = await api('/backup-plans/preview', {method: 'POST', body: {cron: cron.value}, signal: previewSignal});
          if (controller.signal.aborted || previewSignal.aborted || epoch !== previewEpoch) return;
          cron.showError('');
          preview.replaceChildren(h('p', null, `Server timezone: ${timezoneText(result)}`), h('ol', null, result.next_runs.map((run) => h('li', null, localRun(run)))));
        } catch (error) {
          if (controller.signal.aborted || previewSignal.aborted || epoch !== previewEpoch) return;
          const message = error.fields?.cron;
          if (message) cron.showError(message);
          preview.textContent = message ? 'No preview: correct the Cron expression. Saving is always validated by the Server.' : `Schedule preview unavailable. ${error.message} Saving is still validated by the Server.`;
        } finally { controller.signal.removeEventListener('abort', cancel); }
      };
      cron.addEventListener('input', () => {
        clearTimeout(previewTimer);
        previewEpoch += 1;
        previewController?.abort();
        preview.textContent = 'Waiting to check schedule…';
        previewTimer = setTimeout(previewCron, 500);
      });
      provider.addEventListener('change', () => updateProvider(true));
      region.addEventListener('input', () => updateProvider());
      kind.addEventListener('change', updateKind);
      replace.control.addEventListener('change', updateCredentials);
      form.append(field('Name', name), field('Destination kind', kind, {hint: plan ? 'Destination kind cannot be changed after creation.' : undefined}), enabled.element, categories.element,
        field('Cron', cron, {hint: 'Five fields: minute hour day-of-month month day-of-week. Default: 0 3 * * * (03:00 Server time). No seconds, TZ prefixes or @ shortcuts.'}), preview, local, remote);
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
        if (error.code === 'backup_plan_busy') error.message = 'This plan has a queued or running task. Your edits are kept here. Wait for the task to finish, then save again; editing never cancels it.';
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
