import test from 'node:test';
import assert from 'node:assert/strict';
import {copy, flush, installDOM} from './console-dom.js';
import {enrollmentCommand, enrollmentCountdown, enrollmentState, installerCommand, nodeEnrollments, shellQuote} from '../src/console/lib/enrollment-utils.js';
import {issueEnrollment} from '../src/console/pages/enrollments.js';
import {closeDialogs} from '../src/console/ui/dialogs.js';

test('enrollment command reads a ticket privately and includes policy and runtime config', () => {
  assert.equal(shellQuote("a'b"), "'a'\\''b'");
  const command = enrollmentCommand('secret-token');
  assert.doesNotMatch(command, /secret-token/);
  assert.match(command, /install -m 0600 \/dev\/null \/opt\/orbit-beacon\/state\/enrollment.token/);
  assert.match(command, /IFS= read -r token/);
  assert.match(command, /printf '%s\\n' "\$token" > \/opt\/orbit-beacon\/state\/enrollment.token/);
  assert.match(command, /\/opt\/orbit-beacon\/bin\/orbitbeacon-agent enroll --install-policy \/opt\/orbit-beacon\/etc\/install-policy.json --config \/opt\/orbit-beacon\/etc\/agent.json --token-file \/opt\/orbit-beacon\/state\/enrollment.token/);
  assert.doesNotMatch(command, /https?:\/\//);
});

test('installer command quotes only the origin and leaves ticket input to the installer', () => {
  const command = installerCommand('https://monitor.example.com', 'secret-token');
  assert.equal(command, `sudo sh ./install-agent.sh --server 'https://monitor.example.com' --version "$VERSION"`);
  assert.doesNotMatch(command, /secret-token/);
  const origin = "https://monitor.example.com/a'b;$(exit 99)";
  assert.equal(installerCommand(origin), `sudo sh ./install-agent.sh --server ${shellQuote(origin)} --version "$VERSION"`);
});

test('ticket dialog copies ticket and commands separately and clears secrets on close', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const originalNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  const copied = [];
  const secret = "single-use-se'cret";
  location.origin = 'https://monitor.example.com';
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {clipboard: {writeText: async (text) => { copied.push(text); }}}});
  globalThis.fetch = async (path, options) => {
    assert.equal(path, '/api/v1/enrollments');
    assert.equal(options.method, 'POST');
    return new Response(JSON.stringify({token: secret, expires_at: new Date(Date.now() + 900000).toISOString()}));
  };
  try {
    for (const mode of ['single', 'batch']) {
      const pending = issueEnrollment({mode}, copy('nodes.enrollAgent'));
      await flush();
      const dialog = dom.body.querySelector('dialog');
      const blocks = dialog.querySelectorAll('code');
      assert.deepEqual(blocks.map((block) => block.textContent), [secret, installerCommand(location.origin), enrollmentCommand()]);
      assert.equal(dialog.textContent.split(secret).length, 2);
      assert.equal(dialog.querySelector('.text-note').getAttribute('role'), 'status');
      assert.equal(dialog.querySelector('.text-note').textContent, '');
      assert.equal(dialog.querySelector('.callout'), null);
      for (const key of ['copyToken', 'copyInstall', 'copyEnroll']) {
        dialog.querySelectorAll('button').find((button) => button.textContent === copy(`nodes.enrollment.${key}`)).click();
        await flush();
      }
      assert.deepEqual(copied.splice(0), blocks.map((block) => block.textContent));
      assert.ok(dialog.textContent.includes(copy('nodes.enrollment.copied')));
      closeDialogs();
      await pending;
      assert.deepEqual(blocks.map((block) => block.textContent), ['', '', '']);
      assert.equal(dom.body.querySelector('dialog'), null);
      assert.equal(dom.timers.size, 0);
    }
  } finally {
    closeDialogs();
    globalThis.fetch = originalFetch;
    if (originalNavigator) Object.defineProperty(globalThis, 'navigator', originalNavigator); else delete globalThis.navigator;
    dom.restore();
  }
});

test('ticket countdown covers expiry and rounds up remaining seconds', () => {
  const expiry = '2026-10-27T12:15:00Z';
  const now = Date.parse('2026-10-27T12:00:00Z');
  assert.equal(enrollmentCountdown(expiry, now), copy('nodes.enrollment.remaining', {time: '15:00'}));
  assert.equal(enrollmentCountdown(expiry, now + 899001), copy('nodes.enrollment.remaining', {time: '0:01'}));
  assert.equal(enrollmentCountdown(expiry, now + 900000), copy('nodes.enrollment.expired'));
  assert.equal(enrollmentCountdown(expiry, now + 901000), copy('nodes.enrollment.expired'));
  assert.equal(enrollmentCountdown('bad', now), copy('nodes.enrollment.expired'));
});

test('ticket state includes revoked, expired, exhausted and active metadata', () => {
  const now = Date.parse('2026-10-27T12:00:00Z');
  const ticket = {expires_at: '2026-10-27T12:15:00Z', uses: 0, max_uses: 1};
  assert.equal(enrollmentState(ticket, now), copy('nodes.enrollment.active'));
  assert.equal(enrollmentState({...ticket, uses: 1}, now), copy('nodes.enrollment.used'));
  assert.equal(enrollmentState(ticket, now + 900000), copy('nodes.enrollment.expired'));
  assert.equal(enrollmentState({...ticket, revoked_at: '2026-10-27T11:59:00Z'}, now), copy('nodes.enrollment.revoked'));
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
