import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, writeFileSync, readFileSync, existsSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {spawnSync} from 'node:child_process';
import {copy, flush, installDOM} from './console-dom.js';
import {enrollmentCountdown, enrollmentState, installerCommand, nodeEnrollments, shellQuote} from '../src/console/lib/enrollment-utils.js';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {enrollmentTab, issueEnrollment} from '../src/console/pages/enrollments.js';
import {closeDialogs} from '../src/console/ui/dialogs.js';

test('deployment command passes literal data over stdin and authenticates sudo outside the pipeline', () => {
  assert.equal(shellQuote("a'b"), "'a'\\''b'");
  const directory = mkdtempSync(join(tmpdir(), 'orbitbeacon-command-'));
  const origin = "https://monitor.example.com/a'b;$(touch injected-origin)";
  const token = "test-se'cret;$(touch injected-token)";
  const version = '1.2.3-rc.1';
  const script = (name, body) => writeFileSync(join(directory, name), `#!/bin/sh\nset -eu\n${body}\n`, {mode: 0o700});
  script('curl', `printf 'curl\\n' >> "$TEST_DIR/events"
printf '%s\\n' "$@" >> "$TEST_DIR/external-args"
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = --output ]; then output=$2; shift; fi
  shift
done
if [ "$CURL_FAILURE" -ne 0 ]; then
  printf 'printf partial-executed > "$TEST_DIR/partial"\\n' > "$output"
  exit "$CURL_FAILURE"
fi
cp "$TEST_DIR/installer" "$output"`);
  script('id', `printf '%s\\n' "$SIM_UID"`);
  script('sudo', `printf 'sudo %s\\n' "$*" >> "$TEST_DIR/events"
printf '%s\\n' "$@" >> "$TEST_DIR/external-args"
if [ "$1" = -v ]; then exit 0; fi
[ "$1" = -n ]
shift
exec "$@"`);
  script('sh', `printf '%s\\n' "$@" >> "$TEST_DIR/external-args"
exec /bin/sh "$@"`);
  script('installer', `IFS= read -r token
printf '%s' "$token" > "$TEST_DIR/token"
printf '%s\\n' "$@" > "$TEST_DIR/args"
printf 'install\\n' >> "$TEST_DIR/events"`);
  const run = (uid, failure = 0) => {
    for (const name of ['token', 'args', 'events', 'external-args']) rmSync(join(directory, name), {force: true});
    return spawnSync('/bin/sh', [], {input: installerCommand(origin, version, token), cwd: directory,
      env: {...process.env, PATH: `${directory}:${process.env.PATH}`, TEST_DIR: directory, TMPDIR: directory, SIM_UID: String(uid), CURL_FAILURE: String(failure)}, encoding: 'utf8'});
  };
  try {
    for (const uid of [0, 1000]) {
      const result = run(uid);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(readFileSync(join(directory, 'token'), 'utf8'), token);
      assert.deepEqual(readFileSync(join(directory, 'args'), 'utf8').trimEnd().split('\n'), ['--server', origin, '--version', version]);
      const events = readFileSync(join(directory, 'events'), 'utf8').trimEnd().split('\n');
      assert.deepEqual(events.map((event) => event.startsWith('sudo -n ') ? 'sudo -n' : event), uid === 0 ? ['curl', 'install'] : ['curl', 'sudo -v', 'sudo -n', 'install']);
      const args = readFileSync(join(directory, 'external-args'), 'utf8').split('\n');
      assert.ok(args.includes(`https://raw.githubusercontent.com/mr-monkeyray/orbit-beacon/v${version}/deploy/install-agent.sh`));
      for (const required of ['--fail', '--silent', '--show-error', '--location', '--proto', '=https', '--proto-redir', '--tlsv1.2', '--output']) assert.ok(args.includes(required), required);
      assert.equal(readFileSync(join(directory, 'external-args'), 'utf8').includes(token), false);
      assert.equal(existsSync(join(directory, 'injected-origin')), false);
      assert.equal(existsSync(join(directory, 'injected-token')), false);
    }
    const failed = run(1000, 22);
    assert.notEqual(failed.status, 0);
    assert.equal(existsSync(join(directory, 'token')), false);
    assert.equal(existsSync(join(directory, 'partial')), false);
    assert.equal(readFileSync(join(directory, 'events'), 'utf8'), 'curl\n');
    for (const invalid of ['', 'dev', 'v1.2.3', '1.2', '1.2.3;touch injected-version', '1.2.3$(touch injected-version)']) {
      assert.equal(installerCommand(origin, invalid, token), '');
    }
  } finally { rmSync(directory, {recursive: true, force: true}); }
});

