// Alert and notification form logic, kept independent of the DOM.
const utf8 = new TextEncoder();
const bytes = (value) => utf8.encode(value || '').length;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
export const METRICS = [
  ['cpu_percent', 'CPU usage'], ['memory_used_percent', 'Memory usage'],
  ['swap_used_percent', 'Swap usage'], ['disk_used_percent', 'Disk usage'],
  ['load1', 'Load (1 minute)'], ['probes_down', 'Failing probes'], ['offline', 'Offline']
];
export const SEVERITIES = [['info', 'Info'], ['warning', 'Warning'], ['critical', 'Critical']];
export const metricLabel = (metric) => METRICS.find(([id]) => id === metric)?.[1] || 'Unknown metric';
export const severityLabel = (severity) => SEVERITIES.find(([id]) => id === severity)?.[1] || 'Unknown severity';
export const metricUnit = (metric = '') => metric.endsWith('_percent') ? '%' : metric === 'load1' ? 'load' : metric === 'probes_down' ? 'probes' : '';
// Upper threshold bound per metric, mirroring the server's rule validation.
export const metricMax = (metric) => metric === 'load1' ? 1000000 : metric === 'probes_down' ? 5 : 100;
// Mirrors notify.validHost on the server: an IP literal or LDH labels.
export function validHost(host) {
  if (!host || host.length > 253 || host.includes('%')) return false;
  if (/^\d{1,3}(?:\.\d{1,3}){3}$/.test(host) && host.split('.').every((part) => Number(part) <= 255)) return true;
  if (host.includes(':')) { try { return new URL(`http://[${host}]/`).hostname.length > 2; } catch { return false; } }
  return host.replace(/\.$/, '').split('.').every((label) => /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/.test(label));
}

export function formatMetricValue(metric, value) {
  if (!metric) return Number.isFinite(value) ? `${value} (unknown metric)` : 'Unavailable (unknown metric)';
  if (metric === 'offline') return 'Offline';
  if (!Number.isFinite(value)) return 'Unavailable';
  if (metricUnit(metric || '') === '%') return `${Number(value.toFixed(1))}%`;
  if (metric === 'load1') return value > 0 && value < .01 ? '< 0.01 load' : `${value.toFixed(2)} load`;
  if (metric === 'probes_down') return `${value} failing ${value === 1 ? 'probe' : 'probes'}`;
  return `${value} (unknown metric)`;
}
export function formatRuleDuration(seconds) {
  if (!Number.isInteger(seconds) || seconds < 0) return 'Unavailable';
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  return minutes ? `${minutes}m${remainder ? ` ${remainder}s` : ''}` : `${remainder}s`;
}
export function durationSeconds(minutes, seconds) {
  if (String(minutes).trim() === '' || String(seconds).trim() === '') return NaN;
  const m = Number(minutes), s = Number(seconds);
  return Number.isInteger(m) && m >= 0 && Number.isInteger(s) && s >= 0 && s <= 59 ? m * 60 + s : NaN;
}
export function scopeLabel(rule, groups = [], nodes = []) {
  if (rule.scope_kind === 'all') return 'All nodes';
  const collection = rule.scope_kind === 'group' ? groups : rule.scope_kind === 'node' ? nodes : null;
  if (!collection) return 'Unknown scope';
  const label = collection.find((item) => item.id === rule.scope_id)?.name || rule.scope_id || 'Unknown';
  return `${rule.scope_kind === 'group' ? 'Group' : 'Node'}: ${label}`;
}
export function validateRule(rule) {
  const fields = {};
  if (!rule.name?.trim() || bytes(rule.name) > 100) fields.name = 'Enter a nonblank name of at most 100 UTF-8 bytes.';
  if (typeof rule.enabled !== 'boolean') fields.enabled = 'Choose whether this rule is enabled.';
  if (!['all', 'group', 'node'].includes(rule.scope_kind)) fields.scope_kind = 'Choose a scope.';
  else if (rule.scope_kind === 'all' ? rule.scope_id != null : !uuid.test(rule.scope_id || '')) fields.scope_id = rule.scope_kind === 'all' ? 'All nodes has no scope ID.' : 'Choose an existing group or node.';
  if (!METRICS.some(([id]) => rule.metric === id)) fields.metric = 'Choose a metric.';
  if (rule.metric === 'offline') {
    if (rule.comparator != null) fields.comparator = 'Offline rules have no comparator.';
    if (rule.threshold != null) fields.threshold = 'Offline rules have no threshold.';
  } else {
    if (!['>', '<'].includes(rule.comparator)) fields.comparator = 'Choose greater than or less than.';
    const max = metricMax(rule.metric);
    if (!Number.isFinite(rule.threshold) || rule.threshold < 0 || rule.threshold > max) fields.threshold = `Enter a finite value from 0 to ${max}.`;
  }
  const minimum = rule.metric === 'offline' ? 180 : 0;
  if (!Number.isInteger(rule.for_seconds) || rule.for_seconds < minimum || rule.for_seconds > 86400) fields.for_seconds = `Duration must be ${minimum}–86400 whole seconds${minimum ? ' (at least 3 minutes for offline)' : ''}.`;
  if (!SEVERITIES.some(([id]) => rule.severity === id)) fields.severity = 'Choose a severity.';
  if (!Array.isArray(rule.channel_ids) || rule.channel_ids.length > 20 || new Set(rule.channel_ids).size !== rule.channel_ids.length || rule.channel_ids.some((id) => !uuid.test(id))) fields.channel_ids = 'Choose up to 20 distinct existing channels.';
  return fields;
}
export function validationError(fields) {
  return Object.assign(new Error('Check the highlighted fields.'), {code: 'validation_failed', fields});
}

