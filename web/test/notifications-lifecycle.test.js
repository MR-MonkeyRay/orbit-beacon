import test from 'node:test';
import assert from 'node:assert/strict';
import {flush, installDOM} from './console-dom.js';
import {setSession, updateUser} from '../src/console/api.js';

test('notifications activate after SPA login, survive rename, and reset on account switch', async () => {
  const dom = installDOM();
  const originals = new Map(['window', 'matchMedia', 'fetch', 'performance', 'setTimeout', 'clearTimeout'].map((key) => [key, globalThis[key]]));
  const originalNow = Date.now;
  Date.now = () => Date.parse('2026-10-28T00:00:00Z');
  globalThis.performance = {now: () => 0};
  let deadlineID = 0;
  const deadlines = new Map();
  globalThis.setTimeout = (callback, delay) => { deadlines.set(++deadlineID, {callback, delay}); return deadlineID; };
  globalThis.clearTimeout = (id) => deadlines.delete(id);
  globalThis.window = new EventTarget();
  globalThis.matchMedia = () => ({matches: false, addEventListener() {}});
  dom.document.querySelectorAll = (selector) => dom.body.querySelectorAll(selector);
  for (const id of ['sidebar', 'account']) {
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
  let dispose;
  try {
    const {initNotifications} = await import('../src/console/notifications.js');
    const {setActiveNav} = await import('../src/console/shell.js?notifications-lifecycle');
    dispose = initNotifications();
    setSession({user: {id: 'one', username: 'admin-one', must_change_password: false}, csrf_token: 'csrf-one', expires_at: '2026-10-29T00:00:00Z'});
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
    setSession({user: {id: 'two', username: 'admin-two', must_change_password: false}, csrf_token: 'csrf-two', expires_at: '2026-10-29T00:00:00Z'});
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
    dispose?.();
    const pendingDeadlines = deadlines.size;
    Date.now = originalNow;
    dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    assert.equal(pendingDeadlines, 0);
  }
});

test('open notification inbox refreshes when unread count changes without a new latest notification', async () => {
  const dom = installDOM();
  const originals = new Map(['window', 'matchMedia', 'fetch', 'performance', 'setTimeout', 'clearTimeout'].map((key) => [key, globalThis[key]]));
  globalThis.performance = {now: () => 0};
  globalThis.setTimeout = () => 1;
  globalThis.clearTimeout = () => {};
  globalThis.window = new EventTarget();
  globalThis.matchMedia = () => ({matches: true, addEventListener() {}});
  dom.document.querySelectorAll = (selector) => dom.body.querySelectorAll(selector);
  const elementPrototype = Object.getPrototypeOf(dom.body);
  const originalClosest = elementPrototype.closest;
  elementPrototype.closest = function (selector) {
    let current = this;
    while (current) {
      if (selector === 'dialog' && current.tagName === 'dialog') return current;
      current = current.parentElement;
    }
    return null;
  };
  const entry = document.createElement('button');
  entry.className = 'notification-entry';
  const count = document.createElement('span');
  count.className = 'notification-count';
  entry.append(count);
  dom.body.append(entry);
  location.pathname = '/overview';
  setSession({user: {id: 'one', username: 'admin', must_change_password: false}, csrf_token: 'csrf', expires_at: '2099-10-29T00:00:00Z'});
  let unreadCount = 2;
  let listRequests = 0;
  globalThis.fetch = async (url) => {
    if (url.endsWith('/notifications/summary')) return new Response(JSON.stringify({unread_count: unreadCount, latest_id: 'same'}));
    if (url.includes('/notifications?')) {
      listRequests += 1;
      return new Response(JSON.stringify({notifications: [], next_cursor: null, has_more: false, read_through: null}));
    }
    throw new Error(`Unexpected request: ${url}`);
  };
  let dispose;
  try {
    const {initNotifications, refreshNotifications} = await import('../src/console/notifications.js?unread-refresh');
    dispose = initNotifications();
    await flush();
    entry.click();
    await flush();
    assert.equal(listRequests, 1);
    unreadCount = 1;
    await refreshNotifications();
    await flush();
    assert.equal(listRequests, 2);
  } finally {
    setSession({user: null, csrf_token: '', expires_at: ''});
    dispose?.();
    if (originalClosest === undefined) delete elementPrototype.closest; else elementPrototype.closest = originalClosest;
    dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});

test('notification inbox shares segmented filters, empty state and warning/error messages', async () => {
  const dom = installDOM();
  try {
    const {copy, elements} = await import('./console-dom.js');
    const {createNotificationInbox} = await import('../src/console/ui/notification-inbox.js');
    const {closeDialogs} = await import('../src/console/ui/dialogs.js');
    let fail = false;
    const inbox = createNotificationInbox({
      api: async () => {
        if (fail) throw new Error('Unavailable');
        return {notifications: [], has_more: false, read_through: null};
      },
      isActive: () => true, isStale: () => true,
      failureText: () => copy('controls.live.stale'), refreshSummary() {}, openTaskDetails() {}
    });
    try {
      await flush();
      const dialog = dom.body.querySelector('dialog');
      assert.ok(dialog.classList.contains('notifications-dialog'));
      const filters = dialog.querySelector('.segmented');
      assert.equal(filters.getAttribute('role'), 'group');
      assert.equal(filters.children[0].getAttribute('aria-pressed'), 'true');
      assert.equal(filters.children[1].getAttribute('aria-pressed'), 'false');
      assert.equal(dialog.querySelector('.empty').textContent, copy('notifications.noNotifications'));
      assert.equal(dialog.querySelector('.status-message').getAttribute('role'), 'status');
      filters.children[1].click(); await flush();
      assert.equal(filters.children[1].getAttribute('aria-pressed'), 'true');
      assert.equal(dialog.querySelector('.empty').textContent, copy('notifications.noUnread'));
      fail = true;
      elements(dialog).find((element) => element.tagName === 'button' && element.textContent === copy('common.refresh')).click();
      await flush();
      const error = elements(dialog).find((element) => element.classList.contains('error'));
      assert.ok(error.classList.contains('status-message'));
      assert.equal(error.getAttribute('role'), 'alert');
      assert.equal(error.hidden, false);
      assert.equal(dialog.querySelector('.empty').textContent, copy('notifications.noUnread'));
      inbox.setStale(false);
      assert.equal(dialog.querySelector('.status-message').hidden, true);
    } finally { closeDialogs(); await inbox.closed; }
  } finally { dom.restore(); }
});
