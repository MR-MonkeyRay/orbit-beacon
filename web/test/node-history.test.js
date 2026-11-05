import test from 'node:test';
import assert from 'node:assert/strict';
import {historyPanel} from '../src/console/pages/node-history.js';
import {setNavigationSignal} from '../src/console/api.js';
import {copy, copyPattern, elements, flush, installDOM} from './console-dom.js';

function setup() {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = (path, options) => new Promise((resolve) => requests.push({path, signal: options.signal, resolve}));
  setNavigationSignal(null);
  const page = new AbortController();
  const root = historyPanel('node-id', page.signal);
  dom.body.append(root);
  dom.mutations();
  const respond = (request, status = 200, points = [], unitOverride) => {
    const query = new URL(request.path, 'https://example.com').searchParams;
    const metric = query.get('metric');
    const range = query.get('range');
    const unit = unitOverride ?? (metric.startsWith('net_') ? 'bytes_per_second' : ['load1', 'load5', 'load15'].includes(metric) ? 'load' : 'percent');
    const step = ['30d', '90d'].includes(range) ? 3600 : ['24h', '7d'].includes(range) ? 300 : 60;
    request.resolve(new Response(JSON.stringify(status >= 400 ? {code: status === 404 ? 'node_not_found' : 'internal_error'}
      : {metric, range, unit, points, step_seconds: step}), {status}));
  };
  return {...dom, page, root, requests, respond, cleanup: () => { page.abort(); globalThis.fetch = originalFetch; dom.restore(); }};
}

test('history range changes abort stale requests and late responses cannot repaint', async () => {
  const env = setup();
  try {
    assert.equal(env.requests.length, 6);
    const initial = [...env.requests];
    elements(env.root).find((element) => element.id === 'history-range-1h').click();
    assert.equal(env.requests.length, 12);
    assert.ok(initial.every((request) => request.signal.aborted));
    const current = env.requests.slice(6);
    assert.ok(current.every((request) => request.path.endsWith('range=1h')));
    current.forEach((request) => env.respond(request));
    await flush();
    const charts = env.root.querySelector('.history-grid');
    assert.equal(charts.dataset.range, '1h');
    assert.equal(charts.children.length, 5);
    const content = charts.children[0];
    initial.forEach((request) => env.respond(request));
    await flush();
    assert.equal(charts.children[0], content);
    assert.doesNotMatch(env.root.textContent, new RegExp(`${copyPattern('nodes.historyPartial').source}|${copyPattern('nodes.historyLoadError', {error: ''}).source}`));
  } finally { env.cleanup(); }
});
test('history partial failures render a working retry without announcing each chart separately', async () => {
  const env = setup();
  try {
    env.respond(env.requests[0], 503);
    env.requests.slice(1).forEach((request) => env.respond(request));
    await flush();
    const retry = env.root.querySelector('#history-retry-cpu_percent');
    assert.ok(retry);
    assert.equal(retry.parentElement.querySelector('p').getAttribute('role'), null);
    retry.click();
    assert.equal(env.requests.length, 12);
  } finally { env.cleanup(); }
});
test('history clears polling and aborts requests on disconnect, and resumes on reattach', async () => {
  const env = setup();
  try {
    assert.equal(env.timers.size, 1);
    env.root.remove();
    env.mutations();
    assert.equal(env.timers.size, 0);
    assert.ok(env.requests.every((request) => request.signal.aborted));
    env.body.append(env.root);
    env.mutations();
    assert.equal(env.timers.size, 1);
    assert.equal(env.requests.length, 12);
    env.page.abort();
    assert.equal(env.timers.size, 0);
  } finally { env.cleanup(); }
});
test('history 404 tears down sibling requests, charts and interval permanently', async () => {
  const env = setup();
  try {
    env.respond(env.requests[0], 404);
    await flush();
    assert.equal(env.timers.size, 0);
    assert.ok(env.requests.slice(1).every((request) => request.signal.aborted));
    assert.equal(env.root.querySelector('.segmented').hidden, true);
    assert.match(env.root.textContent, copyPattern('nodes.nodeMissingPrefix', {link: copy('nodes.goNodes')}));
    assert.equal(env.root.querySelector('.history-grid').children.length, 0);
    env.requests.slice(1).forEach((request) => env.respond(request));
    await flush();
    assert.match(env.root.textContent, copyPattern('nodes.nodeMissingPrefix', {link: copy('nodes.goNodes')}));
    env.root.remove(); env.mutations(); env.body.append(env.root); env.mutations();
    assert.equal(env.timers.size, 0);
    assert.equal(env.requests.length, 6);
  } finally { env.cleanup(); }
});

test('history offers ninety days and requests the selected range', async () => {
  const env = setup();
  try {
    const control = env.root.querySelector('#history-range-90d');
    assert.equal(control.textContent, copy('nodes.range.days', {count: 90}));
    control.click();
    assert.equal(control.getAttribute('aria-pressed'), 'true');
    assert.ok(env.requests.slice(0, 6).every((request) => request.signal.aborted));
    const current = env.requests.slice(6);
    assert.equal(current.length, 6);
    assert.ok(current.every((request) => new URL(request.path, 'https://example.com').searchParams.get('range') === '90d'));
    current.forEach((request) => env.respond(request));
    await flush();
    assert.equal(env.root.querySelector('.history-grid').children.length, 5);
    assert.doesNotMatch(env.root.textContent, copyPattern('nodes.historyPartial'));
  } finally { env.cleanup(); }
});

