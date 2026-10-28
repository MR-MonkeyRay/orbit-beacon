import {h} from '../../shared/dom.js';
import {api, getNavigationSignal} from '../api.js';
import {when} from '../format.js';
import {facts, link} from '../ui/components.js';
import {modal} from '../ui/dialogs.js';
import {backupPassword} from '../ui/backup-password.js';

const errors = {
  snapshot_failed: 'The database snapshot could not be created.',
  history_unavailable: 'The metrics history database is missing, unreadable or has an unsupported version.',
  archive_too_large: 'The selected data exceeds the 32 GiB archive limit.',
  disk_full: 'The backup destination has insufficient free space.',
  storage_permission_denied: 'The server cannot write to this backup destination.',
  storage_unreachable: 'The storage service could not be reached securely.',
  storage_auth_failed: 'The storage service rejected the credentials.',
  storage_upload_failed: 'The storage service did not confirm a successful upload.',
  secret_unreadable: 'The server key could not decrypt the required backup secret.',
  interrupted: 'The server stopped before the task completed. It was not restarted automatically.',
  restore_interrupted: 'Offline restore interrupted this task. It was not replayed.',
  internal_error: 'The task could not be completed.'
};
const stateNames = {queued: 'Queued', running: 'Running', succeeded: 'Succeeded', failed: 'Failed'};
const phaseNames = {snapshot: 'Creating database snapshots', packaging: 'Compressing and encrypting', storing: 'Storing archive'};
export const taskState = (task) => stateNames[task.state] || task.state;
export const categoryNames = (categories = []) => categories.map((category) => ({configuration: 'Configuration', monitoring: 'Monitoring', events: 'Events'}[category] || category)).join(', ');

export function taskDetails(task, {signal} = {}) {
  const element = h('div', {class: 'stack backup-task-detail'});
  const notices = [];
  if (task.state === 'failed') notices.push(h('div', {class: 'callout', role: 'status'}, errors[task.error_code] || `Task failed (${task.error_code || 'unknown error'}). Start a new manual export or wait for the next scheduled run.`));
  if (task.warning_code === 'storage_outcome_unknown') notices.push(h('div', {class: 'callout', role: 'status'}, 'Storage outcome unknown. An archive may exist at the location below. Verify its ciphertext SHA-256 before recovering it; this task is not a confirmed success. Save its recovery password now.'));
  if (task.warning_code === 'retention_cleanup_failed') notices.push(h('div', {class: 'callout', role: 'status'}, 'The backup succeeded, but old local archives could not be cleaned up. Check destination permissions and free space.'));
  if (task.restored) notices.push(h('p', {class: 'note'}, 'Restored historical task. Original file metadata is retained, but downloads and passwords are unavailable.'));
  const source = task.destination?.plan_name || task.destination?.actor_name || (task.kind === 'backup_export' ? 'Manual export' : 'Deleted plan');
  element.append(...notices, facts([
    ['Task ID / archive ID', task.id], ['Source', source], ['Kind', {backup_export: 'Manual export', backup_local: 'Local backup', backup_remote: 'Object storage backup'}[task.kind] || task.kind],
    ['Categories', categoryNames(task.categories)], ['State', taskState(task)],
    ['Phase', task.state === 'running' ? phaseNames[task.phase] || 'Starting' : null],
    ['Queued', when(task.created_at)], ['Scheduled', task.scheduled_at ? when(task.scheduled_at) : null], ['Started', task.started_at ? when(task.started_at) : null], ['Finished', task.finished_at ? when(task.finished_at) : null],
    ['Archive filename', task.artifact_name], ['Ciphertext size', task.artifact_bytes == null ? null : `${task.artifact_bytes} bytes`],
    ['Ciphertext SHA-256', task.artifact_sha256 ? h('code', {class: 'backup-hash'}, task.artifact_sha256) : null], ['Location', task.result_location],
    ['Download expires', task.artifact_expires_at ? when(task.artifact_expires_at) : null], ['Password expires', task.password_expires_at ? when(task.password_expires_at) : null]
  ]));
  if (task.kind === 'backup_export' && task.state === 'succeeded') {
    if (task.download_available) element.append(h('a', {class: 'btn btn-primary', href: `/api/v1/tasks/${encodeURIComponent(task.id)}/download`}, 'Download archive'));
    else element.append(h('p', {class: 'muted'}, 'The 24-hour download has expired or the local file is unavailable.'));
  }
  if (task.kind !== 'backup_export') element.append(h('p', {class: 'muted'}, 'Scheduled archives stay at their configured destination. This server does not provide a separate download copy.'));
  const password = backupPassword(task, {signal});
  element.append(password.element);
  return {element, dispose: password.dispose};
}

export function showTask(id, {signal = getNavigationSignal()} = {}) {
  const controller = new AbortController();
  let details;
  let disposed = false;
  const body = h('div', {class: 'dialog-body stack'}, h('p', {class: 'muted', role: 'status'}, 'Loading task…'));
  const dialog = modal('Backup task', () => body);
  const dispose = () => {
    if (disposed) return;
    disposed = true; controller.abort(); details?.dispose(); signal?.removeEventListener('abort', stop);
  };
  const stop = () => { dispose(); dialog.close('navigation'); };
  signal?.addEventListener('abort', stop, {once: true});
  dialog.closed.then(dispose);
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(id)) {
    body.replaceChildren(h('p', {role: 'alert'}, 'Task not found. Check the task link.'));
    return dialog;
  }
  if (signal?.aborted) { stop(); return dialog; }
  api(`/tasks/${encodeURIComponent(id)}`, {signal: controller.signal}).then((task) => {
    if (disposed || signal?.aborted) return;
    details = taskDetails(task, {signal: controller.signal});
    body.replaceChildren(details.element, link(`/settings?task=${encodeURIComponent(task.id)}#data`, 'Task link'));
  }).catch((error) => {
    if (disposed || error.name === 'AbortError') return;
    body.replaceChildren(h('p', {role: 'alert'}, error.status === 404 ? 'Task not found. It may have expired.' : `Could not load task: ${error.message}`));
  });
  return dialog;
}
