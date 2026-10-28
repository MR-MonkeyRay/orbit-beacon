import test from 'node:test';
import assert from 'node:assert/strict';
import {invalidate, session, setNavigationSignal, setSession} from '../src/console/api.js';
import {elements, flush, installDOM} from './console-dom.js';

class TestNode extends EventTarget {
  constructor(tagName = '') {
    super();
    this.tagName = tagName;
    this.children = [];
    this.attributes = new Map();
    this.dataset = {};
    this.classList = {toggle() {}};
    this.listeners = new Map();
  }

  append(...children) {
    for (const child of children) {
      if (child instanceof TestNode) child.parentElement = this;
      this.children.push(child);
    }
  }

  replaceChildren(...children) {
    this.children = [];
    this.append(...children);
  }

  setAttribute(key, value) { this.attributes.set(key, String(value)); }
  getAttribute(key) { return this.attributes.get(key); }
  querySelector() { return null; }
  addEventListener(type, listener) { this.listeners.set(type, listener); }
  async click() { await this.listeners.get('click')?.(new Event('click')); }
}

function findButton(node, label) {
  if (node instanceof TestNode && node.tagName === 'button' && node.children.includes(label)) return node;
  if (!(node instanceof TestNode)) return null;
  for (const child of node.children) {
    const found = findButton(child, label);
    if (found) return found;
  }
  return null;
}

const event = (id) => ({id, action: 'node.create', created_at: '2026-10-27T12:00:00Z'});

test('audit load more uses the cursor and hides after a full final page', async () => {
  const calls = [];
  const firstCursor = 'opaque/cursor+value';
  const elements = new Map();
  const document = new TestNode('document');
  document.documentElement = new TestNode('html');
  document.documentElement.dataset = {};
  document.createElement = (tag) => new TestNode(tag);
  document.createElementNS = (_namespace, tag) => new TestNode(tag);
  document.getElementById = (id) => {
    if (!elements.has(id)) elements.set(id, new TestNode('div'));
    return elements.get(id);
  };
  document.querySelector = () => null;
  globalThis.Node = TestNode;
  globalThis.document = document;
  globalThis.window = new EventTarget();
  globalThis.matchMedia = () => ({matches: false, addEventListener() {}});
  globalThis.location = {hash: '#audit', pathname: '/settings', search: ''};
  globalThis.history = {state: null, replaceState() {}};
  globalThis.requestAnimationFrame = () => 0;
  globalThis.MutationObserver = class { observe() {} disconnect() {} };
  globalThis.fetch = async (path) => {
    calls.push(path);
    let body;
    if (path === '/api/v1/users') body = {users: []};
    else if (path === '/api/v1/groups') body = {groups: []};
    else if (path === '/api/v1/nodes?limit=100&offset=0') body = {nodes: [], has_more: false};
    else if (path === '/api/v1/audit?limit=50&offset=0') body = {events: [event('first')], has_more: true, next_cursor: firstCursor};
    else if (path === `/api/v1/audit?limit=50&before=${encodeURIComponent(firstCursor)}`) {
      body = {events: Array.from({length: 50}, (_, n) => event(`older-${n}`)), has_more: false};
    } else throw new Error(`Unexpected request: ${path}`);
    return new Response(JSON.stringify(body), {headers: {'Content-Type': 'application/json'}});
  };
  invalidate('nodes', 'groups', 'users');
  setSession({csrf_token: 'csrf', user: {id: 'user', username: 'admin', created_at: '2026-10-27T12:00:00Z'}, expires_at: ''});
  try {
    const {settingsPage} = await import('../src/console/pages/settings.js');
    const page = await settingsPage();
    const more = findButton(page, 'Load older events');
    assert.ok(more);
    assert.equal(more.hidden, false);
    await more.click();
    assert.ok(calls.includes('/api/v1/audit?limit=50&before=opaque%2Fcursor%2Bvalue'));
    assert.equal(more.hidden, true);
  } finally {
    invalidate('nodes', 'groups', 'users');
    delete globalThis.fetch;
    delete globalThis.requestAnimationFrame;
    delete globalThis.history;
    delete globalThis.location;
    delete globalThis.matchMedia;
    delete globalThis.window;
    delete globalThis.document;
    delete globalThis.Node;
    delete globalThis.MutationObserver;
  }
});

