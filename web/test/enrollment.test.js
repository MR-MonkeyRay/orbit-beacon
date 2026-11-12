import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, writeFileSync, readFileSync, existsSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {spawnSync} from 'node:child_process';
import {copy, flush, installDOM as installConsoleDOM} from './console-dom.js';
import {enrollmentCountdown, installerCommand, shellQuote} from '../src/console/lib/enrollment-utils.js';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {issueEnrollment} from '../src/console/pages/enrollments.js';
import {closeDialogs, hasUnsavedDialogChanges} from '../src/console/ui/dialogs.js';

// Enrollment now owns a private live channel, which listens for window lifecycle events.
function installDOM() {
  const dom = installConsoleDOM();
  const previous = globalThis.window;
  globalThis.window = new EventTarget();
  return {...dom, restore() {
    if (previous === undefined) delete globalThis.window; else globalThis.window = previous;
    dom.restore();
  }};
}

test('one-line commands quote literal values, omit the key by default and clean up downloads', () => {
  assert.equal(shellQuote("a'b"), "'a'\\''b'");
  const directory = mkdtempSync(join(tmpdir(), 'orbitbeacon-command-'));
  const origin = "https://monitor.example.com/a'b;$(touch injected-origin)";
  const key = "test-se'cret;$(touch injected-key)";
  const version = '1.2.3-rc.1';
  const script = (name, body) => writeFileSync(join(directory, name), `#!/bin/sh\nset -eu\n${body}\n`, {mode: 0o700});
  script('curl', `printf '%s\\n' "$@" > "$TEST_DIR/curl-args"
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then output=$2; shift; fi
  shift
done
printf '%s' "$output" > "$TEST_DIR/download"
if [ "$CURL_FAILURE" -ne 0 ]; then
  printf 'touch "$TEST_DIR/partial"\\n' > "$output"
  exit "$CURL_FAILURE"
fi
cp "$TEST_DIR/installer" "$output"`);
  script('sudo', `printf '%s\\n' "$@" > "$TEST_DIR/sudo-args"
exec "$@"`);
  script('installer', `printf '%s\\n' "$@" > "$TEST_DIR/args"`);
  const run = (includeKey, failure = 0) => {
    for (const name of ['args', 'sudo-args', 'download']) rmSync(join(directory, name), {force: true});
    const command = installerCommand(origin, version, includeKey ? key : undefined);
    assert.ok(!command.includes('\n'));
    if (!includeKey) assert.ok(!command.includes(key));
    return spawnSync('/bin/sh', ['-c', command], {cwd: directory,
      env: {...process.env, PATH: `${directory}:${process.env.PATH}`, TEST_DIR: directory, TMPDIR: directory, CURL_FAILURE: String(failure)}, encoding: 'utf8'});
  };
  try {
    for (const includeKey of [false, true]) {
      const result = run(includeKey);
      assert.equal(result.status, 0, result.stderr);
      assert.deepEqual(readFileSync(join(directory, 'args'), 'utf8').trimEnd().split('\n'),
        ['--server', origin, '--version', version, ...(includeKey ? ['--key', key] : [])]);
      assert.equal(readFileSync(join(directory, 'sudo-args'), 'utf8').split('\n')[0], 'sh');
      const args = readFileSync(join(directory, 'curl-args'), 'utf8').split('\n');
      assert.ok(args.includes(`https://raw.githubusercontent.com/mr-monkeyray/orbit-beacon/v${version}/deploy/install-agent.sh`));
      for (const required of ['-fsSL', '--proto', '=https', '--tlsv1.2', '-o']) assert.ok(args.includes(required), required);
      assert.equal(existsSync(readFileSync(join(directory, 'download'), 'utf8')), false);
      assert.equal(existsSync(join(directory, 'injected-origin')), false);
      assert.equal(existsSync(join(directory, 'injected-key')), false);
    }
    run(false, 22);
    assert.equal(existsSync(join(directory, 'args')), false);
    assert.equal(existsSync(join(directory, 'sudo-args')), false);
    assert.equal(existsSync(join(directory, 'partial')), false);
    assert.equal(existsSync(readFileSync(join(directory, 'download'), 'utf8')), false);
    for (const invalid of ['', 'dev', 'v1.2.3', '1.2', '1.2.3;touch injected-version', '1.2.3$(touch injected-version)']) {
      assert.equal(installerCommand(origin, invalid), '');
      assert.equal(installerCommand(origin, invalid, key), '');
    }
  } finally { rmSync(directory, {recursive: true, force: true}); }
});

