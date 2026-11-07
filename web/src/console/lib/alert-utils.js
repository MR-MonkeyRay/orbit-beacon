// 告警与通知表单逻辑，与 DOM 解耦。
import {getLocale, t} from '../i18n.js';
import {byteLength} from '../../shared/dom.js';
import {formatMilliseconds} from '../format.js';
import {uuid} from '../../shared/uuid.js';
export const METRICS = [
  ['cpu_percent', t('alerts.metric.cpu')], ['memory_used_percent', t('alerts.metric.memory')],
  ['swap_used_percent', t('alerts.metric.swap')], ['disk_used_percent', t('alerts.metric.disk')],
  ['traffic_quota_percent', t('alerts.metric.trafficQuota')], ['node_expiry_days', t('alerts.metric.nodeExpiry')], ['load1', t('alerts.metric.load1')], ['probes_down', t('alerts.metric.probesDown')], ['probe_failed', t('alerts.metric.probeFailed')], ['probe_latency_ms', t('alerts.metric.probeLatency')], ['probe_tls_expiry_days', t('alerts.metric.probeTLSExpiry')], ['offline', t('common.status.offline')]
];
export const SEVERITIES = [['info', t('alerts.severity.info')], ['warning', t('alerts.severity.warning')], ['critical', t('alerts.severity.critical')]];
export const metricLabel = (metric) => METRICS.find(([id]) => id === metric)?.[1] || t('alerts.metric.unknown');
export const severityLabel = (severity) => SEVERITIES.find(([id]) => id === severity)?.[1] || t('alerts.severity.unknown');
export const metricUnit = (metric = '') => metric.endsWith('_percent') ? '%' : metric === 'load1' ? t('alerts.unit.load') : metric === 'probes_down' ? t('alerts.unit.probes') : '';
export const isProbeMetric = (metric) => ['probe_failed', 'probe_latency_ms', 'probe_tls_expiry_days'].includes(metric);
// 各指标阈值上限，与服务端规则校验一致。
export const metricMax = (metric) => metric === 'probe_failed' ? 0 : metric === 'probe_latency_ms' ? 60000 : metric === 'probe_tls_expiry_days' ? 3650 : metric === 'node_expiry_days' ? 365 : metric === 'traffic_quota_percent' ? 1000 : metric === 'load1' ? 1000000 : metric === 'probes_down' ? 5 : 100;
// 与服务端 notify.validHost 一致：IP 字面量或 LDH 标签。
export function validHost(host) {
  if (!host || host.length > 253 || host.includes('%')) return false;
  if (/^\d{1,3}(?:\.\d{1,3}){3}$/.test(host) && host.split('.').every((part) => Number(part) <= 255)) return true;
  if (host.includes(':')) { try { return new URL(`http://[${host}]/`).hostname.length > 2; } catch { return false; } }
  return host.replace(/\.$/, '').split('.').every((label) => /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/.test(label));
}

