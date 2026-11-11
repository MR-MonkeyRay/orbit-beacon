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
  globalThis.fetch = async (url) => {
    if (url.endsWith('/alerts/summary')) return Response.json({firing: {total: 0, critical: 0}});
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

test('alert badge shares summary cadence, colors critical, hides zero, retains stale values and stops across session boundaries', async () => {
  const dom = installDOM();
  const originals = new Map(['window', 'fetch', 'setTimeout', 'clearTimeout'].map((key) => [key, globalThis[key]]));
  globalThis.window = new EventTarget();
  globalThis.setTimeout = () => 1; globalThis.clearTimeout = () => {};
  const entry = document.createElement('a'); entry.className = 'alert-entry';
  const badge = document.createElement('span'); badge.className = 'alert-count'; entry.append(badge); dom.body.append(entry);
  location.pathname = '/overview';
  let firing = {total: 24, critical: 1};
  let fail = false;
  let release;
  let pending = false;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({url, signal: options.signal});
    if (url.endsWith('/notifications/summary')) return Response.json({unread_count: 3, latest_id: 'one'});
    if (pending) return new Promise((resolve) => { release = () => resolve(Response.json({firing: {total: 99, critical: 99}})); });
    return fail ? new Response('{}', {status: 503}) : Response.json({firing});
  };
  const account = {user: {id: 'alert-admin', username: 'Admin', must_change_password: false}, csrf_token: 'csrf', expires_at: ''};
  setSession({user: null, csrf_token: '', expires_at: ''});
  const {initNotifications, refreshNotifications} = await import('../src/console/notifications.js');
  const stop = initNotifications();
  try {
    await refreshNotifications(); assert.equal(calls.length, 0);
    setSession(account); await flush();
    assert.equal(dom.timers.size, 1);
    assert.equal(badge.hidden, false); assert.equal(badge.textContent, '24');
    assert.equal(badge.classList.contains('critical'), true);
    assert.equal(entry.getAttribute('aria-label'), 'Alerts, 24 firing');
    firing = {total: 4, critical: 0}; await refreshNotifications();
    assert.equal(badge.classList.contains('critical'), false);
    fail = true; await refreshNotifications();
    assert.equal(badge.textContent, '4'); assert.equal(entry.classList.contains('stale'), true);
    assert.equal(entry.getAttribute('aria-label'), 'Alerts, 4 firing, stale');
    fail = false; firing = {total: 0, critical: 0}; await refreshNotifications();
    assert.equal(badge.hidden, true); assert.equal(entry.classList.contains('stale'), false);
    pending = true; const work = refreshNotifications(); await flush();
    const inflight = calls.at(-1);
    setSession({user: null, csrf_token: '', expires_at: ''});
    assert.equal(inflight.signal.aborted, true); assert.equal(dom.timers.size, 0);
    release(); await work; assert.equal(badge.hidden, true); assert.equal(badge.textContent, '0');
    const count = calls.length; await refreshNotifications(); assert.equal(calls.length, count);
    setSession({...account, user: {...account.user, must_change_password: true}}); await flush();
    assert.equal(calls.length, count); assert.equal(dom.timers.size, 0);
  } finally {
    stop(); setSession({user: null, csrf_token: '', expires_at: ''}); dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});
