import test from 'node:test';
import assert from 'node:assert/strict';
import {invalidate, session, setNavigationSignal, setSession} from '../src/console/api.js';
import {elements, flush, installDOM} from './console-dom.js';
import {reducedMotion} from '../src/shared/dom.js';

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

function findTag(node, tag) {
  if (node instanceof TestNode && node.tagName === tag) return node;
  if (!(node instanceof TestNode)) return null;
  for (const child of node.children) {
    const found = findTag(child, tag);
    if (found) return found;
  }
  return null;
}

const event = (id) => ({id, action: 'node.create', created_at: '2026-10-27T12:00:00Z'});

test('settings loads each tab data only when first shown', async () => {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window', 'matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  const calls = [];
  Object.assign(globalThis, {window: new EventTarget(), matchMedia: () => ({matches: true, addEventListener() {}}),
    history: {state: null, replaceState(_state, _unused, url) { location.hash = new URL(url, 'https://example.test').hash; }}, requestAnimationFrame: (callback) => callback()});
  Object.assign(location, {pathname: '/settings', search: '', hash: '#account'});
  setNavigationSignal(null);
  setSession({csrf_token: 'csrf', user: {id: 'user', username: 'admin', must_change_password: false, created_at: '2026-10-27T12:00:00Z'}, expires_at: ''});
  invalidate('nodes', 'groups', 'users');
  globalThis.fetch = async (path) => {
    calls.push(path);
    const body = path === '/api/v1/users' ? {users: []}
      : path === '/api/v1/groups' ? {groups: []}
        : path.startsWith('/api/v1/nodes?') ? {nodes: [], has_more: false}
          : path.startsWith('/api/v1/audit?') ? {events: [], has_more: false}
            : path.endsWith('/settings/public-page') ? {enabled: true}
              : path.startsWith('/api/v1/backup-plans?') ? {plans: [], has_more: false, timezone_name: 'UTC', utc_offset_seconds: 0}
                : path.startsWith('/api/v1/tasks?') ? {tasks: [], has_more: false}
                  : assert.fail(`Unexpected request: ${path}`);
    return new Response(JSON.stringify(body));
  };
  try {
    const {settingsPage} = await import('../src/console/pages/settings.js');
    const page = await settingsPage();
    dom.body.append(page); dom.mutations(); await flush();
    assert.deepEqual(calls, []);

    const tabs = elements(page).filter((element) => element.getAttribute('role') === 'tab');
    tabs.find((element) => element.id === 'tab-administrators').click(); dom.mutations(); await flush();
    assert.deepEqual(calls, ['/api/v1/users']);
    tabs.find((element) => element.id === 'tab-account').click(); dom.mutations(); await flush();
    tabs.find((element) => element.id === 'tab-administrators').click(); dom.mutations(); await flush();
    assert.deepEqual(calls, ['/api/v1/users']);

    tabs.find((element) => element.id === 'tab-groups').click(); dom.mutations(); await flush();
    assert.deepEqual(calls.slice(1).sort(), ['/api/v1/groups', '/api/v1/nodes?limit=100&offset=0']);
    tabs.find((element) => element.id === 'tab-audit').click(); dom.mutations(); await flush();
    assert.deepEqual(calls.slice(3), ['/api/v1/audit?limit=50&offset=0']);
  } finally {
    invalidate('nodes', 'groups', 'users');
    setSession({user: null, csrf_token: '', expires_at: ''});
    setNavigationSignal(null);
    dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});

test('settings lazy tabs show loading and retry an inline load failure', async () => {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window', 'matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  let userRequests = 0;
  Object.assign(globalThis, {window: new EventTarget(), matchMedia: () => ({matches: true, addEventListener() {}}),
    history: {state: null, replaceState() {}}, requestAnimationFrame: (callback) => callback()});
  Object.assign(location, {pathname: '/settings', search: '', hash: '#account'});
  setNavigationSignal(null);
  setSession({csrf_token: 'csrf', user: {id: 'user', username: 'admin', must_change_password: false, created_at: '2026-10-27T12:00:00Z'}, expires_at: ''});
  invalidate('users');
  globalThis.fetch = async (path) => {
    assert.equal(path, '/api/v1/users');
    userRequests += 1;
    if (userRequests === 1) return new Response(JSON.stringify({message: 'Administrators could not be loaded.'}), {status: 500});
    return new Response(JSON.stringify({users: []}));
  };
  try {
    const {settingsPage} = await import('../src/console/pages/settings.js');
    const page = await settingsPage();
    dom.body.append(page); dom.mutations();
    const tabs = elements(page).filter((element) => element.getAttribute('role') === 'tab');
    tabs.find((element) => element.id === 'tab-administrators').click();
    assert.ok(elements(page).some((element) => element.getAttribute('aria-label') === 'Loading'));
    await flush();
    const loadError = elements(page).find((element) => element.getAttribute('role') === 'alert');
    assert.ok(loadError?.textContent);
    elements(page).find((element) => element.tagName === 'button' && element.textContent === 'Retry').click();
    assert.ok(elements(page).some((element) => element.getAttribute('aria-label') === 'Loading'));
    await flush();
    assert.equal(userRequests, 2);
    assert.ok(elements(page).some((element) => element.tagName === 'button' && element.textContent === 'Add administrator'));
    tabs.find((element) => element.id === 'tab-account').click();
    tabs.find((element) => element.id === 'tab-administrators').click();
    await flush();
    assert.equal(userRequests, 2);
  } finally {
    invalidate('users');
    setSession({user: null, csrf_token: '', expires_at: ''});
    setNavigationSignal(null);
    dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});

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
  setSession({csrf_token: 'csrf', user: {id: 'user', username: 'admin', must_change_password: false, created_at: '2026-10-27T12:00:00Z'}, expires_at: ''});
  try {
    const {settingsPage} = await import('../src/console/pages/settings.js');
    const page = await settingsPage();
    await flush();
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

test('audit load more rejects a cursor that does not advance', async () => {
  const calls = [];
  const firstCursor = 'stuck-cursor';
  const originalSetTimeout = globalThis.setTimeout;
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
  globalThis.setTimeout = () => 0;
  globalThis.MutationObserver = class { observe() {} disconnect() {} };
  globalThis.fetch = async (path) => {
    calls.push(path);
    let body;
    if (path === '/api/v1/users') body = {users: []};
    else if (path === '/api/v1/groups') body = {groups: []};
    else if (path === '/api/v1/nodes?limit=100&offset=0') body = {nodes: [], has_more: false};
    else if (path === '/api/v1/audit?limit=50&offset=0') body = {events: [event('first')], has_more: true, next_cursor: firstCursor};
    else if (path === `/api/v1/audit?limit=50&before=${firstCursor}`) body = {events: [event('duplicate')], has_more: true, next_cursor: firstCursor};
    else throw new Error(`Unexpected request: ${path}`);
    return new Response(JSON.stringify(body), {headers: {'Content-Type': 'application/json'}});
  };
  invalidate('nodes', 'groups', 'users');
  setSession({csrf_token: 'csrf', user: {id: 'user', username: 'admin', must_change_password: false, created_at: '2026-10-27T12:00:00Z'}, expires_at: ''});
  try {
    const {settingsPage} = await import('../src/console/pages/settings.js');
    const page = await settingsPage();
    await flush();
    const more = findButton(page, 'Load older events');
    await more.click();
    assert.equal(calls.at(-1), '/api/v1/audit?limit=50&before=stuck-cursor');
    assert.equal(findTag(page, 'ol').children.length, 1);
    assert.equal(more.hidden, false);
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
    globalThis.setTimeout = originalSetTimeout;
  }
});

test('administrator forms preserve request boundaries and keep conflicts in the dialog', async () => {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window', 'matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  const self = {id: 'user-one', username: 'admin-one', must_change_password: false, created_at: '2026-10-27T12:00:00Z'};
  const requests = [];
  Object.assign(globalThis, {window: new EventTarget(), matchMedia: () => ({matches: true, addEventListener() {}}),
    history: {state: null, replaceState() {}}, requestAnimationFrame: () => 0});
  Object.assign(location, {pathname: '/settings', hash: '#administrators', assign: (path) => assert.fail(`Unexpected navigation: ${path}`)});
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
    await flush();
    elements(page).find((element) => element.tagName === 'button' && element.textContent === 'Add administrator').click();
    let dialog = dom.body.querySelector('dialog');
    let controls = elements(dialog);
    controls.find((element) => element.name === 'username').value = 'admin-two';
    const password = controls.find((element) => element.name === 'password');
    const mustChangePassword = controls.find((element) => element.name === 'must_change_password');
    assert.equal(mustChangePassword.checked, false);
    password.value = 'regression password two';
    dialog.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.deepEqual(requests[0], {path: '/api/v1/users', method: 'POST', body: {username: 'admin-two', password: 'regression password two', must_change_password: false}});
    assert.equal(password.value, '');
    assert.equal(dialog.querySelector('.form-error').hidden, false);
    assert.ok(dialog.querySelector('.form-error').textContent);
    mustChangePassword.checked = true;
    password.value = 'another regression password';
    dialog.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.equal(requests[1].body.must_change_password, true);
    closeDialogs();
    elements(page).find((element) => element.getAttribute('aria-label') === 'Rename administrator "admin-one"').click();
    dialog = dom.body.querySelector('dialog');
    controls = elements(dialog);
    assert.deepEqual(controls.filter((element) => element.name).map((element) => element.name), ['username']);
    controls.find((element) => element.name === 'username').value = 'admin-two';
    dialog.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true}));
    await flush();
    assert.deepEqual(requests[2], {path: '/api/v1/users/user-one', method: 'PATCH', body: {username: 'admin-two'}});
    assert.equal(dialog.querySelector('.form-error').hidden, false);
    assert.ok(dialog.querySelector('.form-error').textContent);
    assert.equal(session.user.username, 'admin-one');
    assert.equal(session.csrf, 'csrf');
  } finally {
    closeDialogs();
    invalidate('nodes', 'groups', 'users');
    dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});