test('Users forms send only administrator credentials and keep conflicts in the dialog', async () => {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window', 'matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  const self = {id: 'user-one', username: 'admin-one', created_at: '2026-10-27T12:00:00Z'};
  const requests = [];
  Object.assign(globalThis, {window: new EventTarget(), matchMedia: () => ({matches: true, addEventListener() {}}),
    history: {state: null, replaceState() {}}, requestAnimationFrame: () => 0});
  Object.assign(location, {pathname: '/settings', hash: '#users', assign: (path) => assert.fail(`Unexpected navigation: ${path}`)});
  setNavigationSignal(null);
  setSession({user: self, csrf_token: 'csrf', expires_at: ''});
  invalidate('nodes', 'groups', 'users');
  globalThis.fetch = async (path, options) => {
    if (options.method !== 'GET') {
      requests.push({path, method: options.method, body: JSON.parse(options.body)});
      return new Response(JSON.stringify({code: 'username_conflict', message: 'Username already exists.'}), {status: 409});
    }
    const body = path === '/api/v1/users' ? {users: [self]}
      : path === '/api/v1/groups' ? {groups: []}
        : path.startsWith('/api/v1/nodes?') ? {nodes: [], has_more: false}
          : {events: [], has_more: false};
    return new Response(JSON.stringify(body));
  };
  const {closeDialogs} = await import('../src/console/ui/dialogs.js');
  try {
    const {settingsPage} = await import('../src/console/pages/settings.js');
    const page = await settingsPage();
    dom.body.append(page);
    assert.match(page.textContent, /Data operations/);
    assert.ok(!elements(page).some((element) => element.tagName === 'th' && element.textContent === 'Role'));
    elements(page).find((element) => element.tagName === 'button' && element.textContent === 'Add user').click();
    let dialog = dom.body.querySelector('dialog');
    let controls = elements(dialog);
    assert.equal(controls.filter((element) => element.name === 'role').length, 0);
    controls.find((element) => element.name === 'username').value = 'admin-two';
    const password = controls.find((element) => element.name === 'password');
    password.value = 'regression password two';
    dialog.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.deepEqual(requests[0], {path: '/api/v1/users', method: 'POST', body: {username: 'admin-two', password: 'regression password two'}});
    assert.equal(password.value, '');
    assert.match(dialog.textContent, /Username already exists/);
    closeDialogs();
    elements(page).find((element) => element.getAttribute('aria-label') === 'Edit admin-one').click();
    dialog = dom.body.querySelector('dialog');
    controls = elements(dialog);
    assert.deepEqual(controls.filter((element) => element.name).map((element) => element.name), ['username']);
    controls.find((element) => element.name === 'username').value = 'admin-two';
    dialog.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.deepEqual(requests[1], {path: '/api/v1/users/user-one', method: 'PATCH', body: {username: 'admin-two'}});
    assert.match(dialog.textContent, /Username already exists/);
    assert.equal(session.user.username, 'admin-one');
    assert.equal(session.csrf, 'csrf');
  } finally {
    closeDialogs();
    invalidate('nodes', 'groups', 'users');
    dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});

test('self deletion redirects immediately without fetching the administrator list again', async () => {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window', 'matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  const self = {id: 'user-one', username: 'admin-one', created_at: '2026-10-27T12:00:00Z'};
  const requests = [];
  const redirects = [];
  Object.assign(globalThis, {window: new EventTarget(), matchMedia: () => ({matches: true, addEventListener() {}}),
    history: {state: null, replaceState() {}}, requestAnimationFrame: () => 0});
  Object.assign(location, {pathname: '/settings', hash: '#users', assign: (path) => redirects.push(path)});
  setNavigationSignal(null);
  setSession({user: self, csrf_token: 'csrf', expires_at: ''});
  invalidate('nodes', 'groups', 'users');
  globalThis.fetch = async (path, options) => {
    requests.push({path, method: options.method});
    if (options.method === 'DELETE') return new Response(null, {status: 204});
    const body = path === '/api/v1/users' ? {users: [self, {...self, id: 'user-two', username: 'admin-two'}]}
      : path === '/api/v1/groups' ? {groups: []}
        : path.startsWith('/api/v1/nodes?') ? {nodes: [], has_more: false}
          : {events: [], has_more: false};
    return new Response(JSON.stringify(body));
  };
  const {closeDialogs} = await import('../src/console/ui/dialogs.js');
  try {
    const {settingsPage} = await import('../src/console/pages/settings.js');
    const page = await settingsPage();
    dom.body.append(page);
    elements(page).find((element) => element.getAttribute('aria-label') === 'Delete admin-one').click();
    const dialog = dom.body.querySelector('dialog');
    assert.match(dialog.textContent, /deleting your own account/);
    elements(dialog).find((element) => element.tagName === 'button' && element.textContent === 'Delete user').click();
    dialog.dispatchEvent(new Event('animationend'));
    await flush();
    assert.deepEqual(redirects, ['/login']);
    assert.deepEqual(requests.at(-1), {path: '/api/v1/users/user-one', method: 'DELETE'});
    assert.equal(requests.filter((request) => request.path === '/api/v1/users').length, 1);
  } finally {
    closeDialogs();
    invalidate('nodes', 'groups', 'users');
    dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});
