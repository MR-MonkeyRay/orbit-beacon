import test from 'node:test';
import assert from 'node:assert/strict';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {flush, installDOM, TestElement} from './console-dom.js';

test('nav badge uses one summary request, polls every minute and pauses while hidden', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const originalWindow = globalThis.window;
  const originalMatchMedia = globalThis.matchMedia;
  const window = globalThis.window = new EventTarget();
  globalThis.matchMedia = () => ({matches: false, addEventListener() {}});
  setNavigationSignal(null);
  setSession({csrf_token: 'csrf', user: {username: 'Admin', role: 'admin'}, expires_at: ''});
  const sidebar = new TestElement('aside'); sidebar.id = 'sidebar';
  const anchor = new TestElement('a'); anchor.dataset.nav = '/alerts'; sidebar.append(anchor);
  dom.body.append(sidebar);
  for (const id of ['account', 'menu', 'scrim']) { const node = new TestElement('div'); node.id = id; dom.body.append(node); }
  const requests = [];
  globalThis.fetch = async (path) => {
    requests.push(path);
    return new Response(JSON.stringify({firing: {total: 2000}, newest: []}));
  };
  try {
    const {initShell} = await import('../src/console/shell.js?badge-test');
    initShell();
    await flush();
    assert.equal(anchor.querySelector('.alert-nav-count').textContent, '2000');
    assert.equal(anchor.getAttribute('aria-label'), 'Alerts, 2000 firing');
    assert.deepEqual(requests, ['/api/v1/alerts/summary']);
    assert.equal(dom.timers.size, 1);
    [...dom.timers.values()][0]();
    await flush();
    assert.deepEqual(requests, ['/api/v1/alerts/summary', '/api/v1/alerts/summary']);
    dom.document.hidden = true;
    dom.document.dispatchEvent(new Event('visibilitychange'));
    assert.equal(dom.timers.size, 0);
    dom.document.hidden = false;
    dom.document.dispatchEvent(new Event('visibilitychange'));
    assert.equal(dom.timers.size, 1);
    let pending;
    globalThis.fetch = (_path, options) => new Promise((resolve) => { pending = {signal: options.signal, resolve}; });
    [...dom.timers.values()][0]();
    window.dispatchEvent(new Event('pagehide'));
    assert.equal(pending.signal.aborted, true);
    assert.equal(dom.timers.size, 0);
    pending.resolve(new Response(JSON.stringify({firing: {total: 0}, newest: []})));
    await flush();
    assert.equal(anchor.querySelector('.alert-nav-count').textContent, '2000');
  } finally {
    window.dispatchEvent(new Event('pagehide'));
    globalThis.fetch = originalFetch;
    if (originalWindow === undefined) delete globalThis.window; else globalThis.window = originalWindow;
    if (originalMatchMedia === undefined) delete globalThis.matchMedia; else globalThis.matchMedia = originalMatchMedia;
    dom.restore();
  }
});
