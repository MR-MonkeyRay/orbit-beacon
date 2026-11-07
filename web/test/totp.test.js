import test from 'node:test';
import assert from 'node:assert/strict';
import {copy, elements, flush, TestElement} from './console-dom.js';
import {liveHarness, response} from './live-harness.js';
import * as client from '../src/console/api.js';
import {describeAction} from '../src/console/format.js';

const user = {id: '00000000-0000-4000-8000-000000000001', username: 'administrator', must_change_password: false};
function setup(t, respond) {
  const originals = new Map(['matchMedia', 'requestAnimationFrame', 'history'].map((key) => [key, globalThis[key]]));
  const page = new AbortController();
  t.after(async () => {
    page.abort(); client.setSession({user: null, csrf_token: '', expires_at: ''}); client.setNavigationSignal(null);
    await flush();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  });
  const env = liveHarness(t, respond);
  globalThis.matchMedia = () => ({matches: true, addEventListener() {}});
  globalThis.requestAnimationFrame = (callback) => callback();
  globalThis.history = {state: null, replaceState() {}};
  location.origin = 'https://example.test';
  client.setNavigationSignal(page.signal);
  client.setSession({user: null, csrf_token: '', expires_at: ''});
  return env;
}
function authenticate() {
  client.setSession({user, csrf_token: 'totp-csrf', expires_at: '2026-10-28T00:10:00Z'});
}
function named(root, name) { return elements(root).find((element) => element.name === name); }
function label(root, text) { return elements(root).find((element) => element.tagName === 'button' && element.textContent === text); }
const submit = async (form) => { form.dispatchEvent(new Event('submit', {cancelable: true})); await flush(); await flush(); };
const rejected = (code) => response({code}, {status: 401});

test('login preserves credentials for TOTP, maps invalid codes to the field, and clears on credential changes', async (t) => {
  let code = 'totp_required';
  const env = setup(t, () => rejected(code)); location.pathname = '/login';
  const sidebar = new TestElement('div'); sidebar.id = 'sidebar';
  const mark = new TestElement('span'); mark.className = 'mark'; mark.cloneNode = () => new TestElement('span');
  sidebar.append(mark); env.body.append(sidebar);
  const {loginPage} = await import('../src/console/pages/login.js');
  const page = await loginPage(); env.body.append(page);
  const form = page.querySelector('form');
  const username = named(form, 'username'); const password = named(form, 'password'); const otp = named(form, 'otp');
  username.value = 'administrator'; password.value = 'private-password';
  assert.equal(otp.disabled, true);
  await submit(form);
  assert.equal(password.value, 'private-password'); assert.equal(username.value, 'administrator');
  assert.equal(otp.disabled, false); assert.equal(otp.required, true); assert.equal(document.activeElement, otp);
  assert.equal(otp.getAttribute('autocomplete'), 'one-time-code');
  assert.equal(JSON.parse(env.requests.at(-1).body).otp, undefined);
  code = 'totp_invalid'; otp.value = 'aaaaa-bbbbb-ccccc-ddddd';
  await submit(form);
  assert.equal(JSON.parse(env.requests.at(-1).body).otp, otp.value);
  assert.equal(password.value, 'private-password'); assert.equal(document.activeElement, otp);
  assert.equal(form.querySelector(`#${otp.id}-error`).textContent, copy('errors.totp_invalid'));
  username.value = 'other'; username.dispatchEvent(new Event('input'));
  assert.equal(otp.value, ''); assert.equal(otp.disabled, true); assert.equal(otp.parentElement.hidden, true);
  code = 'totp_required'; await submit(form); otp.value = '123456';
  password.value = 'replacement'; password.dispatchEvent(new Event('input'));
  assert.equal(otp.value, ''); assert.equal(otp.disabled, true);
  code = 'unauthenticated'; await submit(form);
  assert.equal(password.value, ''); assert.equal(otp.disabled, true);
});

