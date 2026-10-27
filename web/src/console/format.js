import {h} from '../shared/dom.js';

export const STATUS = {online: 'Online', offline: 'Offline', unknown: 'Unknown', pending: 'Pending', disabled: 'Disabled'};
export const STATUS_ORDER = ['online', 'offline', 'unknown', 'pending', 'disabled'];
export const ROLE = {admin: 'Administrator', viewer: 'Viewer'};

const ACTIONS = {
  'node.create': 'Node created', 'node.delete': 'Node deleted', 'node.disable': 'Node disabled', 'node.enable': 'Node enabled',
  'node.metadata.patch': 'Node details updated', 'node.config.patch': 'Desired configuration updated',
  'node.public_settings.patch': 'Public page settings saved', 'group.create': 'Group created', 'group.update': 'Group renamed',
  'group.delete': 'Group deleted', 'user.create': 'User created', 'user.patch': 'User updated', 'user.delete': 'User deleted',
  'user.password.change': 'Password changed', 'enrollment.create': 'Enrollment credential issued',
  'enrollment.claim': 'Agent enrolled', 'enrollment.revoke': 'Enrollment credential revoked'
};

export function describeAction(action) {
  return ACTIONS[action] || action.replace(/[._]/g, ' ').replace(/^./, (letter) => letter.toUpperCase());
}

const dateFormat = new Intl.DateTimeFormat('en', {dateStyle: 'medium', timeStyle: 'short'});
const relativeFormat = new Intl.RelativeTimeFormat('en', {numeric: 'auto'});

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