export const SECRET_GUIDANCE = 'The server cannot decrypt this channel after a key change. Restore the matching server key, or re-enter every affected credential. For webhooks, replace the URL and re-enter or clear the signing secret. For Telegram or email, re-enter the token or password.';
const ERROR_MESSAGES = {
  secret_unreadable: SECRET_GUIDANCE,
  secret_unavailable: 'Configure the server notification encryption key, then re-enter the credentials.',
  destination_denied: 'The destination is blocked by the server network policy. Check the address and private-network settings.',
  insecure_webhook_denied: 'Use HTTPS. HTTP loopback requires the server’s insecure webhook and private-network settings.',
  invalid_destination: 'Check the destination URL, chat ID, or SMTP address.',
  http_rejected: 'The destination rejected the notification. Check the URL and its access settings.',
  remote_unavailable: 'The destination service is unavailable. Try again later.',
  network_error: 'The server could not reach the destination. Check DNS, connectivity, and TLS.',
  redirect_denied: 'The destination redirected the request. Use its final HTTPS URL.',
  telegram_rejected: 'Telegram rejected the request. Check the bot token, chat ID, and bot permissions.',
  smtp_rejected: 'The mail server rejected the request. Check credentials, sender, and recipients.',
  smtp_protocol: 'The mail server returned an unexpected SMTP response. Check the host and port.',
  starttls_required: 'The mail server must support STARTTLS before credentials can be sent.',
  invalid_payload: 'The notification could not be prepared. Check the channel configuration.',
  rate_limited: 'Too many notifications or tests. Wait before trying again.',
  channel_disabled: 'The channel is disabled.',
  attempts_exhausted: 'Delivery stopped after all retry attempts.',
  canceled: 'The notification request was cancelled.',
  notification_channel_capacity_reached: 'The server allows at most 20 notification channels.',
  alert_rule_capacity_reached: 'The server allows at most 200 alert rules.'
};
export function errorCodeMessage(code) {
  return ERROR_MESSAGES[code] || (code ? `Notification failed (${code}).` : 'No error reported.');
}

