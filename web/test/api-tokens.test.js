import test from 'node:test';
import assert from 'node:assert/strict';
import {copy, elements, flush} from './console-dom.js';
import {liveHarness, response} from './live-harness.js';
import * as client from '../src/console/api.js';
import {apiTokensPanel} from '../src/console/ui/api-tokens.js';
import {describeAction} from '../src/console/format.js';
import {closeDialogs} from '../src/console/ui/dialogs.js';
import {errorText, fieldErrorText} from '../src/shared/errors.js';

const entry = {id: '00000000-0000-4000-8000-000000000001', name: 'scripts', created_by_username: 'operator', created_at: '2026-10-28T00:00:00Z', last_used_at: null, expires_at: null, rate_limit_per_minute: null};
const label = (root, text) => elements(root).find((element) => element.tagName === 'button' && element.textContent === text);
const named = (root, name) => elements(root).find((element) => element.name === name);
const submit = async (form) => { form.dispatchEvent(new Event('submit', {cancelable: true})); await flush(); await flush(); };
function setup(t, respond, limits = false) {
  const originals = new Map(['matchMedia', 'requestAnimationFrame', 'navigator'].map((key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)]));
  const env = liveHarness(t, (request) => !limits && request.url === '/api/v1/settings/api-tokens' ? response({rate_limit_per_minute: 600}) : respond(request));
  globalThis.matchMedia = () => ({matches: true, addEventListener() {}});
  globalThis.requestAnimationFrame = (callback) => callback();
  location.origin = 'https://example.test';
  const navigation = new AbortController();
  client.setNavigationSignal(navigation.signal);
  client.setSession({user: {id: entry.id, username: 'operator', must_change_password: false}, csrf_token: 'csrf', expires_at: '2026-10-29T00:00:00Z'});
  t.after(async () => {
    closeDialogs(); navigation.abort(); client.setNavigationSignal(null); client.setSession({user: null, csrf_token: '', expires_at: ''}); await flush();
    for (const [key, descriptor] of originals) { if (descriptor) Object.defineProperty(globalThis, key, descriptor); else delete globalThis[key]; }
  });
  return {...env, navigation};
}

test('API token panel lists metadata, creates once, copies, clears and revokes', async (t) => {
  let tokens = [entry];
  const secret = 'obt_once-only-secret';
  let copied = '';
  const env = setup(t, (request) => {
    if (request.method === 'POST') { tokens = [...tokens, {...entry, id: 'new', name: 'monitor'}]; return response({...entry, token: secret}); }
    if (request.method === 'DELETE') { tokens = tokens.filter((token) => token.id !== entry.id); return new Response(null, {status: 204}); }
    return response({api_tokens: tokens});
  });
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {clipboard: {writeText: async (text) => { copied = text; }}}});
  const panel = apiTokensPanel(); env.body.append(panel); await flush();
  assert.ok(panel.textContent.includes('scripts')); assert.ok(panel.textContent.includes('operator'));
  assert.ok(panel.textContent.includes('—')); assert.equal(panel.textContent.includes('1970'), false);
  for (const key of ['apiTokenName', 'apiTokenCreator', 'apiTokenCreated', 'apiTokenLastUsed', 'apiTokenExpires']) assert.ok(panel.textContent.includes(copy(`account.${key}`)));
  label(panel, copy('account.createAPIToken')).click();
  let dialog = document.querySelector('dialog');
  assert.equal(elements(dialog).some((element) => element.tagName === 'p'), false);
  assert.deepEqual(elements(named(dialog, 'expires_in_days')).filter((element) => element.tagName === 'option').map((element) => element.value), ['30', '90', '365', '']);
  named(dialog, 'name').value = 'monitor'; named(dialog, 'expires_in_days').value = '';
  const password = named(dialog, 'password'); password.value = 'current-password';
  await submit(dialog.querySelector('form'));
  const creation = env.requests.find((request) => request.method === 'POST');
  assert.deepEqual(JSON.parse(creation.body), {name: 'monitor', expires_in_days: null, password: 'current-password'});
  assert.equal(password.value, '');
  dialog = document.querySelector('dialog'); const value = dialog.querySelector('pre'); assert.equal(value.textContent, secret);
  label(dialog, copy('account.copyAPIToken')).click(); await flush(); assert.equal(copied, secret);
  label(dialog, copy('account.recoverySaved')).click(); await flush(); assert.equal(value.textContent, ''); assert.equal(document.querySelector('dialog'), null);
  assert.equal(panel.textContent.includes(secret), false); assert.equal(env.requests.filter((request) => request.method === 'POST').length, 1);
  label(panel, copy('account.revokeAPIToken')).click(); await flush(); dialog = document.querySelector('dialog');
  assert.equal(env.requests.some((request) => request.method === 'DELETE'), false);
  label(dialog, copy('account.revokeAPIToken')).click(); await flush(); await flush();
  assert.equal(env.requests.find((request) => request.method === 'DELETE').url, `/api/v1/api-tokens/${entry.id}`);
  assert.equal(panel.textContent.includes('scripts'), false);
  assert.equal(elements(panel).some((element) => element.hasAttribute('style')), false);
});

