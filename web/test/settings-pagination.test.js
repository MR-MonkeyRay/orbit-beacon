import test from 'node:test';
import assert from 'node:assert/strict';
import {invalidate, setSession} from '../src/console/api.js';

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
  setSession({csrf_token: 'csrf', user: {id: 'user', username: 'admin', role: 'admin', created_at: '2026-10-27T12:00:00Z'}, expires_at: ''});
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
  }
});
