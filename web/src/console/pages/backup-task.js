import {h} from '../../shared/dom.js';
import {t, getLocale} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {api, getNavigationSignal} from '../api.js';
import {when} from '../format.js';
import {facts, link} from '../ui/components.js';
import {modal} from '../ui/dialogs.js';
import {backupPassword} from '../ui/backup-password.js';

const stateNames = {queued: 'common.queued', running: 'common.running', succeeded: 'common.succeeded', failed: 'common.failed'};
const phaseNames = {snapshot: 'data.phase.snapshot', packaging: 'data.phase.packaging', storing: 'data.phase.storing'};
const failureNames = new Set(['snapshot_failed', 'history_unavailable', 'archive_too_large', 'disk_full', 'storage_permission_denied',
  'storage_unreachable', 'storage_auth_failed', 'storage_upload_failed', 'secret_unreadable', 'interrupted', 'restore_interrupted', 'internal_error']);
export const taskState = (task) => t(stateNames[task.state] || 'data.state.unknown');
export const categoryNames = (categories = []) => categories.map((category) => ({configuration: 'common.configuration', monitoring: 'common.monitoring', events: 'common.events'}[category])).map((key) => t(key || 'data.category.unknown')).join(t('data.listSeparator'));

export function taskDetails(task, {signal} = {}) {
  const element = h('div', {class: 'stack backup-task-detail'});
  const notices = [];
  if (task.state === 'failed') notices.push(h('div', {class: 'callout', role: 'status'}, t(`data.failure.${failureNames.has(task.error_code) ? task.error_code : 'unknown'}`)));
  if (task.warning_code === 'storage_outcome_unknown') notices.push(h('div', {class: 'callout', role: 'status'}, t('data.task.storageUnknown')));
  if (task.warning_code === 'retention_cleanup_failed') notices.push(h('div', {class: 'callout', role: 'status'}, t('data.task.cleanupFailed')));
  if (task.restored) notices.push(h('p', {class: 'note'}, t('data.task.restored')));
  const source = task.destination?.plan_name || task.destination?.actor_name || t(task.kind === 'backup_export' ? 'data.task.manual' : 'data.task.deletedPlan');
  element.append(...notices, facts([
    [t('data.task.id'), task.id], [t('data.task.source'), source], [t('data.task.kind'), t({backup_export: 'data.task.manual', backup_local: 'data.task.local', backup_remote: 'data.task.remote'}[task.kind] || 'data.task.unknownKind')],
    [t('data.categories'), categoryNames(task.categories)], [t('data.task.state'), taskState(task)],
    [t('data.task.phase'), task.state === 'running' ? t(phaseNames[task.phase] || 'data.phase.starting') : null],
    [t('data.task.queued'), when(task.created_at)], [t('data.task.scheduled'), task.scheduled_at ? when(task.scheduled_at) : null], [t('data.task.started'), task.started_at ? when(task.started_at) : null], [t('data.task.finished'), task.finished_at ? when(task.finished_at) : null],
    [t('data.task.filename'), task.artifact_name], [t('data.task.size'), task.artifact_bytes == null ? null : t('data.task.bytes', {value: new Intl.NumberFormat(getLocale()).format(task.artifact_bytes)})],
    [t('data.task.sha256'), task.artifact_sha256 ? h('code', {class: 'backup-hash'}, task.artifact_sha256) : null], [t(task.kind === 'backup_remote' ? 'data.task.objectKey' : 'data.task.location'), task.result_location],
    [t('data.task.downloadExpires'), task.artifact_expires_at ? when(task.artifact_expires_at) : null], [t('data.task.passwordExpires'), task.password_expires_at ? when(task.password_expires_at) : null]
  ]));
  if (task.kind === 'backup_export' && task.state === 'succeeded') {
    if (task.download_available) element.append(h('a', {class: 'btn btn-primary', href: `/api/v1/tasks/${encodeURIComponent(task.id)}/download`}, t('data.task.download')));
    else element.append(h('p', {class: 'muted'}, t('data.task.downloadUnavailable')));
  }
  if (task.kind !== 'backup_export') element.append(h('p', {class: 'muted'}, t('data.task.scheduledDestination')));
  const password = backupPassword(task, {signal});
  element.append(password.element);
  return {element, dispose: password.dispose};
}

export function showTask(id, {signal = getNavigationSignal()} = {}) {
  const controller = new AbortController();
  let details;
  let disposed = false;
  const body = h('div', {class: 'dialog-body stack'}, h('p', {class: 'muted', role: 'status'}, t('data.task.loading')));
  const dialog = modal(t('data.task.title'), () => body);
  const dispose = () => {
    if (disposed) return;
    disposed = true; controller.abort(); details?.dispose(); signal?.removeEventListener('abort', stop);
  };
  const stop = () => { dispose(); dialog.close('navigation'); };
  signal?.addEventListener('abort', stop, {once: true});
  dialog.closed.then(dispose);
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(id)) {
    body.replaceChildren(h('p', {role: 'alert'}, t('data.task.invalidLink')));
    return dialog;
  }
  if (signal?.aborted) { stop(); return dialog; }
  api(`/tasks/${encodeURIComponent(id)}`, {signal: controller.signal}).then((task) => {
    if (disposed || signal?.aborted) return;
    details = taskDetails(task, {signal: controller.signal});
    body.replaceChildren(details.element, link(`/settings?task=${encodeURIComponent(task.id)}#data`, t('data.task.link')));
  }).catch((error) => {
    if (disposed || error.name === 'AbortError') return;
    body.replaceChildren(h('p', {role: 'alert'}, error.status === 404 ? t('data.task.notFound') : t('data.task.loadFailed', {error: errorText(error)})));
  });
  return dialog;
}