test('API token creation error stays in form and clears password; cancellation never creates', async (t) => {
  const env = setup(t, (request) => request.method === 'POST' ? response({code: 'api_token_limit'}, {status: 409}) : response({api_tokens: []}));
  const panel = apiTokensPanel(); env.body.append(panel); await flush(); label(panel, copy('account.createAPIToken')).click();
  const dialog = document.querySelector('dialog'); named(dialog, 'name').value = 'script'; const password = named(dialog, 'password'); password.value = 'current-password';
  await submit(dialog.querySelector('form'));
  assert.ok(dialog.textContent.includes(copy('errors.api_token_limit'))); assert.equal(password.value, ''); assert.equal(dialog.querySelector('pre'), null);
  closeDialogs(); await flush();
  label(panel, copy('account.createAPIToken')).click();
  const cancelled = document.querySelector('dialog'); named(cancelled, 'password').value = 'transient-password';
  const control = named(cancelled, 'password'); label(cancelled, copy('controls.cancel')).click(); await flush();
  assert.equal(control.value, ''); assert.equal(env.requests.filter((request) => request.method === 'POST').length, 1);
});

test('API token secret is cleared when session ends', async (t) => {
  const env = setup(t, (request) => request.method === 'POST' ? response({...entry, token: 'obt_secret'}) : response({api_tokens: []}));
  const panel = apiTokensPanel(); env.body.append(panel); await flush(); label(panel, copy('account.createAPIToken')).click();
  const dialog = document.querySelector('dialog'); named(dialog, 'name').value = 'script'; named(dialog, 'password').value = 'current-password'; await submit(dialog.querySelector('form'));
  const value = document.querySelector('dialog').querySelector('pre');
  client.invalidateSession('expired'); await flush(); assert.equal(value.textContent, ''); assert.equal(document.querySelector('dialog'), null);
});

test('API token actions and errors use translated labels', () => {
  assert.equal(describeAction('api_token.create'), copy('audit.apiTokenCreate'));
  assert.equal(describeAction('api_token.revoke'), copy('audit.apiTokenRevoke'));
  assert.equal(errorText({code: 'api_token_limit'}), copy('errors.api_token_limit'));
  assert.equal(fieldErrorText('must be 30, 90, 365 or null'), copy('fields.apiTokenExpiry'));
});

