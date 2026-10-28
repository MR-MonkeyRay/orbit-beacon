export function apiTokenState(token, now = Date.now()) {
  if (token.revoked_at) return 'Revoked';
  if (token.expires_at && Date.parse(token.expires_at) <= now) return 'Expired';
  return 'Active';
}

export function apiTokenExpiry(value) {
  if (value === 'never') return null;
  if (!/^\d+$/.test(value)) throw new Error('Choose an expiry from 1 to 365 days.');
  const days = Number(value);
  if (!Number.isInteger(days) || days < 1 || days > 365) throw new Error('Choose an expiry from 1 to 365 days.');
  return days;
}
