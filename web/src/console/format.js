import {h} from '../shared/dom.js';

export const STATUS = {online: 'Online', offline: 'Offline', never: 'Never seen', unknown: 'Unknown', pending: 'Pending', disabled: 'Disabled'};
export const STATUS_ORDER = ['online', 'offline', 'never', 'pending', 'disabled'];

const ACTIONS = {
  'node.create': 'Node created', 'node.delete': 'Node deleted', 'node.disable': 'Node disabled', 'node.enable': 'Node enabled',
  'node.metadata.patch': 'Node details updated', 'node.config.patch': 'Desired configuration updated',
  'node.traffic_settings.patch': 'Traffic billing settings saved',
  'public_page.update': 'Public page access updated',
  'node.public_settings.patch': 'Public page settings saved', 'group.create': 'Group created', 'group.update': 'Group renamed',
  'group.delete': 'Group deleted', 'user.create': 'Administrator added', 'user.patch': 'Administrator renamed', 'user.delete': 'Administrator deleted',
  'user.password.change': 'Password changed', 'enrollment.create': 'Enrollment credential issued',
  'enrollment.claim': 'Agent enrolled', 'enrollment.revoke': 'Enrollment credential revoked',
  'session.create': 'Signed in', 'session.delete': 'Signed out',
  'node.credential.revoke': 'Revoked agent credential'
};

export function describeAction(action) {
  return ACTIONS[action] || action.replace(/[._]/g, ' ').replace(/^./, (letter) => letter.toUpperCase());
}

const dateFormat = new Intl.DateTimeFormat('en', {dateStyle: 'medium', timeStyle: 'short'});
const relativeFormat = new Intl.RelativeTimeFormat('en', {numeric: 'auto'});
const observationFormat = new Intl.DateTimeFormat('en', {dateStyle: 'medium', timeStyle: 'medium'});

export function relative(date) {
  const seconds = (date.getTime() - Date.now()) / 1000;
  const abs = Math.abs(seconds);
  if (abs < 45) return 'just now';
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
    if (value === 0n) return '0 B';
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB'];
    let unit = 0;
    let remainder = 0n;
    while (value >= 1024n && unit < units.length - 1) { remainder = value % 1024n; value /= 1024n; unit += 1; }
    const fraction = unit ? Number(remainder * 10n / 1024n) : 0;
    return `${value}${fraction ? `.${fraction}` : ''} ${units[unit]}`;
  } catch { return decimal; }
}

export function formatPercent(value) {
  return Number.isFinite(value) ? `${Number(value.toFixed(1))}%` : 'Unavailable';
}

export function formatLoad(value) {
  return Number.isFinite(value) && value >= 0 ? value.toFixed(2) : 'Unavailable';
}

export function formatDuration(seconds) {
  if (!Number.isFinite(seconds) || seconds < 0) return 'Unavailable';
  const total = Math.floor(seconds);
  const days = Math.floor(total / 86400);
  const hours = Math.floor(total % 86400 / 3600);
  const minutes = Math.floor(total % 3600 / 60);
  if (days) return `${days}d ${hours}h`;
  if (hours) return `${hours}h ${minutes}m`;
  if (minutes) return `${minutes}m`;
  return `${total}s`;
}

export function formatRate(bytesPerSecond) {
  return Number.isFinite(bytesPerSecond) && bytesPerSecond >= 0 ? `${formatBytes(Math.round(bytesPerSecond))}/s` : 'Unavailable';
}