test('API token limits save globally, set and clear overrides, and cancel without saving', async (t) => {
  let global = 600; let override = null;
  const changes = [];
  const env = setup(t, (request) => {
    if (request.url === '/api/v1/settings/api-tokens') {
      if (request.method === 'PATCH') { global = JSON.parse(request.body).rate_limit_per_minute; changes.push(['global', global]); }
      return response({rate_limit_per_minute: global});
    }
    if (request.method === 'PATCH') { override = JSON.parse(request.body).rate_limit_per_minute; changes.push(['token', override]); return response({rate_limit_per_minute: override}); }
    return response({api_tokens: [{...entry, rate_limit_per_minute: override}]});
  }, true);
  const panel = apiTokensPanel(); env.body.append(panel); await flush();
  assert.ok(panel.textContent.includes(copy('account.apiTokenDefaultRate', {limit: 600})));
  named(panel, 'rate_limit_per_minute').value = '12';
  await submit(panel.querySelector('form'));
  assert.deepEqual(changes, [['global', 12]]);
  assert.ok(panel.textContent.includes(copy('account.apiTokenDefaultRate', {limit: 12})));
  const open = () => { label(panel, copy('account.apiTokenEditRate')).focus(); label(panel, copy('account.apiTokenEditRate')).click(); return document.querySelector('dialog'); };
  let dialog = open();
  assert.equal(elements(dialog).some((element) => element.tagName === 'p'), false);
  assert.equal(document.activeElement, named(dialog, 'rate_limit_per_minute'));
  named(dialog, 'rate_limit_per_minute').value = '3'; await submit(dialog.querySelector('form'));
  assert.deepEqual(changes.at(-1), ['token', 3]); assert.equal(document.querySelector('dialog'), null);
  dialog = open();
  assert.equal(String(named(dialog, 'rate_limit_per_minute').getAttribute('placeholder')), '12');
  named(dialog, 'rate_limit_per_minute').value = '';
  await submit(dialog.querySelector('form'));
  assert.deepEqual(changes.at(-1), ['token', null]);
  assert.ok(panel.textContent.includes(copy('account.apiTokenDefaultRate', {limit: 12})));
  dialog = open(); const opener = label(panel, copy('account.apiTokenEditRate'));
  const queryControls = dialog.querySelectorAll.bind(dialog);
  dialog.querySelectorAll = (selector) => selector === 'input:not([data-transient]),select,textarea,[data-tags]'
    ? elements(dialog).filter((element) => element.tagName === 'input') : queryControls(selector);
  named(dialog, 'rate_limit_per_minute').value = '99';
  label(dialog, copy('controls.cancel')).click(); await flush();
  const discard = elements(document.body).filter((element) => element.tagName === 'dialog').at(-1);
  label(discard, copy('controls.discard')).click(); await flush(); await flush();
  assert.equal(document.querySelector('dialog'), null); assert.equal(changes.length, 3);
  assert.equal(document.activeElement, opener);
  assert.equal(elements(panel).some((element) => element.hasAttribute('style')), false);
});

test('API token limit errors remain visible in the panel and modal', async (t) => {
  const env = setup(t, (request) => request.method === 'PATCH'
    ? response({code: 'validation_failed', fields: {rate_limit_per_minute: 'must be an integer between 1 and 100000'}}, {status: 400})
    : request.url === '/api/v1/settings/api-tokens' ? response({rate_limit_per_minute: 600}) : response({api_tokens: [entry]}), true);
  const panel = apiTokensPanel(); env.body.append(panel); await flush();
  named(panel, 'rate_limit_per_minute').value = '0'; await submit(panel.querySelector('form'));
  assert.ok(panel.textContent.includes(copy('errors.validation_failed')));
  assert.ok(panel.textContent.includes(copy('fields.apiTokenRate')));
  label(panel, copy('account.apiTokenEditRate')).click();
  const dialog = document.querySelector('dialog'); named(dialog, 'rate_limit_per_minute').value = '0'; await submit(dialog.querySelector('form'));
  assert.ok(dialog.textContent.includes(copy('errors.validation_failed')));
  assert.ok(dialog.textContent.includes(copy('fields.apiTokenRate')));
  closeDialogs(); await flush();
  assert.equal(describeAction('api_token.rate_limit.update'), copy('audit.apiTokenRateUpdate'));
  assert.equal(describeAction('settings.api_token_rate_limit.update'), copy('audit.apiTokenGlobalRateUpdate'));
});