test('key dialog toggles command inclusion, copies key and command, expires both controls and clears DOM and timer on close', async () => {
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
    for (expected of [{node_id: '00000000-0000-4000-8000-000000000001'}, {group_id: 'group'}]) {
      now = Date.parse('2026-11-09T12:00:00Z');
      const pending = issueEnrollment(expected, copy('nodes.installCommand'));
      await flush();
      const dialog = dom.body.querySelector('dialog');
      const blocks = dialog.querySelectorAll('code');
      assert.deepEqual(blocks.map((block) => block.textContent), [installerCommand(location.origin, version), secret]);
      assert.equal(dialog.querySelector('details'), null);
      const toggle = dialog.querySelector('[role="switch"]');
      assert.equal(Boolean(toggle.checked), false);
      toggle.checked = true; toggle.dispatchEvent(new Event('change'));
      assert.equal(blocks[0].textContent, installerCommand(location.origin, version, secret));
      assert.equal(hasUnsavedDialogChanges(), false);
      dialog.querySelector('.btn-primary').click(); await flush();
      assert.deepEqual(copied.splice(0), [installerCommand(location.origin, version, secret)]);
      toggle.checked = false; toggle.dispatchEvent(new Event('change'));
      assert.equal(blocks[0].textContent, installerCommand(location.origin, version));
      assert.equal(dialog.querySelector('ol').children.length, expected.node_id ? 3 : 2);
      const primary = dialog.querySelector('.btn-primary');
      assert.equal(primary.textContent, copy('nodes.enrollment.copyInstall'));
      assert.equal(dialog.querySelector('ol').querySelector('button'), primary);
      assert.ok(dialog.querySelector('.enrollment-key').textContent.includes(secret));
      assert.ok(dialog.textContent.includes(copy('nodes.enrollment.runStep')));
      const timer = dialog.querySelector('[role="timer"]');
      assert.equal(timer.textContent, copy('nodes.enrollment.remaining', {time: '30:00'}));
      const buttons = ['copyToken', 'copyInstall'].map((key) => dialog.querySelectorAll('button').find((button) => button.textContent === copy(`nodes.enrollment.${key}`)));
      assert.equal(dialog.querySelectorAll('button').filter((button) => button.textContent.startsWith('Copy')).length, 2);
      for (const button of buttons) { assert.equal(button.disabled, false); button.click(); await flush(); }
      assert.deepEqual(copied.splice(0), [secret, installerCommand(location.origin, version)]);
      assert.ok(dialog.textContent.includes(copy('nodes.enrollment.copied')));
      now += 1799001;
      for (const tick of dom.timers.values()) tick();
      assert.equal(timer.textContent, copy('nodes.enrollment.remaining', {time: '0:01'}));
      assert.ok(buttons.every((button) => !button.disabled));
      now = Date.parse('2026-11-09T12:30:00Z');
      for (const tick of dom.timers.values()) tick();
      assert.ok(buttons.every((button) => button.disabled));
      assert.equal(toggle.disabled, true);
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

test('key countdown covers expiry and rounds up remaining seconds', () => {
  const expiry = '2026-10-27T12:30:00Z';
  const now = Date.parse('2026-10-27T12:00:00Z');
  assert.equal(enrollmentCountdown(expiry, now), copy('nodes.enrollment.remaining', {time: '30:00'}));
  assert.equal(enrollmentCountdown(expiry, now + 1799001), copy('nodes.enrollment.remaining', {time: '0:01'}));
  assert.equal(enrollmentCountdown(expiry, now + 1800000), copy('nodes.enrollment.expired'));
  assert.equal(enrollmentCountdown(expiry, now + 1801000), copy('nodes.enrollment.expired'));
  assert.equal(enrollmentCountdown('bad', now), copy('nodes.enrollment.expired'));
});

test('invalid release keeps key copying and clipboard manual fallback available', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const originalNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  const originalSelection = globalThis.getSelection;
  let selected;
  document.createRange = () => ({selectNodeContents: (node) => { selected = node; }});
  document.execCommand = () => false;
  globalThis.getSelection = () => ({removeAllRanges() {}, addRange() {}});
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {clipboard: {writeText: async () => { throw new Error('denied'); }}}});
  globalThis.fetch = async () => new Response(JSON.stringify({token: 'fake-key', server_version: 'dev', expires_at: new Date(Date.now() + 1800000).toISOString()}));
  try {
    const pending = issueEnrollment({node_id: '00000000-0000-4000-8000-000000000001'}, 'Deploy'); await flush();
    const dialog = dom.body.querySelector('dialog');
    const buttons = dialog.querySelectorAll('button');
    assert.equal(buttons.find((button) => button.textContent === copy('nodes.enrollment.copyInstall')), undefined);
    assert.deepEqual(dialog.querySelectorAll('pre').map((block) => block.textContent), ['fake-key']);
    assert.ok(dialog.textContent.includes(copy('nodes.enrollment.unreleased')));
    assert.equal(dialog.querySelector('details'), null);
    assert.equal(dialog.querySelector('[role="switch"]'), null);
    buttons.find((button) => button.textContent === copy('nodes.enrollment.copyToken')).click(); await flush();
    assert.equal(selected.textContent, 'fake-key');
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

for (const boundary of ['delete', 'navigation', 'session']) test(`pending key hides late plaintext after ${boundary}`, async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const controller = new AbortController(); setNavigationSignal(controller.signal);
  let deleted = false; let resolve;
  globalThis.fetch = async () => new Promise((done) => { resolve = done; });
  try {
    const pending = issueEnrollment({node_id: '00000000-0000-4000-8000-000000000001'}, 'Deploy', () => deleted);
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

for (const boundary of ['close', 'navigation', 'session', 'view', 'delete']) test(`node enrollment observes a live report and closes its channel on ${boundary}`, async (t) => {
  const {liveHarness, frame, response} = await import('./live-harness.js');
  const nodeID = '00000000-0000-4000-8000-000000000009';
  const user = {id: '00000000-0000-4000-8000-000000000001', username: 'Admin', must_change_password: false};
  const proof = {user, csrf_token: 'enrollment-live', expires_at: '2026-10-28T01:00:00Z'};
  const env = liveHarness(t, ({url}) => {
    if (url === '/api/v1/me') return response(proof);
    assert.equal(url, '/api/v1/enrollments');
    return response({token: 'live-secret', server_version: '1.2.3', expires_at: '2026-10-28T00:30:00Z'});
  });
  const navigation = new AbortController();
  setNavigationSignal(navigation.signal); setSession(proof);
  location.origin = 'https://example.test';
  const sessionTimers = env.timers.size;
  try {
    const pending = issueEnrollment({node_id: nodeID}, 'Deploy'); await flush();
    const dialog = env.body.querySelector('dialog');
    const progress = dialog.querySelector('.enrollment-progress');
    assert.equal(progress.textContent, copy('nodes.enrollment.waiting'));
    assert.ok(progress.querySelector('.enrollment-spinner'));
    assert.equal(env.sockets.length, 1);
    const socket = env.sockets[0];
    assert.ok(socket.url.endsWith(`/api/v1/live/ws?node_id=${nodeID}`));
    const node = {id: nodeID, name: 'Node', notes: '', region: '', provider: '', group_id: null, tags: [],
      created_at: '2026-10-27T00:00:00Z', status: 'pending', presence: 'pending', stopped: false, last_seen: null, agent_version: ''};
    socket.open();
    socket.message(frame('snapshot', {data: {node, latest: null}})); await flush();
    assert.equal(progress.textContent, copy('nodes.enrollment.waiting'));
    socket.message(frame('snapshot', {data: {node: {...node, status: 'online', presence: 'online', last_seen: '2026-10-28T00:00:00Z'}, latest: null}})); await flush();
    assert.equal(progress.querySelector('.enrollment-spinner'), null);
    assert.ok(progress.textContent.includes(copy('nodes.enrollment.reported')));
    const view = progress.querySelector('a');
    assert.equal(view.textContent, copy('nodes.enrollment.viewNode'));
    assert.equal(view.getAttribute('href'), `/nodes/${nodeID}`);
    assert.equal(view.dataset.link, '');
    assert.equal(dialog.querySelector('.enrollment-key').hidden, true);
    // Subsequent frames do not replace the focused success action.
    socket.message(frame('snapshot', {data: {node: {...node, status: 'online', presence: 'online'}, latest: null}})); await flush();
    assert.equal(progress.querySelector('a'), view);
    const toggle = dialog.querySelector('[role="switch"]');
    toggle.checked = true; toggle.dispatchEvent(new Event('change'));
    const secrets = dialog.querySelectorAll('code');
    assert.ok(secrets[0].textContent.includes("--key 'live-secret'"));
    if (boundary === 'delete') socket.message(frame('snapshot', {data: {node: null, latest: null}}));
    else if (boundary === 'navigation') navigation.abort();
    else if (boundary === 'session') setSession({user: null, csrf_token: '', expires_at: ''});
    else if (boundary === 'view') view.click();
    else closeDialogs();
    assert.equal(socket.closed, true);
    await env.advance(300); await pending;
    assert.equal(env.body.querySelector('dialog'), null);
    assert.deepEqual(secrets.map((element) => element.textContent), ['', '']);
    assert.equal(env.intervals.size, 0);
    assert.equal(env.timers.size, boundary === 'session' ? 0 : sessionTimers);
  } finally { closeDialogs(); navigation.abort(); setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''}); }
});

test('node enrollment skips the wait step when the node already reported before the key', async (t) => {
  const {liveHarness, frame, response} = await import('./live-harness.js');
  const nodeID = '00000000-0000-4000-8000-000000000010';
  const user = {id: '00000000-0000-4000-8000-000000000001', username: 'Admin', must_change_password: false};
  const proof = {user, csrf_token: 'enrollment-reissue', expires_at: '2026-10-28T01:00:00Z'};
  const env = liveHarness(t, ({url}) => {
    if (url === '/api/v1/me') return response(proof);
    return response({token: 'reissue-secret', server_version: '1.2.3', expires_at: '2026-10-28T00:30:00Z'});
  });
  const navigation = new AbortController();
  setNavigationSignal(navigation.signal); setSession(proof);
  location.origin = 'https://example.test';
  try {
    const pending = issueEnrollment({node_id: nodeID}, 'Reissue'); await flush();
    const dialog = env.body.querySelector('dialog');
    const socket = env.sockets[0];
    socket.open();
    const node = {id: nodeID, name: 'Node', notes: '', region: '', provider: '', group_id: null, tags: [],
      created_at: '2026-10-27T00:00:00Z', status: 'online', presence: 'online', stopped: false, last_seen: '2026-10-28T00:00:00Z', agent_version: '1.2.3'};
    socket.message(frame('snapshot', {data: {node, latest: null}})); await flush();
    // 旧 Agent 仍在上报，不能当作新安装成功。
    assert.equal(dialog.querySelector('.enrollment-progress'), null);
    assert.equal(dialog.querySelector('.enrollment-steps').children.length, 2);
    assert.equal(dialog.textContent.includes(copy('nodes.enrollment.reported')), false);
    assert.equal(dialog.querySelector('.enrollment-key').hidden, false);
    assert.equal(socket.closed, true);
    closeDialogs(); await env.advance(300); await pending;
  } finally { closeDialogs(); navigation.abort(); setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''}); }
});
