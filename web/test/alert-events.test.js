import test from 'node:test';
import assert from 'node:assert/strict';
import {activeAlertsSummary, eventRows, eventsPanel} from '../src/console/pages/alert-events.js';
import {invalidate, setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, elements, flush, installDOM} from './console-dom.js';

const event = (id, extra = {}) => ({id, rule_id: 'deleted-rule', rule_name: 'High CPU', node_id: 'node-id', node_name: 'Edge', metric: 'cpu_percent', severity: 'critical', state: 'firing', value: 90.1, started_at: '2026-10-27T12:00:00Z', resolved_at: null, ...extra});
const respond = (body) => new Response(JSON.stringify(body));

test('event rows use stored metric/severity snapshots, including unknown metadata', () => {
  const dom = installDOM();
  try {
    const [row, unknown] = eventRows([event('one'), event('two', {metric: '', severity: ''})]);
    assert.ok(elements(row).some((element) => element.classList.contains('alert-critical')));
    assert.match(row.textContent, /90\.1%/);
    assert.ok(elements(unknown).some((element) => element.classList.contains('alert-unknown')));
    assert.equal(parseFloat(unknown.children[3].textContent), 90.1);
    assert.ok(Number.isNaN(parseFloat(eventRows([event('three', {metric: '', severity: '', value: null})])[0].children[3].textContent)));
    const summary = activeAlertsSummary({firing: {total: 7000, warning: 6999, unknown: 1, info: 0, critical: 0},
      newest: Array.from({length: 5}, (_, i) => event(String(i), {severity: 'warning'}))});
    assert.ok(elements(summary).some((element) => element.tagName === 'a' && element.getAttribute('href') === '/alerts'));
    assert.ok(elements(summary).some((element) => element.classList.contains('alert-unknown')));
    assert.match(summary.textContent, /6999/);
    assert.equal(elements(activeAlertsSummary({firing: {total: 0}, newest: []})).filter((element) => element.tagName === 'li').length, 0);
    assert.equal(elements(summary).filter((element) => element.tagName === 'li').length, 5);
    const ordered = activeAlertsSummary({firing: {total: 2}, newest: [event('warning', {rule_name: 'Warning', severity: 'warning'}), event('critical', {rule_name: 'Critical', started_at: '2020-01-01'})]});
    assert.match(elements(ordered).find((e) => e.tagName === 'li').textContent, /^Critical/);
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
    assert.ok(elements(root).some((element) => element.tagName === 'a' && element.getAttribute('href') === '/alerts'));
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
    const more = elements(root).find((element) => element.tagName === 'button' && element.textContent === copy('common.loadMore'));
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

test('active table loads all pages, sorts, filters and groups with remembered controls and duration columns', async () => {
  const dom = installDOM();
  const original = globalThis.fetch;
  const page = new AbortController();
  let requests = 0, stored;
  localStorage.getItem = () => stored;
  localStorage.setItem = (_key, value) => { stored = value; };
  globalThis.fetch = async () => respond(++requests % 2 ? {events: [event('warning', {severity: 'warning', rule_id: 'a'}), event('info', {severity: 'info', rule_id: 'b'})], has_more: true, next_cursor: 'older'} :
    {events: [event('critical', {rule_id: 'a', silenced: true, started_at: '2020-01-01T00:00:00Z'})], has_more: false});
  try {
    const root = eventsPanel('firing', page.signal, {getRules: () => [{id: 'a', metric: 'cpu_percent', comparator: '>', threshold: 80}]});
    dom.body.append(root); dom.mutations(); await flush();
    const headers = elements(root).filter((e) => e.getAttribute('scope') === 'col').map((e) => e.textContent);
    assert.ok(headers.includes(copy('alerts.column.duration')));
    assert.ok(!headers.includes(copy('alerts.column.state')));
    assert.ok(!headers.includes(copy('alerts.column.resolved')));
    const dataRows = () => elements(root).filter((e) => e.dataset.eventId);
    assert.deepEqual(dataRows().map((e) => e.dataset.eventId), ['critical', 'warning', 'info']);
    assert.ok(dataRows()[0].textContent.includes(copy('alerts.silences.silenced')));
    assert.match(dataRows()[0].textContent, /90.1% > 80%/);
    assert.ok(dataRows()[0].children[5].textContent);
    const updated = elements(root).find((e) => e.classList.contains('alert-updated'));
    assert.equal(updated.children[0].tagName, 'time');
    assert.ok(updated.children[0].getAttribute('datetime'));
    assert.doesNotMatch(updated.textContent, /object Object/);
    const critical = elements(root).find((e) => e.dataset.severity === 'critical');
    assert.equal(critical.lastElementChild.textContent, '1');
    critical.click();
    assert.equal(critical.getAttribute('aria-pressed'), 'true');
    assert.deepEqual(dataRows().map((e) => e.dataset.eventId), ['critical']);
    elements(root).find((e) => e.dataset.severity === 'all').click();
    elements(root).find((e) => e.dataset.grouping === 'rule').click();
    const groups = elements(root).filter((e) => e.getAttribute('scope') === 'rowgroup');
    assert.equal(groups.length, 2);
    assert.match(groups[0].textContent, /\(2\)/);
    assert.equal(groups[0].getAttribute('colspan'), '7');
    assert.ok(elements(groups[0]).some((e) => e.classList.contains('alert-critical')));
    assert.equal(elements(root).filter((e) => e.tagName === 'table').length, 1);
    assert.equal(elements(root).filter((e) => e.tagName === 'tbody').length, 2);
    elements(root).find((e) => e.dataset.grouping === 'node').click();
    assert.equal(elements(root).filter((e) => e.getAttribute('scope') === 'rowgroup').length, 1);
    assert.deepEqual(JSON.parse(stored), {severity: 'all', grouping: 'node'});
    root.remove(); dom.mutations();
    const remembered = eventsPanel('firing', page.signal); dom.body.append(remembered); dom.mutations(); await flush();
    assert.equal(elements(remembered).find((e) => e.dataset.grouping === 'node').getAttribute('aria-pressed'), 'true');
  } finally { page.abort(); globalThis.fetch = original; dom.restore(); }
});

test('active polling pauses hidden requests, ignores cancelled results and stops on navigation or tab detach', async () => {
  const dom = installDOM();
  const original = globalThis.fetch;
  const page = new AbortController();
  const requests = [];
  globalThis.fetch = (_path, options) => new Promise((resolve) => requests.push({resolve, signal: options.signal}));
  try {
    const root = eventsPanel('firing', page.signal); dom.body.append(root); dom.mutations();
    assert.equal(dom.timers.size, 1);
    requests[0].resolve(respond({events: [event('first')], has_more: false})); await flush();
    [...dom.timers.values()].forEach((tick) => tick());
    assert.equal(requests.length, 2);
    document.hidden = true; document.dispatchEvent(new Event('visibilitychange'));
    assert.equal(requests[1].signal.aborted, true);
    [...dom.timers.values()].forEach((tick) => tick()); assert.equal(requests.length, 2);
    document.hidden = false; document.dispatchEvent(new Event('visibilitychange'));
    assert.equal(requests.length, 3);
    requests[1].resolve(respond({events: [event('late')], has_more: false})); await flush();
    assert.ok(!elements(root).some((e) => e.dataset.eventId === 'late'));
    page.abort();
    assert.equal(requests[2].signal.aborted, true);
    assert.equal(dom.timers.size, 0);
    requests[2].resolve(respond({events: [event('late-again')], has_more: false})); await flush();
    assert.ok(!elements(root).some((e) => e.dataset.eventId === 'late-again'));
    const other = new AbortController();
    const detached = eventsPanel('firing', other.signal); dom.body.append(detached); dom.mutations();
    detached.remove(); dom.mutations();
    assert.equal(dom.timers.size, 0); assert.equal(requests.at(-1).signal.aborted, true);
    requests.at(-1).resolve(respond({events: [], has_more: false})); other.abort(); await flush();
  } finally { page.abort(); globalThis.fetch = original; dom.restore(); }
});

test('active row silence opens the shared editor with node prefill and refreshes badges after save', async () => {
  const dom = installDOM();
  const original = globalThis.fetch;
  const page = new AbortController();
  const id = '00000000-0000-4000-8000-000000000001';
  setNavigationSignal(page.signal);
  setSession({user: {id: 'admin', username: 'admin', must_change_password: false}, csrf_token: 'csrf', expires_at: ''});
  invalidate('groups', 'nodes');
  let created = false, alertRequests = 0, payload;
  globalThis.fetch = async (path, options) => {
    if (path.startsWith('/api/v1/alerts?')) { ++alertRequests; return respond({events: [event('one', {node_id: id, silenced: created})], has_more: false}); }
    if (path === '/api/v1/groups') return respond({groups: []});
    if (path.startsWith('/api/v1/nodes?')) return respond({nodes: [{id, name: 'Edge'}], has_more: false});
    if (path === '/api/v1/alert-silences') { payload = JSON.parse(options.body); created = true; return respond({}); }
    assert.fail(path);
  };
  try {
    const root = eventsPanel('firing', page.signal); dom.body.append(root); dom.mutations(); await flush();
    elements(root).find((e) => e.tagName === 'button' && e.textContent === copy('alerts.events.silence')).click(); await flush();
    const control = (name) => elements(dom.body).find((e) => e.name === name);
    assert.equal(control('scope_kind').value, 'node'); assert.equal(control('scope_id').value, id);
    control('reason').value = 'Maintenance';
    elements(dom.body).find((e) => e.tagName === 'form').dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(payload.scope_kind, 'node'); assert.equal(payload.scope_id, id);
    assert.equal(alertRequests, 2);
    assert.ok(root.textContent.includes(copy('alerts.silences.silenced')));
    const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs();
  } finally { page.abort(); setNavigationSignal(null); invalidate('groups', 'nodes'); setSession({user: null, csrf_token: '', expires_at: ''}); globalThis.fetch = original; dom.restore(); }
});
