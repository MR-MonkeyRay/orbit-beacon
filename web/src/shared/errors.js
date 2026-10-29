import {t} from './i18n.js';

const errorKeys = new Set([
  'unauthenticated', 'forbidden', 'password_change_required', 'password_unchanged', 'public_page_disabled',
  'invalid_old_password', 'rate_limited', 'validation_failed', 'invalid_json', 'network', 'timeout',
  'unreadable_response', 'locale_save_failed', 'not_found', 'node_not_found', 'node_disabled',
  'node_capacity_reached', 'username_conflict', 'last_administrator', 'storage_busy', 'not_available',
  'body_too_large', 'internal_error', 'secret_unreadable', 'backup_plan_busy', 'backup_queue_full',
  'notification_channel_capacity_reached', 'alert_rule_capacity_reached', 'enrollment_not_found',
  'enrollment_conflict', 'identity_conflict', 'artifact_not_ready', 'artifact_expired', 'password_expired',
  'task_not_found', 'backup_plan_not_found', 'locale_storage_failed', 'secret_unavailable',
  'snapshot_failed', 'history_unavailable', 'archive_too_large', 'disk_full', 'storage_permission_denied',
  'storage_unreachable', 'storage_auth_failed', 'storage_upload_failed', 'interrupted', 'restore_interrupted',
  'backup_secret_unreadable'
]);

export function errorText(error) {
  let code = error?.code;
  if (error?.name === 'TimeoutError') code = 'timeout';
  const message = t(errorKeys.has(code) ? `errors.${code}` : 'errors.generic');
  return error?.requestID ? t('errors.withRequest', {message, id: error.requestID}) : message;
}

const fieldReasons = new Map([
  ['is required', 'fields.required'], ['must be valid UTF-8', 'fields.utf8'],
  ['must be nonempty when published', 'fields.publishedName'],
  ['must be valid UTF-8 without control characters and at most 255 bytes', 'fields.publicText255'],
  ['must be valid UTF-8 without control characters and at most 128 bytes', 'fields.publicText128'],
  ['Select one or more distinct data categories.', 'fields.selectCategories'],
  ['must not be null', 'fields.notNull'], ['cannot be null', 'fields.notNull'],
  ['Must not be null; omit this field to retain its value.', 'fields.notNull'],
  ['has an invalid type', 'fields.invalidType'], ['entries must be unique', 'fields.unique'],
  ['group does not exist', 'fields.groupMissing'], ['must be a group UUID or null', 'fields.groupID'],
  ['must be between 1 and 28', 'fields.billingDay'], ['must be a canonical unsigned decimal byte count', 'fields.quota'],
  ['must be between 0 and 86400', 'fields.duration'], ['must be info, warning or critical', 'fields.severity'],
  ['offline has no comparator or threshold', 'fields.offlineThreshold'],
  ['offline duration must be at least the online window', 'fields.offlineDuration'],
  ['must be finite and within the metric bounds', 'fields.threshold'], ['must be > or <', 'fields.comparator'],
  ['unsupported metric', 'fields.metric'], ['must be null for all', 'fields.scope'],
  ['must be all, group or node', 'fields.scope'], ['must identify an existing scope', 'fields.scope'], ['scope does not exist', 'fields.scope'],
  ['must be an array of at most 20 existing channel IDs', 'fields.channels'],
  ['must contain unique existing channel IDs', 'fields.channels'], ['channel does not exist', 'fields.channels'],
  ['must be an object', 'fields.object'], ['unexpected fields for webhook', 'fields.object'],
  ['unexpected or null configuration field', 'fields.object'], ['invalid configuration fields', 'fields.object'],
  ['unknown field', 'fields.object'], ['invalid field type', 'fields.invalidType'],
  ['must be a string; omit to retain or empty to clear', 'fields.invalidType'],
  ['webhook URL cannot be empty', 'fields.webhook'],
  ['requires an absolute webhook URL without credentials or fragment', 'fields.webhook'],
  ['invalid webhook port', 'fields.port'],
  ['HTTPS required; explicit insecure setting permits literal loopback HTTP only', 'fields.webhookTLS'],
  ['requires a valid chat_id only', 'fields.chatID'], ['requires a bot token', 'fields.botToken'],
  ['requires SMTP host, port 1..65535, from and 1..20 recipients', 'fields.smtp'], ['invalid recipient address', 'fields.recipient'],
  ['cannot contain NUL', 'fields.nul'], ['SMTP credentials exceed command size limit', 'fields.smtpSize'],
  ['password required with username', 'fields.smtpPassword'], ['must be webhook, telegram or email', 'fields.channelKind'],
  ['name, kind, enabled and config required', 'fields.channelRequired'], ['cannot change kind', 'fields.kindImmutable'],
  ['Enter a destination object.', 'fields.object'], ['Destination fields cannot be null.', 'fields.destinationNull'],
  ['Changing provider requires endpoint, region and replacement credentials.', 'fields.providerChange'],
  ['Enter a local path.', 'fields.localPath'], ['Enter a valid object destination.', 'fields.destination'],
  ['Use an existing owned 0700 directory without symlinks.', 'fields.localPath'],
  ['The server must be able to write this directory.', 'fields.directoryWrite'],
  ['The server must be able to write and remove files in this directory.', 'fields.directoryWrite'],
  ['The destination kind cannot be changed.', 'fields.kindImmutable'], ['Choose local or object storage.', 'fields.planKind'],
  ['Keep between 1 and 1000 archives.', 'fields.retention'], ['Local plans do not use credentials.', 'fields.localCredentials'],
  ['Enter a valid five-field Cron expression.', 'fields.cron'],
  ['This expression has no upcoming run.', 'fields.cronUpcoming'],
  ['Object retention is managed by the storage provider.', 'fields.objectRetention'],
  ['Enter a valid destination for this provider.', 'fields.destination'],
  ['Provide a complete, unmasked access key and secret.', 'fields.credentials'], ['Provide an access key and secret.', 'fields.credentials']
]);
const byteLimits = new Map([
  ['must be nonempty and at most 255 bytes', 255],
  ['entries must be nonempty and at most 255 bytes', 255],
  ['must contain 1..100 UTF-8 bytes', 100],
  ['Enter a name of at most 100 bytes.', 100],
  ['must be at most 4096 bytes', 4096]
]);

export function fieldErrorText(reason) {
  if (byteLimits.has(reason)) return t('fields.byteLimit', {max: byteLimits.get(reason)});
  return t(fieldReasons.get(reason) || 'fields.generic');
}