test('ticket dialog copies two secrets, expires both controls and clears DOM and timer on close', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const originalNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  const originalNow = Date.now;
  let now = Date.parse('2026-11-09T12:00:00Z');
  Date.now = () => now;
  const copied = [];
  const secret = "single-use-se'cret";
  const version = '1.2.3';
  location.origin = 'https://monitor.example.com';
  setNavigationSignal(null);
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {clipboard: {writeText: async (text) => { copied.push(text); }}}});
  let expected;
  globalThis.fetch = async (path, options) => {
    assert.equal(path, '/api/v1/enrollments');
    assert.equal(options.method, 'POST');
    assert.deepEqual(JSON.parse(options.body), expected);
    return new Response(JSON.stringify({token: secret, server_version: version, expires_at: '2026-11-09T12:30:00Z'}));
  };
  try {
    for (expected of [{node_id: 'node'}, {group_id: 'group'}]) {
      now = Date.parse('2026-11-09T12:00:00Z');
      const pending = issueEnrollment(expected, copy('nodes.enrollAgent'));
      await flush();
      const dialog = dom.body.querySelector('dialog');
      const blocks = dialog.querySelectorAll('code');
      assert.deepEqual(blocks.map((block) => block.textContent), [secret, installerCommand(location.origin, version, secret)]);
      assert.ok(dialog.textContent.includes(copy('nodes.enrollment.issueIntro')));
      assert.ok(dialog.textContent.includes(copy('nodes.enrollment.secretWarning')));
      const timer = dialog.querySelector('[role="timer"]');
      assert.equal(timer.textContent, copy('nodes.enrollment.remaining', {time: '30:00'}));
      const buttons = ['copyToken', 'copyInstall'].map((key) => dialog.querySelectorAll('button').find((button) => button.textContent === copy(`nodes.enrollment.${key}`)));
      assert.equal(dialog.querySelectorAll('button').filter((button) => button.textContent.startsWith('Copy')).length, 2);
      for (const button of buttons) { assert.equal(button.disabled, false); button.click(); await flush(); }
      assert.deepEqual(copied.splice(0), blocks.map((block) => block.textContent));
      assert.ok(dialog.textContent.includes(copy('nodes.enrollment.copied')));
      now += 1799001;
      for (const tick of dom.timers.values()) tick();
      assert.equal(timer.textContent, copy('nodes.enrollment.remaining', {time: '0:01'}));
      assert.ok(buttons.every((button) => !button.disabled));
      now = Date.parse('2026-11-09T12:30:00Z');
      for (const tick of dom.timers.values()) tick();
      assert.ok(buttons.every((button) => button.disabled));
      assert.ok(dialog.textContent.includes(copy('nodes.enrollment.expiredReissue')));
      closeDialogs(); await pending;
      assert.deepEqual(blocks.map((block) => block.textContent), ['', '']);
      assert.equal(dom.body.querySelector('dialog'), null);
      assert.equal(dom.timers.size, 0);
    }
  } finally {
    closeDialogs(); Date.now = originalNow; globalThis.fetch = originalFetch;
    if (originalNavigator) Object.defineProperty(globalThis, 'navigator', originalNavigator); else delete globalThis.navigator;
    dom.restore();
  }
});

test('ticket countdown covers expiry and rounds up remaining seconds', () => {
  const expiry = '2026-10-27T12:30:00Z';
  const now = Date.parse('2026-10-27T12:00:00Z');
  assert.equal(enrollmentCountdown(expiry, now), copy('nodes.enrollment.remaining', {time: '30:00'}));
  assert.equal(enrollmentCountdown(expiry, now + 1799001), copy('nodes.enrollment.remaining', {time: '0:01'}));
  assert.equal(enrollmentCountdown(expiry, now + 1800000), copy('nodes.enrollment.expired'));
  assert.equal(enrollmentCountdown(expiry, now + 1801000), copy('nodes.enrollment.expired'));
  assert.equal(enrollmentCountdown('bad', now), copy('nodes.enrollment.expired'));
});

test('ticket state includes revoked, expired, exhausted and active metadata', () => {
  const now = Date.parse('2026-10-27T12:00:00Z');
  const ticket = {expires_at: '2026-10-27T12:15:00Z', uses: 0, max_uses: 1};
  assert.equal(enrollmentState(ticket, now), copy('nodes.enrollment.active'));
  assert.equal(enrollmentState({...ticket, uses: 1}, now), copy('nodes.enrollment.used'));
  assert.equal(enrollmentState({...ticket, uses: 7, max_uses: 100}, now + 900000), copy('nodes.enrollment.used'));
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

test('invalid release keeps ticket copying and clipboard manual fallback available', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const originalNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  const originalSelection = globalThis.getSelection;
  let selected;
  document.createRange = () => ({selectNodeContents: (node) => { selected = node; }});
  document.execCommand = () => false;
  globalThis.getSelection = () => ({removeAllRanges() {}, addRange() {}});
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {clipboard: {writeText: async () => { throw new Error('denied'); }}}});
  globalThis.fetch = async () => new Response(JSON.stringify({token: 'fake-ticket', server_version: 'dev', expires_at: new Date(Date.now() + 1800000).toISOString()}));
  try {
    const pending = issueEnrollment({node_id: 'node'}, 'Deploy'); await flush();
    const dialog = dom.body.querySelector('dialog');
    const buttons = dialog.querySelectorAll('button');
    assert.equal(buttons.find((button) => button.textContent === copy('nodes.enrollment.copyInstall')).disabled, true);
    assert.ok(dialog.textContent.includes(copy('nodes.enrollment.unreleased')));
    buttons.find((button) => button.textContent === copy('nodes.enrollment.copyToken')).click(); await flush();
    assert.equal(selected.textContent, 'fake-ticket');
    assert.ok(dialog.textContent.includes(copy('nodes.enrollment.copyManual')));
    document.execCommand = () => true;
    buttons.find((button) => button.textContent === copy('nodes.enrollment.copyToken')).click(); await flush();
    assert.ok(dialog.textContent.includes(copy('nodes.enrollment.copiedFallback')));
    closeDialogs(); await pending;
  } finally {
    closeDialogs(); globalThis.fetch = originalFetch; globalThis.getSelection = originalSelection;
    if (originalNavigator) Object.defineProperty(globalThis, 'navigator', originalNavigator); else delete globalThis.navigator;
    dom.restore();
  }
});