test('TOTP credential errors preserve the active account session', async (t) => {
  setup(t, () => rejected('totp_invalid')); authenticate();
  const epoch = client.getSessionEpoch();
  await assert.rejects(client.api('/account/totp/disable', {method: 'POST', body: {password: 'private', otp: 'wrong'}}), (error) => error.code === 'totp_invalid');
  assert.equal(client.session.user.id, user.id); assert.equal(client.getSessionEpoch(), epoch);
  globalThis.fetch = async () => rejected('unauthenticated');
  await assert.rejects(client.api('/account/totp/disable', {method: 'POST', body: {}}));
  assert.equal(client.session.user, null);
});

test('a TOTP 401 whose body times out still ends the session, but navigation does not', async (t) => {
  const stalled = (request) => new Promise((resolve) => resolve({
    ok: false, status: 401, headers: new Headers(), body: null,
    json: () => new Promise((_resolve, reject) => {
      request.signal.addEventListener('abort', () => reject(request.signal.reason), {once: true});
    })
  }));
  const env = setup(t, stalled); authenticate();
  const pending = client.api('/account/totp/disable', {method: 'POST', body: {}});
  const failed = assert.rejects(pending, (error) => error.code === 'timeout');
  await flush(); await env.advance(15000); await failed;
  assert.equal(client.session.user, null);

  authenticate();
  const page = new AbortController(); client.setNavigationSignal(page.signal);
  const aborted = assert.rejects(client.api('/account/totp/disable', {method: 'POST', body: {}}), (error) => error.name === 'AbortError');
  await flush(); page.abort(new DOMException('Navigated.', 'AbortError')); await aborted;
  assert.equal(client.session.user.id, user.id);
});

