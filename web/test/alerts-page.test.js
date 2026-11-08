import test from 'node:test';
import assert from 'node:assert/strict';
import {setNavigationSignal, setSession, invalidate} from '../src/console/api.js';
import {copy, elements, flush, installDOM} from './console-dom.js';

test('alerts tabs load independently: one failed resource keeps the others and offers an inline retry', async () => {
  const dom = installDOM();
  const originals = new Map(['fetch', 'window', 'matchMedia', 'history', 'requestAnimationFrame'].map((key) => [key, globalThis[key]]));
  let ruleRequests = 0;
  Object.assign(globalThis, {window: new EventTarget(), matchMedia: () => ({matches: true, addEventListener() {}}),
    history: {state: null, replaceState() {}}, requestAnimationFrame: (callback) => callback()});
  Object.assign(location, {pathname: '/alerts', search: '', hash: ''});
  setNavigationSignal(null);
  setSession({csrf_token: 'csrf', user: {id: 'user', username: 'admin', must_change_password: false, created_at: '2026-10-27T12:00:00Z'}, expires_at: ''});
  globalThis.fetch = async (path) => {
    const url = String(path);
    if (url.startsWith('/api/v1/alert-rules')) {
      ruleRequests += 1;
      if (ruleRequests === 1) return new Response(JSON.stringify({message: 'Rules could not be loaded.'}), {status: 500});
      return new Response(JSON.stringify({rules: []}));
    }
    if (url.startsWith('/api/v1/alert-silences')) return new Response(JSON.stringify({silences: []}));
    if (url.startsWith('/api/v1/notification-channels')) return new Response(JSON.stringify({channels: []}));
    if (url.startsWith('/api/v1/nodes?')) return new Response(JSON.stringify({nodes: [], has_more: false}));
    if (url === '/api/v1/groups') return new Response(JSON.stringify({groups: []}));
    if (url === '/api/v1/alerts/summary') return new Response(JSON.stringify({firing: {total: 0}, newest: []}));
    return assert.fail(`Unexpected request ${url}`);
  };
  try {
    const {alertsPage} = await import('../src/console/pages/alerts.js');
    const page = await alertsPage();
    dom.body.append(page); dom.mutations();
    const tabs = elements(page).filter((element) => element.getAttribute('role') === 'tab');
    tabs.find((element) => element.id === 'tab-rules').click();
    await flush();
    const loadError = elements(page).find((element) => element.getAttribute('role') === 'alert');
    assert.ok(loadError?.textContent, 'rules tab shows its own load failure');
    // 原地重试恢复。
    elements(page).find((element) => element.tagName === 'button' && element.textContent === copy('common.retry')).click();
    await flush();
    assert.equal(ruleRequests, 2);
    assert.ok(elements(page).some((element) => element.tagName === 'button' && element.textContent === copy('alerts.rules.add')));
    // 静默标签不受规则加载失败影响。
    tabs.find((element) => element.id === 'tab-silences').click();
    await flush();
    assert.ok(elements(page).some((element) => element.tagName === 'button' && element.textContent === copy('alerts.silences.add')));
  } finally {
    invalidate('nodes');
    setSession({user: null, csrf_token: '', expires_at: ''});
    setNavigationSignal(null);
    dom.restore();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
  }
});