for (const boundary of ['delete', 'navigation', 'session']) test(`pending ticket hides late plaintext after ${boundary}`, async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const controller = new AbortController(); setNavigationSignal(controller.signal);
  let deleted = false; let resolve;
  globalThis.fetch = async () => new Promise((done) => { resolve = done; });
  try {
    const pending = issueEnrollment({node_id: 'node'}, 'Deploy', () => deleted);
    const observed = pending.catch((error) => { assert.equal(error.name, 'AbortError'); });
    await flush();
    if (boundary === 'delete') deleted = true;
    if (boundary === 'navigation') { controller.abort(); setNavigationSignal(new AbortController().signal); }
    if (boundary === 'session') setSession({user: {id: 'changed-user', username: 'Other', must_change_password: false}, csrf_token: boundary, expires_at: ''});
    resolve(new Response(JSON.stringify({token: 'late-fake-secret', server_version: '1.2.3', expires_at: new Date(Date.now() + 1800000).toISOString()})));
    await observed; await flush();
    assert.equal(dom.body.querySelector('dialog'), null); assert.equal(dom.body.textContent.includes('late-fake-secret'), false);
  } finally { closeDialogs(); setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''}); globalThis.fetch = originalFetch; dom.restore(); }
});

test('ticket list shows a historical consumed ticket as used without a quota column', async () => {
  const dom = installDOM(); const originalFetch = globalThis.fetch;
  setNavigationSignal(null);
  globalThis.fetch = async () => new Response(JSON.stringify({enrollments: [{id: 'old-ticket', node_id: 'node', uses: 7, max_uses: 1, expires_at: '2020-01-01T00:00:00Z'}], has_more: false}));
  try {
    const tab = enrollmentTab({id: 'node', name: 'Node'}); dom.body.append(tab); await flush();
    assert.deepEqual(tab.querySelectorAll('th').map((cell) => cell.textContent), ['ticket', 'expiresHeader', 'state', 'actions'].map((key) => copy(`nodes.enrollment.${key}`)));
    assert.ok(tab.querySelector('tbody').textContent.includes(copy('nodes.enrollment.used')));
    assert.equal(tab.querySelector('tbody').textContent.includes('7 / 1'), false);
  } finally { globalThis.fetch = originalFetch; dom.restore(); }
});

test('detail deployment restores its enabled trigger only within the same live node lifecycle', async () => {
  const dom = installDOM(); const originalFetch = globalThis.fetch;
  globalThis.fetch = async (_path, options) => new Response(JSON.stringify(options.method === 'POST'
    ? {token: 'fake-focus-secret', server_version: '1.2.3', expires_at: new Date(Date.now() + 1800000).toISOString()}
    : {enrollments: [], has_more: false}));
  try {
    for (const boundary of ['close', 'navigation', 'session', 'delete']) {
      const navigation = new AbortController(); setNavigationSignal(navigation.signal);
      setSession({user: {id: 'focus-admin', username: 'Admin', must_change_password: false}, csrf_token: 'csrf', expires_at: ''});
      const node = {id: 'node', name: 'Node'};
      const tab = enrollmentTab(node); dom.body.append(tab); await flush();
      const issue = tab.querySelectorAll('button').find((button) => button.textContent === copy('nodes.enrollment.issue'));
      issue.focus(); issue.click(); document.body.focus(); await flush();
      assert.ok(dom.body.querySelector('dialog'));
      if (boundary === 'navigation') { navigation.abort(); setNavigationSignal(new AbortController().signal); }
      if (boundary === 'session') setSession({user: {id: 'other-admin', username: 'Other', must_change_password: false}, csrf_token: 'changed', expires_at: ''});
      if (boundary === 'delete') node.deleted = true;
      closeDialogs(); document.body.focus(); await flush();
      assert.equal(issue.disabled, boundary === 'delete');
      assert.equal(document.activeElement, boundary === 'close' ? issue : document.body);
      tab.remove();
    }
  } finally { closeDialogs(); setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''}); globalThis.fetch = originalFetch; dom.restore(); }
});
