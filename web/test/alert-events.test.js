import test from 'node:test';
import assert from 'node:assert/strict';
import {activeAlertsSummary, eventRows, eventsPanel} from '../src/console/pages/alert-events.js';
import {invalidate, setNavigationSignal, setSession} from '../src/console/api.js';
import {elements, flush, installDOM} from './console-dom.js';

const event = (id, extra = {}) => ({id, rule_id: 'deleted-rule', rule_name: 'High CPU', node_id: 'node-id', node_name: 'Edge', metric: 'cpu_percent', severity: 'critical', state: 'firing', value: 90.1, started_at: '2026-10-27T12:00:00Z', resolved_at: null, ...extra});
const respond = (body) => new Response(JSON.stringify(body));

test('event rows use stored metric/severity snapshots, including unknown metadata', () => {
  const dom = installDOM();
  try {
    const [row, unknown] = eventRows([event('one'), event('two', {metric: '', severity: ''})]);
    assert.match(row.textContent, /Critical/);
    assert.match(row.textContent, /90.1%/);
    assert.match(unknown.textContent, /Unknown severity/);
    assert.match(unknown.textContent, /unknown metric/);
    assert.match(eventRows([event('three', {metric: '', severity: '', value: null})])[0].textContent, /Unavailable \(unknown metric\)/);
    const summary = activeAlertsSummary({firing: {total: 7000, warning: 6999, unknown: 1, info: 0, critical: 0},
      newest: Array.from({length: 5}, (_, i) => event(String(i), {severity: 'warning'}))});
    assert.match(summary.textContent, /View alerts \(7000\)/);
    assert.match(summary.textContent, /Unknown severity/);
    assert.match(summary.textContent, /6999/);
    assert.match(activeAlertsSummary({firing: {total: 0}, newest: []}).textContent, /No firing alerts/);
    assert.equal(elements(summary).filter((element) => element.tagName === 'li').length, 5);
  } finally { dom.restore(); }
});
test('overview requests one alert summary and uses aggregate counts with only the newest events', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const originalWindow = globalThis.window;
  const originalMatchMedia = globalThis.matchMedia;
  globalThis.window = new EventTarget();
  globalThis.matchMedia = () => ({matches: false, addEventListener() {}});
  const requests = [];
  const page = new AbortController();
  setNavigationSignal(page.signal);
  setSession({user: {id: 'admin-two', username: 'admin-two', must_change_password: false}, csrf_token: 'csrf', expires_at: ''});
  invalidate('nodes', 'groups', 'users');
  globalThis.fetch = async (path) => {
    requests.push(path);
    if (path === '/api/v1/alerts/summary') return respond({firing: {total: 2000, critical: 1998, warning: 1, unknown: 1, info: 0}, newest: [event('one')]});
    if (path.startsWith('/api/v1/nodes?')) return respond({nodes: [], has_more: false});
    if (path === '/api/v1/groups') return respond({groups: []});
    if (path === '/api/v1/users') return respond({users: []});
    if (path.startsWith('/api/v1/audit?')) return respond({events: []});
    throw new Error(`Unexpected request: ${path}`);
  };
  try {
    const {overviewPage} = await import('../src/console/pages/overview.js');
    const root = await overviewPage();
    assert.match(root.textContent, /View alerts \(2000\)/);
    assert.match(root.textContent, /1998/);
    assert.match(root.textContent, /High CPU/);
    assert.equal(elements(root).filter((element) => element.tagName === 'li').length, 1);
    assert.deepEqual(requests.filter((path) => path.startsWith('/api/v1/alerts')), ['/api/v1/alerts/summary']);
  } finally {
    page.abort();
    setNavigationSignal(null);
    await flush();
    invalidate('nodes', 'groups', 'users');
    globalThis.fetch = originalFetch;
    if (originalWindow === undefined) delete globalThis.window; else globalThis.window = originalWindow;
    if (originalMatchMedia === undefined) delete globalThis.matchMedia; else globalThis.matchMedia = originalMatchMedia;
    dom.restore();
  }
});
test('alert history follows next_cursor and appends the last full page without guessing has_more', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const requests = [];
  const page = new AbortController();
  setNavigationSignal(null);
  globalThis.fetch = async (path) => {
    requests.push(path);
    return respond(requests.length === 1 ? {events: [event('first')], has_more: true, next_cursor: 'cursor/+value'}
      : {events: Array.from({length: 50}, (_, i) => event(`older-${i}`)), has_more: false});
  };
  try {
    const root = eventsPanel('all', page.signal);
    dom.body.append(root); dom.mutations();
    await flush();
    const more = elements(root).find((element) => element.tagName === 'button' && element.textContent === 'Load more');
    assert.equal(more.hidden, false);
    more.click();
    await flush();
    assert.equal(requests[1], '/api/v1/alerts?state=all&limit=50&before=cursor%2F%2Bvalue');
    assert.equal(more.hidden, true);
    assert.equal(elements(root).filter((element) => element.tagName === 'tbody')[0].children.length, 51);
  } finally { page.abort(); globalThis.fetch = originalFetch; dom.restore(); }
});
test('detaching an alert history tab aborts in-flight loads and ignores late results', async () => {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const page = new AbortController();
  let request;
  setNavigationSignal(null);
  globalThis.fetch = (path, options) => new Promise((resolve) => { request = {resolve, signal: options.signal}; });
  try {
    const root = eventsPanel('all', page.signal);
    dom.body.append(root); dom.mutations();
    root.remove(); dom.mutations();
    assert.equal(request.signal.aborted, true);
    request.resolve(respond({events: [event('late')], has_more: false}));
    await flush();
    assert.doesNotMatch(root.textContent, /High CPU/);
  } finally { page.abort(); globalThis.fetch = originalFetch; dom.restore(); }
});