export function formatMetricValue(metric, value) {
  if (!metric) return t('alerts.value.unknownMetric', {value: Number.isFinite(value) ? value.toLocaleString(getLocale(), {maximumSignificantDigits: 21}) : t('common.unavailable')});
  if (metric === 'offline') return t('common.status.offline');
  if (!Number.isFinite(value)) return t('common.unavailable');
  if (metricUnit(metric) === '%') return `${Number(value.toFixed(1)).toLocaleString(getLocale(), {maximumFractionDigits: 1})}%`;
  if (metric === 'probe_latency_ms') return formatMilliseconds(value);
  if (metric === 'probe_failed') return t(value ? 'nodes.probe.down' : 'nodes.probe.up');
  if (metric === 'node_expiry_days' || metric === 'probe_tls_expiry_days') return t('alerts.value.expiryDays', {value: Number(value.toFixed(1)).toLocaleString(getLocale(), {maximumFractionDigits: 1})});
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
  if (!rule.name?.trim() || byteLength(rule.name || '') > 100) fields.name = t('alerts.validation.name');
  if (typeof rule.enabled !== 'boolean') fields.enabled = t('alerts.validation.enabled');
  if (!['all', 'group', 'node'].includes(rule.scope_kind)) fields.scope_kind = t('alerts.validation.scope');
  else if (rule.scope_kind === 'all' ? rule.scope_id != null : !uuid.test(rule.scope_id || '')) fields.scope_id = t(rule.scope_kind === 'all' ? 'alerts.validation.allScopeID' : 'alerts.validation.scopeID');
  if (!METRICS.some(([id]) => rule.metric === id)) fields.metric = t('alerts.validation.metric');
  if (isProbeMetric(rule.metric) ? typeof rule.target !== 'string' || !rule.target || byteLength(rule.target) > 1024 || /\s/.test(rule.target) : rule.target != null) fields.target = t('alerts.validation.probeTarget');
  if (rule.metric === 'offline') {
    if (rule.comparator != null) fields.comparator = t('alerts.validation.offlineComparator');
    if (rule.threshold != null) fields.threshold = t('alerts.validation.offlineThreshold');
  } else {
    if (rule.metric === 'probe_failed' ? rule.comparator !== '>' : ['node_expiry_days', 'probe_tls_expiry_days'].includes(rule.metric) ? rule.comparator !== '<' : !['>', '<'].includes(rule.comparator)) fields.comparator = t('alerts.validation.comparator');
    const max = metricMax(rule.metric);
    if (!Number.isFinite(rule.threshold) || rule.threshold < 0 || (rule.metric === 'node_expiry_days' && rule.threshold < 1) || (['traffic_quota_percent', 'node_expiry_days', 'probe_latency_ms', 'probe_tls_expiry_days'].includes(rule.metric) && rule.threshold === 0) || rule.threshold > max) fields.threshold = rule.metric === 'node_expiry_days' ? t('alerts.validation.expiryThreshold') : rule.metric === 'traffic_quota_percent' ? t('alerts.validation.quotaThreshold') : t('alerts.validation.threshold', {max});
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
  redirect_denied: 'redirectDenied', telegram_rejected: 'telegramRejected', channel_rejected: 'channelRejected',
  smtp_rejected: 'smtpRejected', smtp_protocol: 'smtpProtocol',
  starttls_required: 'starttlsRequired', invalid_payload: 'invalidPayload',
  rate_limited: 'rateLimited', channel_disabled: 'channelDisabled',
  attempts_exhausted: 'attemptsExhausted', canceled: 'canceled',
  notification_channel_capacity_reached: 'channelCapacity', alert_rule_capacity_reached: 'ruleCapacity'
};
export function errorCodeMessage(code) {
  return t(`alerts.error.${Object.hasOwn(ERROR_KEYS, code) ? ERROR_KEYS[code] : code ? 'unknown' : 'none'}`);
}

export const URL_CHANNELS = {
  webhook: {placeholder: 'https://example.com/hook'},
  feishu: {hosts: ['open.feishu.cn', 'open.larksuite.com'], path: /^\/open-apis\/bot\/v2\/hook\/[^/]+$/, placeholder: 'https://open.feishu.cn/open-apis/bot/v2/hook/…'},
  dingtalk: {hosts: ['oapi.dingtalk.com'], path: /^\/robot\/send$/, query: 'access_token', placeholder: 'https://oapi.dingtalk.com/robot/send?access_token=…'},
  wecom: {hosts: ['qyapi.weixin.qq.com'], path: /^\/cgi-bin\/webhook\/send$/, query: 'key', placeholder: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…'},
  bark: {path: /^\/?$/, placeholder: 'https://api.day.app'},
  ntfy: {path: /^(?:\/[A-Za-z0-9_-]+)+$/, placeholder: 'https://ntfy.sh/topic'}
};
export const usesURL = (kind) => Object.hasOwn(URL_CHANNELS, kind);
// 服务端是权威校验；这里只拦截常见错误，给出即时反馈。
function validChannelURL(kind, raw) {
  const policy = URL_CHANNELS[kind];
  let url;
  try { url = new URL(raw); } catch { return false; }
  const loopback = url.hostname === '[::1]' || /^127(?:\.\d{1,3}){3}$/.test(url.hostname);
  const secure = url.protocol === 'https:' || (url.protocol === 'http:' && loopback && !policy.hosts);
  if (!secure || url.username || url.password || raw.includes('#')) return false;
  if (policy.hosts && !policy.hosts.includes(url.host)) return false;
  if (policy.path && !policy.path.test(url.pathname)) return false;
  if (policy.query) {
    const entries = [...url.searchParams];
    return entries.length === 1 && entries[0][0] === policy.query && Boolean(entries[0][1]);
  }
  return kind === 'webhook' || !url.search;
}

// 未修改的只写字段必须省略；将 secret 设为空字符串表示显式清除。
export function buildChannelPayload(values, {editing = false, replaceURL = false, replaceSecret = false} = {}) {
  const body = {name: values.name, enabled: values.enabled};
  if (!editing) body.kind = values.kind;
  if (usesURL(values.kind)) body.config = !editing || replaceURL ? {url: values.url} : {};
  else if (values.kind === 'telegram') body.config = {chat_id: values.chat_id};
  else if (values.kind === 'email') body.config = {
    host: values.host, port: values.port === '' ? NaN : Number(values.port), username: values.username || '',
    from: values.from, to: Array.isArray(values.to) ? values.to : String(values.to || '').split(/[,\n]/).map((item) => item.trim()).filter(Boolean)
  };
  if (values.kind !== 'webhook' && body.config && values.template !== undefined) body.config.template = values.template.replace(/\r\n/g, '\n').trim();
  if (values.kind !== 'wecom' && (replaceSecret || values.secret)) body.secret = values.secret ?? '';
  return body;
}
export const MESSAGE_TEMPLATE_TOKENS = ['{state}', '{rule}', '{node}', '{metric}', '{target}', '{severity}', '{value}', '{started_at}', '{resolved_at}', '{event_id}'];
export function validateChannel(body, {kind = body.kind, existing = null} = {}) {
  const fields = {};
  const config = body.config || {};
  if (config.template !== undefined && (kind === 'webhook' || typeof config.template !== 'string' || /[\uD800-\uDFFF]/u.test(config.template) || byteLength(config.template) > 1000 || /[\p{Cc}]/u.test(config.template.replace(/\n/g, '')) || [...config.template.matchAll(/\{[a-z_]+\}/g)].some(([token]) => !MESSAGE_TEMPLATE_TOKENS.includes(token)))) fields['config.template'] = t('alerts.validation.template');
  if (!body.name?.trim() || byteLength(body.name || '') > 100) fields.name = t('alerts.validation.name');
  if (!usesURL(kind) && !['telegram', 'email'].includes(kind)) fields.kind = t('alerts.validation.kind');
  if (body.secret !== undefined && (typeof body.secret !== 'string' || byteLength(body.secret || '') > 4096)) fields.secret = t('alerts.validation.secret');
  const hasSecret = body.secret === undefined ? existing?.secret_set : Boolean(body.secret);
  if (usesURL(kind) && (config.url !== undefined || !existing?.config?.url_set) && !validChannelURL(kind, config.url)) fields['config.url'] = t('alerts.validation.channelURL');
  if (kind === 'wecom' && hasSecret) fields.secret = t('alerts.validation.channelSecret');
  if (kind === 'bark' && (!hasSecret || (body.secret !== undefined && !/^[A-Za-z0-9_-]{1,128}$/.test(body.secret)))) fields.secret = t('alerts.validation.channelSecret');
  if (kind === 'ntfy' && body.secret !== undefined && /[\x00-\x1f\x7f]/.test(body.secret)) fields.secret = t('alerts.validation.channelSecret');
  if (kind === 'telegram') {
    if (!/^(?:-?[0-9]{1,20}|@[A-Za-z][A-Za-z0-9_]{4,31})$/.test(config.chat_id || '')) fields['config.chat_id'] = t('alerts.validation.chatID');
    if (!hasSecret || (body.secret !== undefined && !/^[0-9]+:[A-Za-z0-9_-]+$/.test(body.secret))) fields.secret = t('alerts.validation.botToken');
  }
  if (kind === 'email') {
    if (!validHost(config.host || '')) fields['config.host'] = t('alerts.validation.smtpHost');
    if (!Number.isInteger(config.port) || config.port < 1 || config.port > 65535) fields['config.port'] = t('alerts.validation.smtpPort');
    if (byteLength(config.username || '') > 100 || /[\r\n\x00]/.test(config.username || '')) fields['config.username'] = t('alerts.validation.smtpUsername');
    const address = {test: (value) => byteLength(value || '') <= 254 && /^[\x21-\x7e]+$/.test(value) && /^[^\s@<>]+@[^\s@<>]+$/.test(value)};
    if (!address.test(config.from || '')) fields['config.from'] = t('alerts.validation.smtpFrom');
    if (!Array.isArray(config.to) || !config.to.length || config.to.length > 20 || config.to.some((item) => !address.test(item))) fields['config.to'] = t('alerts.validation.smtpTo');
    if (config.username && !hasSecret) fields.secret = t('alerts.validation.smtpPassword');
    if (body.secret !== undefined && (body.secret.includes('\0') || byteLength(config.username || '') + byteLength(body.secret || '') > 360)) fields.secret = t('alerts.validation.smtpCredentials');
  }
  return fields;
}
