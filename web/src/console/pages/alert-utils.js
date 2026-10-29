// Alert and notification form logic, kept independent of the DOM.
import {getLocale, t} from '../../shared/i18n.js';

const utf8 = new TextEncoder();
const bytes = (value) => utf8.encode(value || '').length;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
export const METRICS = [
  ['cpu_percent', t('alerts.metric.cpu')], ['memory_used_percent', t('alerts.metric.memory')],
  ['swap_used_percent', t('alerts.metric.swap')], ['disk_used_percent', t('alerts.metric.disk')],
  ['load1', t('alerts.metric.load1')], ['probes_down', t('alerts.metric.probesDown')], ['offline', t('alerts.metric.offline')]
];
export const SEVERITIES = [['info', t('alerts.severity.info')], ['warning', t('alerts.severity.warning')], ['critical', t('alerts.severity.critical')]];
export const metricLabel = (metric) => METRICS.find(([id]) => id === metric)?.[1] || t('alerts.metric.unknown');
export const severityLabel = (severity) => SEVERITIES.find(([id]) => id === severity)?.[1] || t('alerts.severity.unknown');
export const metricUnit = (metric = '') => metric.endsWith('_percent') ? '%' : metric === 'load1' ? t('alerts.unit.load') : metric === 'probes_down' ? t('alerts.unit.probes') : '';
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
  if (!metric) return t('alerts.value.unknownMetric', {value: Number.isFinite(value) ? value.toLocaleString(getLocale(), {maximumSignificantDigits: 21}) : t('common.unavailable')});
  if (metric === 'offline') return t('alerts.metric.offline');
  if (!Number.isFinite(value)) return t('common.unavailable');
  if (metricUnit(metric) === '%') return `${Number(value.toFixed(1)).toLocaleString(getLocale(), {maximumFractionDigits: 1})}%`;
  if (metric === 'load1') return t('alerts.value.load', {value: value > 0 && value < .01 ? `< ${(0.01).toLocaleString(getLocale())}` : Number(value.toFixed(2)).toLocaleString(getLocale(), {minimumFractionDigits: 2, maximumFractionDigits: 2})});
  if (metric === 'probes_down') return t('alerts.value.failingProbes', {count: value});
  return t('alerts.value.unknownMetric', {value: value.toLocaleString(getLocale(), {maximumSignificantDigits: 21})});
}
export function formatRuleDuration(seconds) {
  if (!Number.isInteger(seconds) || seconds < 0) return t('common.unavailable');
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  return minutes ? remainder ? t('alerts.duration.minutesSeconds', {minutes, seconds: remainder}) : t('alerts.duration.minutes', {minutes}) : t('alerts.duration.seconds', {seconds: remainder});
}
export function durationSeconds(minutes, seconds) {
  if (String(minutes).trim() === '' || String(seconds).trim() === '') return NaN;
  const m = Number(minutes), s = Number(seconds);
  return Number.isInteger(m) && m >= 0 && Number.isInteger(s) && s >= 0 && s <= 59 ? m * 60 + s : NaN;
}
export function scopeLabel(rule, groups = [], nodes = []) {
  if (rule.scope_kind === 'all') return t('alerts.scope.all');
  const collection = rule.scope_kind === 'group' ? groups : rule.scope_kind === 'node' ? nodes : null;
  if (!collection) return t('alerts.scope.unknown');
  const label = collection.find((item) => item.id === rule.scope_id)?.name || rule.scope_id || t('alerts.unknown');
  return t(rule.scope_kind === 'group' ? 'alerts.scope.groupName' : 'alerts.scope.nodeName', {name: label});
}
export function validateRule(rule) {
  const fields = {};
  if (!rule.name?.trim() || bytes(rule.name) > 100) fields.name = t('alerts.validation.name');
  if (typeof rule.enabled !== 'boolean') fields.enabled = t('alerts.validation.enabled');
  if (!['all', 'group', 'node'].includes(rule.scope_kind)) fields.scope_kind = t('alerts.validation.scope');
  else if (rule.scope_kind === 'all' ? rule.scope_id != null : !uuid.test(rule.scope_id || '')) fields.scope_id = t(rule.scope_kind === 'all' ? 'alerts.validation.allScopeID' : 'alerts.validation.scopeID');
  if (!METRICS.some(([id]) => rule.metric === id)) fields.metric = t('alerts.validation.metric');
  if (rule.metric === 'offline') {
    if (rule.comparator != null) fields.comparator = t('alerts.validation.offlineComparator');
    if (rule.threshold != null) fields.threshold = t('alerts.validation.offlineThreshold');
  } else {
    if (!['>', '<'].includes(rule.comparator)) fields.comparator = t('alerts.validation.comparator');
    const max = metricMax(rule.metric);
    if (!Number.isFinite(rule.threshold) || rule.threshold < 0 || rule.threshold > max) fields.threshold = t('alerts.validation.threshold', {max});
  }
  const minimum = rule.metric === 'offline' ? 180 : 0;
  if (!Number.isInteger(rule.for_seconds) || rule.for_seconds < minimum || rule.for_seconds > 86400) fields.for_seconds = t(minimum ? 'alerts.validation.offlineDuration' : 'alerts.validation.duration');
  if (!SEVERITIES.some(([id]) => rule.severity === id)) fields.severity = t('alerts.validation.severity');
  if (!Array.isArray(rule.channel_ids) || rule.channel_ids.length > 20 || new Set(rule.channel_ids).size !== rule.channel_ids.length || rule.channel_ids.some((id) => !uuid.test(id))) fields.channel_ids = t('alerts.validation.channels');
  return fields;
}
export function validationError(fields) {
  return Object.assign(new Error(t('alerts.validation.checkFields')), {code: 'validation_failed', fields, clientValidation: true});
}

