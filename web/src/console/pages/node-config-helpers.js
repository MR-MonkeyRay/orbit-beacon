const UINT64_MAX = 18446744073709551615n;
const QUOTA_UNITS = Object.freeze({bytes: 1n, KB: 1000n, MB: 1000n ** 2n, GB: 1000n ** 3n, TB: 1000n ** 4n});
const textBytes = (value) => new TextEncoder().encode(value).length;

function validDNSName(host) {
  if (!host || host.length > 253) return false;
  const name = host.endsWith('.') ? host.slice(0, -1) : host;
  // A numeric final label makes URL parsers treat the host as IPv4 (e.g. 0x7f.1).
  if (/^(?:\d+|0x[0-9a-f]*)$/i.test(name.split('.').pop())) return false;
  return Boolean(name) && name.split('.').every((label) => /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/.test(label));
}

function validIPv4(host) {
  const parts = host.split('.');
  return parts.length === 4 && parts.every((part) => /^(?:0|[1-9]\d{0,2})$/.test(part) && Number(part) <= 255);
}

function validBracketedIPv6(host) {
  if (!/^\[[^\]]+\]$/.test(host)) return false;
  try {
    const parsed = new URL(`http://${host}/`);
    return parsed.hostname.startsWith('[') && parsed.hostname.endsWith(']') && host.includes(':');
  } catch {
    return false;
  }
}

function validProbeHost(host) {
  if (host.startsWith('[')) return validBracketedIPv6(host);
  if (/^[\d.]+$/.test(host)) return validIPv4(host);
  return validDNSName(host);
}

