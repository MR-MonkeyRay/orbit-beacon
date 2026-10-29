import {getLocale, t} from './i18n.js';
import {statusLabels} from '../shared/status.js';
import {h} from '../shared/dom.js';

export const STATUS = statusLabels();
export const STATUS_ORDER = ['online', 'offline', 'never', 'pending', 'disabled'];

const ACTIONS = {
  'node.create': t('audit.nodeCreate'), 'node.delete': t('audit.nodeDelete'), 'node.disable': t('audit.nodeDisable'), 'node.enable': t('audit.nodeEnable'),
  'node.metadata.patch': t('audit.nodeMetadata'), 'node.config.patch': t('audit.nodeConfig'),
  'node.traffic_settings.patch': t('audit.nodeTraffic'),
  'public_page.update': t('audit.publicPage'),
  'node.public_settings.patch': t('audit.nodePublic'), 'group.create': t('audit.groupCreate'), 'group.update': t('audit.groupUpdate'),
  'group.delete': t('audit.groupDelete'), 'user.create': t('audit.userCreate'), 'user.patch': t('audit.userPatch'), 'user.delete': t('audit.userDelete'),
  'user.password.change': t('audit.password'), 'enrollment.create': t('audit.enrollmentCreate'),
  'enrollment.claim': t('audit.enrollmentClaim'), 'enrollment.revoke': t('audit.enrollmentRevoke'),
  'session.create': t('audit.signIn'), 'session.delete': t('audit.signOut'),
  'node.credential.revoke': t('audit.credentialRevoke'),
  'alert_rule.create': t('audit.ruleCreate'), 'alert_rule.update': t('audit.ruleUpdate'), 'alert_rule.delete': t('audit.ruleDelete'),
  'notification_channel.create': t('audit.channelCreate'), 'notification_channel.update': t('audit.channelUpdate'),
  'notification_channel.delete': t('audit.channelDelete'), 'notification_channel.test': t('audit.channelTest'),
  'backup.plan.create': t('audit.backupPlanCreate'), 'backup.plan.update': t('audit.backupPlanUpdate'), 'backup.plan.delete': t('audit.backupPlanDelete'),
  'backup.restore': t('audit.backupRestore'), 'backup.schedule': t('audit.backupSchedule'),
  'backup.export': t('audit.backupExport'), 'backup.password': t('audit.backupPassword')
};

export function describeAction(action) {
  return Object.hasOwn(ACTIONS, action) ? ACTIONS[action] : t('nodes.audit.unknown');
}

const dateFormat = new Intl.DateTimeFormat(getLocale(), {dateStyle: 'medium', timeStyle: 'short'});
const relativeFormat = new Intl.RelativeTimeFormat(getLocale(), {numeric: 'auto'});
const observationFormat = new Intl.DateTimeFormat(getLocale(), {dateStyle: 'medium', timeStyle: 'medium'});
const integerFormat = new Intl.NumberFormat(getLocale(), {useGrouping: false, maximumFractionDigits: 0});
const percentFormat = new Intl.NumberFormat(getLocale(), {useGrouping: false, maximumFractionDigits: 1});
const loadFormat = new Intl.NumberFormat(getLocale(), {useGrouping: false, minimumFractionDigits: 2, maximumFractionDigits: 2});
const decimalSeparator = percentFormat.formatToParts(1.1).find((part) => part.type === 'decimal').value;

export function relative(date) {
  const seconds = (date.getTime() - Date.now()) / 1000;
  const abs = Math.abs(seconds);
  if (abs < 45) return t('nodes.justNow');
  if (abs < 3600) return relativeFormat.format(Math.round(seconds / 60), 'minute');
  if (abs < 86400) return relativeFormat.format(Math.round(seconds / 3600), 'hour');
  if (abs < 86400 * 30) return relativeFormat.format(Math.round(seconds / 86400), 'day');
  return dateFormat.format(date);
}

// Relative time with the local and UTC timestamps on hover.
export function when(iso) {
  const date = new Date(iso);
  if (!Number.isFinite(date.getTime())) return h('span', null, iso);
  return h('time', {datetime: iso, title: `${dateFormat.format(date)} (UTC ${iso})`}, relative(date));
}

// Quiet live streams do not repaint unchanged observations. An absolute clock
// cannot turn into a permanently misleading "just now" label.
export function absoluteWhen(iso) {
  const date = new Date(iso);
  if (!Number.isFinite(date.getTime())) return h('span', null, iso);
  return h('time', {datetime: iso, title: `UTC ${iso}`}, observationFormat.format(date));
}

export const shortID = (id) => (id ? id.slice(0, 8) : '');

// quota_bytes is a decimal string up to 2^64-1, so it is formatted with BigInt.
export function formatBytes(decimal) {
  try {
    let value = BigInt(decimal);
    if (value === 0n) return `${integerFormat.format(value)} B`;
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB'];
    let unit = 0;
    let remainder = 0n;
    while (value >= 1024n && unit < units.length - 1) { remainder = value % 1024n; value /= 1024n; unit += 1; }
    const fraction = unit ? remainder * 10n / 1024n : 0n;
    return `${integerFormat.format(value)}${fraction ? `${decimalSeparator}${integerFormat.format(fraction)}` : ''} ${units[unit]}`;
  } catch { return decimal; }
}

export function formatPercent(value) {
  return Number.isFinite(value) ? `${percentFormat.format(Number(value.toFixed(1)))}%` : t('common.unavailable');
}

export function formatLoad(value) {
  return Number.isFinite(value) && value >= 0 ? loadFormat.format(Number(value.toFixed(2))) : t('common.unavailable');
}

export function formatDuration(seconds) {
  if (!Number.isFinite(seconds) || seconds < 0) return t('common.unavailable');
  const total = Math.floor(seconds);
  const days = Math.floor(total / 86400);
  const hours = Math.floor(total % 86400 / 3600);
  const minutes = Math.floor(total % 3600 / 60);
  if (days) return t('nodes.duration.days', {days, hours});
  if (hours) return t('nodes.duration.hours', {hours, minutes});
  if (minutes) return t('nodes.duration.minutes', {minutes});
  return t('nodes.duration.seconds', {seconds: total});
}

export function formatRate(bytesPerSecond) {
  return Number.isFinite(bytesPerSecond) && bytesPerSecond >= 0 ? t('nodes.rate', {value: formatBytes(Math.round(bytesPerSecond))}) : t('common.unavailable');
}