const ERROR_KEYS = {
  secret_unreadable: 'secretUnreadable', secret_unavailable: 'secretUnavailable',
  destination_denied: 'destinationDenied', insecure_webhook_denied: 'insecureWebhookDenied',
  invalid_destination: 'invalidDestination', http_rejected: 'httpRejected',
  remote_unavailable: 'remoteUnavailable', network_error: 'networkError',
  redirect_denied: 'redirectDenied', telegram_rejected: 'telegramRejected',
  smtp_rejected: 'smtpRejected', smtp_protocol: 'smtpProtocol',
  starttls_required: 'starttlsRequired', invalid_payload: 'invalidPayload',
  rate_limited: 'rateLimited', channel_disabled: 'channelDisabled',
  attempts_exhausted: 'attemptsExhausted', canceled: 'canceled',
  notification_channel_capacity_reached: 'channelCapacity', alert_rule_capacity_reached: 'ruleCapacity'
};
export function errorCodeMessage(code) {
  return t(`alerts.error.${Object.hasOwn(ERROR_KEYS, code) ? ERROR_KEYS[code] : code ? 'unknown' : 'none'}`);
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
  if (!body.name?.trim() || bytes(body.name) > 100) fields.name = t('alerts.validation.name');
  if (!['webhook', 'telegram', 'email'].includes(kind)) fields.kind = t('alerts.validation.kind');
  if (body.secret !== undefined && (typeof body.secret !== 'string' || bytes(body.secret) > 4096)) fields.secret = t('alerts.validation.secret');
  const hasSecret = body.secret === undefined ? existing?.secret_set : Boolean(body.secret);
  if (kind === 'webhook' && (config.url !== undefined || !existing?.config?.url_set)) {
    try {
      const url = new URL(config.url);
      const loopback = url.hostname === '[::1]' || /^127(?:\.\d{1,3}){3}$/.test(url.hostname);
      if (url.username || url.password || url.hash || !['https:', 'http:'].includes(url.protocol) || (url.protocol === 'http:' && !loopback)) throw new Error();
    } catch { fields['config.url'] = t('alerts.validation.webhookURL'); }
  }
  if (kind === 'telegram') {
    if (!/^(?:-?[0-9]{1,20}|@[A-Za-z][A-Za-z0-9_]{4,31})$/.test(config.chat_id || '')) fields['config.chat_id'] = t('alerts.validation.chatID');
    if (!hasSecret || (body.secret !== undefined && !/^[0-9]+:[A-Za-z0-9_-]+$/.test(body.secret))) fields.secret = t('alerts.validation.botToken');
  }
  if (kind === 'email') {
    if (!validHost(config.host || '')) fields['config.host'] = t('alerts.validation.smtpHost');
    if (!Number.isInteger(config.port) || config.port < 1 || config.port > 65535) fields['config.port'] = t('alerts.validation.smtpPort');
    if (bytes(config.username) > 100 || /[\r\n\x00]/.test(config.username || '')) fields['config.username'] = t('alerts.validation.smtpUsername');
    const address = {test: (value) => bytes(value) <= 254 && /^[\x21-\x7e]+$/.test(value) && /^[^\s@<>]+@[^\s@<>]+$/.test(value)};
    if (!address.test(config.from || '')) fields['config.from'] = t('alerts.validation.smtpFrom');
    if (!Array.isArray(config.to) || !config.to.length || config.to.length > 20 || config.to.some((item) => !address.test(item))) fields['config.to'] = t('alerts.validation.smtpTo');
    if (config.username && !hasSecret) fields.secret = t('alerts.validation.smtpPassword');
    if (body.secret !== undefined && (body.secret.includes('\0') || bytes(config.username) + bytes(body.secret) > 360)) fields.secret = t('alerts.validation.smtpCredentials');
  }
  return fields;
}
