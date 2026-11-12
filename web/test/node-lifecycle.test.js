import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, flush, installDOM} from './console-dom.js';
import {closeDialogs} from '../src/console/ui/dialogs.js';

async function setup(t) {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window'].map((key) => [key, globalThis[key]]));
  globalThis.window = new EventTarget();
  const scope = new AbortController(); setNavigationSignal(scope.signal);
  setSession({user: {id: 'admin', username: 'Admin', must_change_password: false}, csrf_token: 'csrf', expires_at: ''});
  const requests = [];
  globalThis.fetch = async (url, options) => { requests.push({url, options}); return Response.json({}); };
  const toasts = document.createElement('div'); toasts.id = 'toasts'; dom.body.append(toasts);
  const sourceURL = new URL('../src/console/pages/node-lifecycle.js', import.meta.url);
  const source = (await readFile(sourceURL, 'utf8')).replace(/from '(\.[^']+)'/g, (_match, path) =>
    `from '${path === '../router.js' ? `data:text/javascript,${encodeURIComponent('export async function refresh() {} export async function go() {}')}` : new URL(path, sourceURL).href}'`);
  const {lifecycleActions} = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`);
  const node = {id: 'node-1', name: 'Edge Tokyo', status: 'online'};
  const header = lifecycleActions(node); const manage = header.manage(); dom.body.append(header, manage);
  t.after(() => {
    closeDialogs(); scope.abort(); setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''});
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    dom.restore();
  });
  return {...dom, header, manage, node, requests};
}

for (const [key, endpoint] of [['revokeCredential', '/credential'], ['deleteNode', '?confirm=true']]) {
  for (const deleted of [false, true]) test(`${key} requires typed confirmation${deleted ? ' and fences deletion during the dialog' : ' before DELETE'}`, async (t) => {
    const env = await setup(t);
    const action = env.manage.querySelectorAll('button').find((item) => item.textContent === copy(`nodes.${key}`));
    action.click(); await flush();
    const dialog = env.body.querySelector('dialog'); const form = dialog.querySelector('form');
    const entry = dialog.querySelector('input'); const submit = dialog.querySelectorAll('button').find((item) => item.type === 'submit');
    assert.equal(submit.disabled, true);
    entry.value = 'Wrong'; entry.dispatchEvent(new Event('input'));
    form.dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(env.requests.length, 0); assert.equal(dialog.open, true);
    entry.value = env.node.name; entry.dispatchEvent(new Event('input'));
    assert.equal(submit.disabled, false);
    if (deleted) { env.node.deleted = true; env.header.snapshot(); }
    form.dispatchEvent(new Event('submit', {cancelable: true})); dialog.dispatchEvent(new Event('animationend')); await flush();
    assert.deepEqual(env.requests.map(({url, options}) => [url, options.method]), deleted ? [] : [[`/api/v1/nodes/node-1${endpoint}`, 'DELETE']]);
    assert.equal(action.disabled, deleted);
  });
  test(`${key} cancellation sends nothing and unlocks actions`, async (t) => {
    const env = await setup(t);
    const action = env.manage.querySelectorAll('button').find((item) => item.textContent === copy(`nodes.${key}`));
    action.click(); await flush();
    const dialog = env.body.querySelector('dialog');
    dialog.querySelector('#typed-confirm-cancel').click(); dialog.dispatchEvent(new Event('animationend')); await flush();
    assert.equal(env.requests.length, 0);
    assert.ok(env.manage.querySelectorAll('button').every((item) => !item.disabled));
    assert.equal(document.activeElement, action);
  });
}