test('load selector stays in one chart header and requests each selected metric', async () => {
  const env = setup();
  try {
    env.requests.forEach((request) => env.respond(request));
    await flush();
    for (const minutes of [5, 15, 1]) {
      const start = env.requests.length;
      const button = env.root.querySelector(`#history-load-load${minutes}`);
      assert.equal(button.textContent, copy('nodes.duration.minutes', {minutes}));
      assert.equal(button.parentElement.className, 'segmented');
      assert.equal(button.parentElement.getAttribute('aria-label'), copy('nodes.load'));
      assert.equal(button.parentElement.parentElement.querySelector('h3').textContent, copy('nodes.load'));
      button.focus();
      button.click();
      assert.equal(env.document.activeElement, button);
      assert.equal(button.getAttribute('aria-pressed'), 'true');
      const current = env.requests.slice(start);
      assert.equal(current.length, 6);
      const metrics = current.map((request) => new URL(request.path, 'https://example.com').searchParams.get('metric'));
      assert.deepEqual(metrics, ['cpu_percent', 'memory_used_percent', `load${minutes}`, 'disk_used_percent', 'net_rx_bytes_per_second', 'net_tx_bytes_per_second']);
      current.forEach((request) => env.respond(request));
      await flush();
      assert.equal(env.root.querySelector('.history-grid').children.length, 5);
      assert.equal(env.root.querySelectorAll('.segmented').length, 2);
      assert.equal(env.document.activeElement, button);
      button.click();
      assert.equal(env.requests.length, start + 6);
      assert.doesNotMatch(env.root.textContent, copyPattern('nodes.historyPartial'));
    }
  } finally { env.cleanup(); }
});

test('rapid load and range changes abort old requests and ignore late success and 404 responses', async () => {
  const env = setup();
  try {
    const initial = [...env.requests];
    env.root.querySelector('#history-load-load5').click();
    const five = env.requests.slice(6);
    env.root.querySelector('#history-load-load15').click();
    const fifteen = env.requests.slice(12);
    env.root.querySelector('#history-range-90d').click();
    const current = env.requests.slice(18);
    assert.equal(current.length, 6);
    assert.ok([...initial, ...five, ...fifteen].every((request) => request.signal.aborted));
    assert.ok(current.every((request) => !request.signal.aborted && request.path.endsWith('range=90d')));
    assert.ok(current.some((request) => request.path.includes('metric=load15&')));
    current.forEach((request) => env.respond(request, 200, [{t: '2026-11-05T12:00:00Z', avg: 3, max: 4}]));
    await flush();
    const charts = env.root.querySelector('.history-grid');
    const loadChart = charts.querySelector('#history-chart-load15');
    assert.ok(loadChart);
    assert.equal(charts.dataset.range, '90d');
    assert.equal(charts.children.length, 5);
    const rendered = [...charts.children];
    initial.forEach((request) => env.respond(request));
    five.forEach((request) => env.respond(request, 404));
    fifteen.forEach((request) => env.respond(request, 200, [{t: '2026-11-05T12:00:00Z', avg: 1, max: 2}]));
    await flush();
    rendered.forEach((section, index) => assert.equal(charts.children[index], section));
    assert.equal(charts.querySelector('#history-chart-load15'), loadChart);
    assert.equal(env.root.querySelector('#history-load-load15').getAttribute('aria-pressed'), 'true');
    assert.equal(charts.getAttribute('aria-busy'), 'false');
    assert.equal(env.timers.size, 1);
    assert.doesNotMatch(env.root.textContent, copyPattern('nodes.historyPartial'));
    assert.doesNotMatch(env.root.textContent, copyPattern('nodes.nodeMissingPrefix', {link: copy('nodes.goNodes')}));
  } finally { env.cleanup(); }
});

test('selected load keeps retry, unit validation and node removal handling', async () => {
  const env = setup();
  try {
    env.root.querySelector('#history-load-load5').click();
    const current = env.requests.slice(6);
    for (const request of current) env.respond(request, 200, [], request.path.includes('metric=load5&') ? 'percent' : undefined);
    await flush();
    assert.match(env.root.textContent, copyPattern('nodes.historyLoadError', {error: copy('errors.unreadable_response')}));
    const retry = env.root.querySelector('#history-retry-load5');
    assert.ok(retry);
    retry.click();
    const retried = env.requests.slice(12);
    assert.equal(retried.length, 6);
    const load = retried.find((request) => request.path.includes('metric=load5&'));
    assert.ok(load);
    env.respond(load, 404);
    await flush();
    assert.ok(retried.filter((request) => request !== load).every((request) => request.signal.aborted));
    assert.equal(env.root.querySelector('.history-grid').children.length, 0);
    assert.equal(env.timers.size, 0);
    assert.match(env.root.textContent, copyPattern('nodes.nodeMissingPrefix', {link: copy('nodes.goNodes')}));
  } finally { env.cleanup(); }
});
