import test from 'node:test';
import assert from 'node:assert/strict';
import {enrollmentCommand, enrollmentCountdown, enrollmentState, installerCommand, nodeEnrollments, shellQuote} from '../src/console/pages/enrollment-utils.js';

test('enrollment command quotes token literally and uses private file + policy', () => {
  assert.equal(shellQuote("a'b"), "'a'\\''b'");
  const command = enrollmentCommand('secret-token');
  assert.match(command, /^install -m 0600 \/dev\/null \/var\/lib\/orbitbeacon-agent\/enrollment.token &&/);
  assert.match(command, /printf '%s\\n' 'secret-token' > \/var\/lib\/orbitbeacon-agent\/enrollment.token/);
  assert.match(command, /orbitbeacon-agent enroll --install-policy \/etc\/orbitbeacon\/install-policy.json --token-file \/var\/lib\/orbitbeacon-agent\/enrollment.token$/);
  assert.doesNotMatch(command, /https?:\/\//);
});

test('installer command pipes the quoted token through stdin, never as an argument', () => {
  const command = installerCommand("se'cret", 'https://monitor.example.com');
  assert.equal(command, `printf '%s\\n' 'se'\\''cret' |\nsudo sh ./install-agent.sh --server 'https://monitor.example.com' --version "$VERSION"`);
  assert.doesNotMatch(command.split('|')[1], /cret/);
});

test('ticket countdown covers expiry and rounds up remaining seconds', () => {
  const expiry = '2026-10-27T12:15:00Z';
  const now = Date.parse('2026-10-27T12:00:00Z');
  assert.equal(enrollmentCountdown(expiry, now), '15:00 remaining');
  assert.equal(enrollmentCountdown(expiry, now + 899001), '0:01 remaining');
  assert.equal(enrollmentCountdown(expiry, now + 900000), 'Expired');
  assert.equal(enrollmentCountdown(expiry, now + 901000), 'Expired');
  assert.equal(enrollmentCountdown('bad', now), 'Expired');
});

test('ticket state includes revoked, expired, exhausted and active metadata', () => {
  const now = Date.parse('2026-10-27T12:00:00Z');
  const ticket = {expires_at: '2026-10-27T12:15:00Z', uses: 0, max_uses: 1};
  assert.equal(enrollmentState(ticket, now), 'Active');
  assert.equal(enrollmentState({...ticket, uses: 1}, now), 'Used');
  assert.equal(enrollmentState(ticket, now + 900000), 'Expired');
  assert.equal(enrollmentState({...ticket, revoked_at: '2026-10-27T11:59:00Z'}, now), 'Revoked');
});

test('node tickets are filtered after reading every page, including unrelated full pages', async () => {
  const calls = [];
  const first = Array.from({length: 100}, (_, n) => ({id: `other-${n}`, node_id: 'other'}));
  const wanted = {id: 'wanted', node_id: 'node'};
  const tickets = await nodeEnrollments('node', async (path) => {
    calls.push(path);
    return calls.length === 1
      ? {enrollments: first, has_more: true, next_offset: 73}
      : {enrollments: [wanted, {id: 'group', group_id: 'group'}], has_more: false};
  });
  assert.deepEqual(tickets, [wanted]);
  assert.deepEqual(calls, ['/enrollments?limit=100&offset=0', '/enrollments?limit=100&offset=73']);
});