export function validateProbeTarget(value) {
  if (typeof value !== 'string' || value === '') return 'Probe targets cannot be blank.';
  if (textBytes(value) > 1024) return 'Each probe target is limited to 1024 UTF-8 bytes.';
  if (/[\s\u0085]/u.test(value)) return 'Probe targets cannot contain whitespace.';
  // The server's URL parser rejects ASCII control characters anywhere.
  if (/[\u0000-\u001f\u007f]/.test(value)) return 'Probe targets cannot contain control characters.';

  const tcp = value.match(/^tcp:\/\/(\[[^\]]+\]|[^:/?#@]+):(\d+)$/);
  if (tcp) {
    const port = Number(tcp[2]);
    if (!validProbeHost(tcp[1])) return 'TCP probe targets need a valid DNS name, IPv4 address, or bracketed IPv6 address.';
    if (port < 1 || port > 65535) return 'TCP probe ports must be from 1 to 65535.';
    return '';
  }
  if (value.startsWith('tcp://')) return 'TCP probe targets must use tcp://host:port with a port from 1 to 65535.';

  if (value.startsWith('http://') || value.startsWith('https://')) {
    const authority = value.slice(value.indexOf('//') + 2).split(/[/?#]/, 1)[0];
    if (!authority || value.includes('\\') || authority.includes('@')) return 'HTTP probe targets must include a host and cannot include user information.';
    if (value.includes('#')) return 'HTTP probe targets cannot include a fragment.';
    // Validate the host as written; URL parsers rewrite hosts differently.
    const host = authority.startsWith('[') ? authority.slice(0, authority.indexOf(']') + 1) : authority.replace(/:[^:]*$/, '');
    // Only an optional port may follow the host; anything else (as in [::1]evil)
    // would merge into the neutral host below.
    if (!validProbeHost(host) || !/^(?::\d*)?$/.test(authority.slice(host.length))) return 'HTTP probe targets need a valid DNS name, IPv4 address, or bracketed IPv6 address.';
    // Browsers tolerate malformed path escapes such as %zz; the server's URL parser does not.
    const path = value.slice(value.indexOf('//') + 2 + authority.length).split('?', 1)[0];
    if (/%(?![0-9A-Fa-f]{2})/.test(path)) return 'HTTP probe targets must use valid percent-escapes.';
    // The host is already checked as written, so parse the rest with a neutral
    // host; browser IDNA rules (e.g. rejecting xn--a) must not decide validity.
    const scheme = value.slice(0, value.indexOf('//') + 2);
    try { new URL(scheme + 'h.invalid' + authority.slice(host.length) + value.slice(scheme.length + authority.length)); } catch { return 'HTTP probe targets must be valid absolute URLs.'; }
    return '';
  }

  const icmp = value.match(/^icmp:\/\/(\[[^\]]+\]|[^:/?#@]+)$/);
  if (icmp) return validProbeHost(icmp[1]) ? '' : 'ICMP probe targets need a valid host.';
  if (value.startsWith('icmp://')) return 'ICMP probe targets must use icmp://host without a port or path.';
  return 'Probe targets must use tcp://, http://, https://, or icmp://.';
}

export function validateProbeTargets(values) {
  if (!Array.isArray(values)) return 'Probe targets must be a list.';
  if (values.length > 5) return 'Probe targets can contain at most 5 entries.';
  const seen = new Set();
  for (const value of values) {
    const error = validateProbeTarget(value);
    if (error) return error;
    if (seen.has(value)) return 'Probe targets must be unique.';
    seen.add(value);
  }
  return '';
}

export function quotaToBytes(value, unit = 'bytes') {
  if (typeof value === 'number' && (!Number.isSafeInteger(value) || value < 0)) return {ok: false, error: 'Enter a whole number.'};
  if (typeof value !== 'string' && typeof value !== 'number' && typeof value !== 'bigint') return {ok: false, error: 'Enter a whole number.'};
  const raw = String(value).trim();
  if (!/^\d+$/.test(raw) || !Object.hasOwn(QUOTA_UNITS, unit)) return {ok: false, error: 'Enter a whole number and choose a valid unit.'};
  const bytes = BigInt(raw) * QUOTA_UNITS[unit];
  if (bytes > UINT64_MAX) return {ok: false, error: 'The quota must be at most 18,446,744,073,709,551,615 bytes.'};
  return {ok: true, value: bytes.toString()};
}

export function validateSelection(values, kind) {
  if (!Array.isArray(values)) return `${kind} must be a list.`;
  if (values.length > 8) return `${kind} can contain at most 8 entries.`;
  const seen = new Set();
  for (const value of values) {
    if (typeof value !== 'string' || value === '' || textBytes(value) > (kind === 'interfaces' ? 255 : 1024)) {
      return kind === 'interfaces' ? 'Interfaces must be nonblank and at most 255 UTF-8 bytes.' : 'Disk paths must be at most 1024 UTF-8 bytes.';
    }
    if (seen.has(value)) return `${kind === 'interfaces' ? 'Interfaces' : 'Disk paths'} must be unique.`;
    seen.add(value);
    if (kind === 'interfaces' && /[\u0000\r\n]/.test(value)) return 'Interfaces cannot contain NUL, carriage return, or line feed characters.';
    if (kind === 'interfaces' && /^[\t-\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/u.test(value)) return 'Interfaces cannot be blank.';
    if (kind === 'disk_paths' && (value.includes('\u0000') || !value.startsWith('/'))) return 'Disk paths must be absolute and cannot contain NUL.';
  }
  return '';
}

export function changedConfig(config, values) {
  const patch = {};
  for (const key of ['report_interval_seconds', 'disk_interval_seconds', 'billing_day']) {
    if (values[key] !== undefined && Number(values[key]) !== Number(config[key])) patch[key] = Number(values[key]);
  }
  for (const key of ['interfaces', 'disk_paths', 'probe_targets']) {
    if (values[key] !== undefined && JSON.stringify(values[key]) !== JSON.stringify(config[key] || [])) patch[key] = values[key];
  }
  if (values.quota_bytes !== undefined && String(values.quota_bytes) !== String(config.quota_bytes)) patch.quota_bytes = String(values.quota_bytes);
  return patch;
}

export {QUOTA_UNITS, UINT64_MAX};
