import test from 'node:test';
import assert from 'node:assert/strict';
import {flush, installDOM} from './console-dom.js';
import {setSession, updateUser} from '../src/console/api.js';

test('notifications activate after SPA login, survive rename, and reset on account switch', async () => {
  const dom = installDOM();
  const originals = new Map(['window', 'matchMedia', 'fetch'].map((key) => [key, globalThis[key]]));
  globalThis.window = new EventTarget();
  globalThis.matchMedia = () => ({matches: false, addEventListener() {}});
  dom.document.querySelectorAll = (selector) => dom.body.querySelectorAll(selector);
  for (const id of ['sidebar', 'account', 'menu', 'scrim']) {
    const element = document.createElement('div');
    element.id = id;
    dom.body.append(element);
  }
  const entry = document.createElement('button');
  entry.className = 'notification-entry';
  const count = document.createElement('span');
  count.className = 'notification-count';
  entry.append(count);
  dom.body.append(entry);
  location.pathname = '/login';
  setSession({user: null, csrf_token: '', expires_at: ''});
  let requests = 0;
  let resolveSecond;
  globalThis.fetch = async () => {
    requests += 1;
    if (requests === 2) return new Promise((resolve) => { resolveSecond = resolve; });
    return new Response(JSON.stringify({unread_count: 2, latest_id: 'first'}));
  };
  try {
    const {initNotifications} = await import('../src/console/notifications.js');
    const {setActiveNav} = await import('../src/console/shell.js?notifications-lifecycle');
    initNotifications();
    setSession({user: {id: 'one', username: 'admin-one'}, csrf_token: 'csrf-one', expires_at: ''});
    assert.equal(requests, 0);
    location.pathname = '/overview';
    setActiveNav('/overview');
    await flush();
    assert.equal(count.textContent, '2');
    assert.equal(dom.timers.size, 1);
    updateUser({username: 'renamed'});
    setActiveNav('/settings');
    await flush();
    assert.equal(requests, 1);
    assert.equal(dom.timers.size, 1);
    setSession({user: {id: 'two', username: 'admin-two'}, csrf_token: 'csrf-two', expires_at: ''});
    assert.equal(count.textContent, '…');
    assert.equal(dom.timers.size, 1);
    resolveSecond(new Response(JSON.stringify({unread_count: 7, latest_id: 'second'})));
    await flush();
    assert.equal(count.textContent, '7');
    assert.equal(requests, 2);
    setSession({user: null, csrf_token: '', expires_at: ''});
    location.pathname = '/login';
    setActiveNav(null);
    assert.equal(entry.hidden, true);
    assert.equal(dom.timers.size, 0);
  } finally {
    setSession({user: null, csrf_token: '', expires_at: ''});
    dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});