test('public-page settings confirm disabling, block duplicate writes, and restore server state after failure', async () => {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window', 'matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  const motion = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => true});
  Object.assign(globalThis, {window: new EventTarget(), matchMedia: () => ({matches: true, addEventListener() {}}), history: {state: null, replaceState() {}}, requestAnimationFrame: () => 0});
  Object.assign(location, {pathname: '/settings', hash: '#public-page'});
  setNavigationSignal(null);
  setSession({user: {id: 'public-settings-user', username: 'admin', must_change_password: false}, csrf_token: 'csrf', expires_at: ''});
  invalidate('nodes', 'groups', 'users');
  const writes = [];
  let resolveWrite;
  globalThis.fetch = async (path, options) => {
    if (options.method === 'PATCH') {
      writes.push(JSON.parse(options.body));
      return new Promise((resolve) => { resolveWrite = resolve; });
    }
    const body = path.endsWith('/settings/public-page') ? {enabled: true}
      : path.endsWith('/users') ? {users: []} : path.endsWith('/groups') ? {groups: []}
        : path.includes('/nodes?') ? {nodes: [], has_more: false} : {events: [], has_more: false};
    return new Response(JSON.stringify(body));
  };
  const {closeDialogs} = await import('../src/console/ui/dialogs.js');
  try {
    const {settingsPage} = await import('../src/console/pages/settings.js');
    const page = await settingsPage(); dom.body.append(page); dom.mutations(); await flush();
    const control = elements(page).find((element) => element.name === 'public_page_enabled');
    assert.ok(control);
    assert.equal(control.checked, true);
    const toggle = () => { control.checked = !control.checked; control.dispatchEvent(new Event('change')); };
    toggle(); await flush();
    assert.equal(writes.length, 0);
    const cancel = elements(dom.body.querySelector('dialog')).find((element) => element.textContent === 'Cancel');
    cancel.click(); await flush();
    assert.equal(control.checked, true); assert.equal(writes.length, 0);
    toggle(); await flush();
    elements(dom.body.querySelector('dialog')).find((element) => element.tagName === 'button' && element.textContent === 'Disable public page').click();
    await flush();
    assert.equal(control.disabled, true); assert.deepEqual(writes, [{enabled: false}]);
    resolveWrite(new Response(JSON.stringify({code: 'internal_error', message: 'Could not save setting.'}), {status: 500}));
    await flush();
    assert.equal(control.checked, true); assert.equal(control.disabled, false);
    toggle(); await flush();
    elements(dom.body.querySelector('dialog')).find((element) => element.tagName === 'button' && element.textContent === 'Disable public page').click();
    await flush(); resolveWrite(new Response(JSON.stringify({enabled: false}))); await flush();
    assert.equal(control.checked, false);
    toggle(); await flush();
    assert.deepEqual(writes.at(-1), {enabled: true});
    assert.equal(dom.body.querySelector('dialog'), null);
    resolveWrite(new Response(JSON.stringify({enabled: true}))); await flush();
    assert.equal(control.checked, true); assert.equal(control.disabled, false);
  } finally {
    closeDialogs(); invalidate('nodes', 'groups', 'users'); setSession({user: null, csrf_token: '', expires_at: ''});
    setNavigationSignal(null);
    Object.defineProperty(reducedMotion, 'matches', motion);
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    dom.restore();
  }
});

test('self deletion clears the session without fetching the administrator list again', async () => {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window', 'matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  const self = {id: 'user-one', username: 'admin-one', must_change_password: false, created_at: '2026-10-27T12:00:00Z'};
  const requests = [];
  Object.assign(globalThis, {window: new EventTarget(), matchMedia: () => ({matches: true, addEventListener() {}}),
    history: {state: null, replaceState() {}}, requestAnimationFrame: () => 0});
  Object.assign(location, {pathname: '/settings', hash: '#administrators'});
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
    await flush();
    elements(page).find((element) => element.getAttribute('aria-label') === 'Delete administrator "admin-one"').click();
    const dialog = dom.body.querySelector('dialog');
    elements(dialog).find((element) => element.tagName === 'button' && element.textContent === 'Delete my account').click();
    dialog.dispatchEvent(new Event('animationend'));
    await flush();
    assert.equal(session.user, null);
    assert.equal(session.csrf, '');
    assert.deepEqual(requests.at(-1), {path: '/api/v1/users/user-one', method: 'DELETE'});
    assert.equal(requests.filter((request) => request.path === '/api/v1/users').length, 1);
  } finally {
    closeDialogs();
    invalidate('nodes', 'groups', 'users');
    dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});