// Untouched write-only values must be absent: an empty secret explicitly clears it.
export function buildChannelPayload(values, {editing = false, replaceURL = false, replaceSecret = false} = {}) {
  const body = {name: values.name, enabled: values.enabled};
  if (!editing) body.kind = values.kind;
  if (values.kind === 'webhook') body.config = !editing || replaceURL ? {url: values.url} : {};
  else if (values.kind === 'telegram') body.config = {chat_id: values.chat_id};
  else if (values.kind === 'email') body.config = {
    host: values.host, port: values.port === '' ? NaN : Number(values.port), username: values.username || '',
    from: values.from, to: Array.isArray(values.to) ? values.to : String(values.to || '').split(/[,\n]/).map((item) => item.trim()).filter(Boolean)
  };
  if (replaceSecret || values.secret) body.secret = values.secret ?? '';
  return body;
}
export function validateChannel(body, {kind = body.kind, existing = null} = {}) {
  const fields = {};
  const config = body.config || {};
  if (!body.name?.trim() || bytes(body.name) > 100) fields.name = 'Enter a nonblank name of at most 100 UTF-8 bytes.';
  if (!['webhook', 'telegram', 'email'].includes(kind)) fields.kind = 'Choose a channel kind.';
  if (body.secret !== undefined && (typeof body.secret !== 'string' || bytes(body.secret) > 4096)) fields.secret = 'The secret is limited to 4096 UTF-8 bytes.';
  const hasSecret = body.secret === undefined ? existing?.secret_set : Boolean(body.secret);
  if (kind === 'webhook' && (config.url !== undefined || !existing?.config?.url_set)) {
    try {
      const url = new URL(config.url);
      const loopback = url.hostname === '[::1]' || /^127(?:\.\d{1,3}){3}$/.test(url.hostname);
      if (url.username || url.password || url.hash || !['https:', 'http:'].includes(url.protocol) || (url.protocol === 'http:' && !loopback)) throw new Error();
    } catch { fields['config.url'] = 'Use an HTTPS URL without credentials or a fragment. HTTP is only allowed for literal loopback when configured on the server.'; }
  }
  if (kind === 'telegram') {
    if (!/^(?:-?[0-9]{1,20}|@[A-Za-z][A-Za-z0-9_]{4,31})$/.test(config.chat_id || '')) fields['config.chat_id'] = 'Enter a numeric chat ID or an @channel username (5–32 characters).';
    if (!hasSecret || (body.secret !== undefined && !/^[0-9]+:[A-Za-z0-9_-]+$/.test(body.secret))) fields.secret = 'Enter a bot token in the form 123456:token.';
  }
  if (kind === 'email') {
    if (!validHost(config.host || '')) fields['config.host'] = 'Enter a mail server host name or IP address (letters, digits and hyphens per label).';
    if (!Number.isInteger(config.port) || config.port < 1 || config.port > 65535) fields['config.port'] = 'Enter a port from 1 to 65535.';
    if (bytes(config.username) > 100 || /[\r\n\x00]/.test(config.username || '')) fields['config.username'] = 'Username is limited to 100 UTF-8 bytes.';
    const address = {test: (value) => bytes(value) <= 254 && /^[\x21-\x7e]+$/.test(value) && /^[^\s@<>]+@[^\s@<>]+$/.test(value)};
    if (!address.test(config.from || '')) fields['config.from'] = 'Enter a sender email address.';
    if (!Array.isArray(config.to) || !config.to.length || config.to.length > 20 || config.to.some((item) => !address.test(item))) fields['config.to'] = 'Enter 1–20 recipient email addresses, separated by commas or new lines.';
    if (config.username && !hasSecret) fields.secret = 'Enter the SMTP password when a username is set.';
    if (body.secret !== undefined && (body.secret.includes('\0') || bytes(config.username) + bytes(body.secret) > 360)) fields.secret = 'SMTP credentials must fit within 360 UTF-8 bytes and contain no NUL character.';
  }
  return fields;
}