test('account setup renders a QR with quiet zone, confirms, and shows recovery codes once', async (t) => {
  let enabled = false;
  const codes = ['abcde-fghij-klmno-pqrst', 'uvwxy-23456-abcde-fghij'];
  const env = setup(t, (request) => {
    if (request.url.endsWith('/account/totp')) return response({enabled, recovery_codes_remaining: enabled ? 10 : 0});
    if (request.url.endsWith('/setup')) return response({secret: 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567', qr: {size: 21, modules: '1'.repeat(441)}});
    if (request.url.endsWith('/confirm')) { enabled = true; return response({recovery_codes: [...codes]}); }
    assert.fail(request.url);
  }); authenticate();
  const {totpPanel} = await import('../src/console/ui/totp.js');
  const panel = totpPanel(); env.body.append(panel); await flush();
  label(panel, copy('account.totpEnable')).click(); await flush();
  let dialog = document.querySelector('dialog'); named(dialog, 'password').value = 'private-password';
  await submit(dialog.querySelector('form'));
  dialog = document.querySelector('dialog');
  const qr = elements(dialog).find((element) => element.getAttribute('aria-label') === copy('account.totpQR'));
  assert.equal(qr.getAttribute('viewBox'), '0 0 29 29');
  assert.equal(qr.querySelector('rect').getAttribute('fill'), '#fff');
  assert.equal(qr.querySelector('path').getAttribute('fill'), '#000');
  assert.match(qr.querySelector('path').getAttribute('d'), /^M4 4h1v1h-1z/);
  assert.equal(qr.hasAttribute('style'), false);
  assert.ok(dialog.textContent.includes('ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'));
  named(dialog, 'code').value = '123456'; await submit(dialog.querySelector('form'));
  dialog = document.querySelector('dialog');
  assert.ok(dialog.textContent.includes(codes.join('\n')));
  assert.ok(label(dialog, copy('account.copyRecoveryCodes')));
  assert.equal(client.session.user.totp_enabled, true);
  const secretNode = dialog.querySelector('pre'); label(dialog, copy('account.recoverySaved')).click(); await flush();
  assert.equal(secretNode.textContent, ''); assert.equal(document.querySelector('dialog'), null);
  assert.equal(env.requests.filter((request) => request.url.endsWith('/confirm')).length, 1);
});

test('cancelling TOTP confirmation retains the disabled state', async (t) => {
  const env = setup(t, (request) => request.url.endsWith('/setup')
    ? response({secret: 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567', qr: {size: 21, modules: '0'.repeat(441)}})
    : response({enabled: false, recovery_codes_remaining: 0})); authenticate();
  const {totpPanel} = await import('../src/console/ui/totp.js');
  const panel = totpPanel(); env.body.append(panel); await flush(); label(panel, copy('account.totpEnable')).click();
  let dialog = document.querySelector('dialog'); named(dialog, 'password').value = 'private-password'; await submit(dialog.querySelector('form'));
  dialog = document.querySelector('dialog'); label(dialog, copy('controls.cancel')).click(); await flush();
  assert.equal(client.session.user.totp_enabled, false); assert.equal(env.requests.some((request) => request.url.endsWith('/confirm')), false);
});

test('administrator reset prompts for the operator password and excludes self', async (t) => {
  const target = {...user, id: '00000000-0000-4000-8000-000000000002', username: 'other', totp_enabled: true};
  const env = setup(t, (request) => {
    if (request.url.endsWith('/users')) return response({users: [{...user, totp_enabled: true}, target]});
    if (request.url.endsWith('/totp/reset')) return new Response(null, {status: 204});
    if (request.url.endsWith('/account/totp')) return response({enabled: true, recovery_codes_remaining: 10});
    assert.fail(request.url);
  }); authenticate(); location.pathname = '/settings'; location.hash = '#administrators'; client.invalidate('users');
  const {settingsPage} = await import('../src/console/pages/settings.js');
  const page = await settingsPage(); env.body.append(page); env.mutations(); await flush();
  const resets = elements(page).filter((element) => element.tagName === 'button' && element.textContent === copy('account.totpReset'));
  assert.equal(resets.length, 1); resets[0].click();
  const dialog = document.querySelector('dialog'); assert.equal(named(dialog, 'otp'), undefined);
  named(dialog, 'password').value = 'operator-password'; await submit(dialog.querySelector('form'));
  const request = env.requests.find((request) => request.url.endsWith('/totp/reset'));
  assert.equal(request.url, `/api/v1/users/${target.id}/totp/reset`); assert.deepEqual(JSON.parse(request.body), {password: 'operator-password'});
});

test('TOTP audit labels use translated actions', () => {
  for (const [action, key] of [['enable', 'totpEnable'], ['disable', 'totpDisable'], ['reset', 'totpReset'], ['recovery_codes', 'totpRecoveryCodes']]) {
    assert.equal(describeAction(`user.totp.${action}`), copy(`audit.${key}`));
  }
});

test('regenerating and disabling require both credentials and render the updated account state', async (t) => {
  let enabled = true;
  let remaining = 10;
  const env = setup(t, (request) => {
    if (request.url.endsWith('/account/totp')) return response({enabled, recovery_codes_remaining: remaining});
    if (request.url.endsWith('/recovery-codes')) return response({recovery_codes: ['abcde-fghij-klmno-pqrst']});
    if (request.url.endsWith('/disable')) { enabled = false; remaining = 0; return new Response(null, {status: 204}); }
    assert.fail(request.url);
  }); authenticate();
  const {totpPanel} = await import('../src/console/ui/totp.js');
  const panel = totpPanel(); env.body.append(panel); await flush();
  label(panel, copy('account.totpRegenerate')).click();
  let dialog = document.querySelector('dialog');
  assert.equal(named(dialog, 'password').required, true); assert.equal(named(dialog, 'otp').required, true);
  named(dialog, 'password').value = 'private-password'; named(dialog, 'otp').value = '123456';
  await submit(dialog.querySelector('form'));
  const regenerated = env.requests.find((request) => request.url.endsWith('/recovery-codes'));
  assert.deepEqual(JSON.parse(regenerated.body), {password: 'private-password', otp: '123456'});
  dialog = document.querySelector('dialog'); assert.ok(dialog.textContent.includes('abcde-fghij-klmno-pqrst'));
  label(dialog, copy('account.recoverySaved')).click(); await flush();
  label(panel, copy('account.totpDisable')).click(); dialog = document.querySelector('dialog');
  named(dialog, 'password').value = 'private-password'; named(dialog, 'otp').value = 'abcde-fghij-klmno-pqrst';
  await submit(dialog.querySelector('form'));
  assert.equal(client.session.user.totp_enabled, false);
  assert.ok(label(panel, copy('account.totpEnable')));
  assert.equal(document.querySelector('dialog'), null);
});
