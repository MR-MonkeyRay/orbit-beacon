import test from 'node:test';
import assert from 'node:assert/strict';
import {apiTokenExpiry, apiTokenState} from '../src/console/pages/api-token-utils.js';

test('API token expiry only allows bounded whole days or explicit never', () => {
  assert.equal(apiTokenExpiry('never'), null);
  for (const value of ['1', '30', '365']) assert.equal(apiTokenExpiry(value), Number(value));
  for (const value of ['', '0', '366', '-1', '1.5', 'NaN', ' 30', '1e2']) assert.throws(() => apiTokenExpiry(value));
});

test('API token state handles non-expiring tokens and exact expiry', () => {
  const now = Date.parse('2026-10-27T12:00:00Z');
  assert.equal(apiTokenState({expires_at: null}, now), 'Active');
  assert.equal(apiTokenState({expires_at: '2026-10-27T12:00:01Z'}, now), 'Active');
  assert.equal(apiTokenState({expires_at: '2026-10-27T12:00:00Z'}, now), 'Expired');
  assert.equal(apiTokenState({expires_at: '2026-10-26T12:00:00Z', revoked_at: '2026-10-25T12:00:00Z'}, now), 'Revoked');
});
