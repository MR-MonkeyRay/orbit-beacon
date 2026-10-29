import test from 'node:test';
import assert from 'node:assert/strict';
import {historyPanel} from '../src/console/pages/node-history.js';
import {setNavigationSignal} from '../src/console/api.js';
import {elements, flush, installDOM} from './console-dom.js';

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
  const respond = (request, status = 200) => {
    const metric = new URL(request.path, 'https://example.com').searchParams.get('metric');
    const unit = metric.startsWith('net_') ? 'bytes_per_second' : metric === 'load1' ? 'load' : 'percent';
    request.resolve(new Response(JSON.stringify(status === 404 ? {code: 'node_not_found', message: 'Not found'} : {unit, points: [], step_seconds: 60}), {status}));
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
    assert.doesNotMatch(env.root.textContent, /could not load/);
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
    assert.match(env.root.textContent, /This node no longer exists/);
    assert.equal(env.root.querySelector('.history-grid').children.length, 0);
    env.requests.slice(1).forEach((request) => env.respond(request));
    await flush();
    assert.match(env.root.textContent, /This node no longer exists/);
    env.root.remove(); env.mutations(); env.body.append(env.root); env.mutations();
    assert.equal(env.timers.size, 0);
    assert.equal(env.requests.length, 6);
  } finally { env.cleanup(); }
});
