import test from 'node:test';
import assert from 'node:assert/strict';
import {historyPanel} from '../src/console/pages/node-history.js';
import {setNavigationSignal} from '../src/console/api.js';
import {copy, copyPattern, elements, flush, installDOM} from './console-dom.js';

function setup(disks = []) {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = (path, options) => new Promise((resolve) => requests.push({path, signal: options.signal, resolve}));
  setNavigationSignal(null);
  const page = new AbortController();
  const root = historyPanel('node-id', page.signal, {metrics: {disks}});
  dom.body.append(root);
  dom.mutations();
  const respond = (request, status = 200, points = [], unitOverride) => {
    const query = new URL(request.path, 'https://example.com').searchParams;
    const metric = query.get('metric');
    const range = query.get('range');
    const unit = unitOverride ?? (metric === 'temperature_celsius' ? 'celsius' : metric.endsWith('_bytes_per_second') ? 'bytes_per_second' : ['load1', 'load5', 'load15'].includes(metric) ? 'load' : 'percent');
    const step = ['30d', '90d'].includes(range) ? 3600 : ['24h', '7d'].includes(range) ? 300 : 60;
    request.resolve(new Response(JSON.stringify(status >= 400 ? {code: status === 404 ? 'node_not_found' : 'internal_error'}
      : {metric, range, unit, points, step_seconds: step}), {status}));
  };
  return {...dom, page, root, requests, respond, cleanup: () => { page.abort(); globalThis.fetch = originalFetch; dom.restore(); }};
}

test('history range changes abort stale requests and late responses cannot repaint', async () => {
  const env = setup();
  try {
    assert.equal(env.requests.length, 14);
    const initial = [...env.requests];
    elements(env.root).find((element) => element.id === 'history-range-1h').click();
    assert.equal(env.requests.length, 28);
    assert.ok(initial.every((request) => request.signal.aborted));
    const current = env.requests.slice(14);
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
    assert.equal(env.requests.length, 28);
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
    assert.equal(env.requests.length, 28);
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
    assert.equal(env.requests.length, 14);
  } finally { env.cleanup(); }
});

test('history offers ninety days and requests the selected range', async () => {
  const env = setup();
  try {
    const control = env.root.querySelector('#history-range-90d');
    assert.equal(control.textContent, copy('nodes.range.days', {count: 90}));
    control.click();
    assert.equal(control.getAttribute('aria-pressed'), 'true');
    assert.ok(env.requests.slice(0, 14).every((request) => request.signal.aborted));
    const current = env.requests.slice(14);
    assert.equal(current.length, 14);
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
      assert.equal(current.length, 14);
      const metrics = current.map((request) => new URL(request.path, 'https://example.com').searchParams.get('metric'));
      assert.deepEqual(metrics, ['cpu_percent', 'memory_used_percent', `load${minutes}`, 'disk_used_percent', 'inode_used_percent', 'disk_read_bytes_per_second', 'disk_write_bytes_per_second', 'disk_busy_percent', 'pressure_cpu_percent', 'pressure_memory_percent', 'pressure_io_percent', 'temperature_celsius', 'net_rx_bytes_per_second', 'net_tx_bytes_per_second']);
      current.forEach((request) => env.respond(request));
      await flush();
      assert.equal(env.root.querySelector('.history-grid').children.length, 5);
      assert.equal(env.root.querySelectorAll('.segmented').length, 2);
      assert.equal(env.document.activeElement, button);
      button.click();
      assert.equal(env.requests.length, start + 14);
      assert.doesNotMatch(env.root.textContent, copyPattern('nodes.historyPartial'));
    }
  } finally { env.cleanup(); }
});

test('rapid load and range changes abort old requests and ignore late success and 404 responses', async () => {
  const env = setup();
  try {
    const initial = [...env.requests];
    env.root.querySelector('#history-load-load5').click();
    const five = env.requests.slice(14);
    env.root.querySelector('#history-load-load15').click();
    const fifteen = env.requests.slice(28);
    env.root.querySelector('#history-range-90d').click();
    const current = env.requests.slice(42);
    assert.equal(current.length, 14);
    assert.ok([...initial, ...five, ...fifteen].every((request) => request.signal.aborted));
    assert.ok(current.every((request) => !request.signal.aborted && request.path.endsWith('range=90d')));
    assert.ok(current.some((request) => request.path.includes('metric=load15&')));
    current.forEach((request) => env.respond(request, 200, [{t: '2026-11-05T12:00:00Z', avg: 3, max: 4}]));
    await flush();
    const charts = env.root.querySelector('.history-grid');
    const loadChart = charts.querySelector('#history-chart-load15');
    assert.ok(loadChart);
    assert.equal(charts.dataset.range, '90d');
    assert.equal(charts.children.length, 10);
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
    const current = env.requests.slice(14);
    for (const request of current) env.respond(request, 200, [], request.path.includes('metric=load5&') ? 'percent' : undefined);
    await flush();
    assert.match(env.root.textContent, copyPattern('nodes.historyLoadError', {error: copy('errors.unreadable_response')}));
    const retry = env.root.querySelector('#history-retry-load5');
    assert.ok(retry);
    retry.click();
    const retried = env.requests.slice(28);
    assert.equal(retried.length, 14);
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

test('disk selection shares disk and inode queries, aborts old requests and keeps charts visible', async () => {
  const env = setup([{path: '/'}, {path: '/data:one'}]);
  try {
    const selector = env.root.querySelector('#history-disk');
    assert.equal(selector.parentElement.hidden, false);
    assert.equal(selector.getAttribute('aria-label'), copy('nodes.disk'));
    assert.equal(selector.children[0].textContent, copy('nodes.allDisks'));
    assert.equal(selector.value, '');
    env.requests.forEach((request) => env.respond(request, 200, [{t: '2026-11-05T12:00:00Z', avg: 25, max: 50}]));
    await flush();
    const charts = env.root.querySelector('.history-grid');
    const rendered = [...charts.children];
    selector.value = '/data:one';
    selector.dispatchEvent(new Event('change'));
    rendered.forEach((section, i) => assert.equal(charts.children[i], section));
    const diskRequests = env.requests.slice(14);
    for (const request of diskRequests) {
      const query = new URL(request.path, 'https://example.com').searchParams;
      assert.equal(query.get('disk'), ['disk_used_percent', 'inode_used_percent'].includes(query.get('metric')) ? '/data:one' : null);
    }
    selector.value = '/';
    selector.dispatchEvent(new Event('change'));
    assert.ok(diskRequests.every((request) => request.signal.aborted));
    const current = env.requests.slice(28);
    current.forEach((request) => env.respond(request, 200, [{t: '2026-11-05T12:00:00Z', avg: 50, max: 60}]));
    await flush();
    const currentCharts = [...charts.children];
    diskRequests.forEach((request) => env.respond(request, 404));
    await flush();
    currentCharts.forEach((section, i) => assert.equal(charts.children[i], section));
    assert.equal(selector.value, '/');
    assert.ok(charts.querySelector('#history-chart-inode_used_percent'));
    selector.value = '';
    selector.dispatchEvent(new Event('change'));
    assert.ok(env.requests.slice(42).every((request) => !new URL(request.path, 'https://example.com').searchParams.has('disk')));
  } finally { env.cleanup(); }
});

test('single disk hides selection and live snapshots update available paths', () => {
  const env = setup([{path: '/'}]);
  try {
    const selector = env.root.querySelector('#history-disk');
    assert.equal(selector.parentElement.hidden, true);
    env.root.snapshot({metrics: {disks: [{path: '/'}, {path: '/new'}]}});
    assert.equal(selector.parentElement.hidden, false);
    assert.deepEqual(selector.children.map((option) => option.value), ['', '/', '/new']);
    selector.value = '/new';
    selector.dispatchEvent(new Event('change'));
    const previous = [...env.requests];
    env.root.snapshot({metrics: {disks: [{path: '/'}]}});
    assert.equal(selector.value, '');
    assert.equal(selector.parentElement.hidden, true);
    assert.ok(previous.every((request) => request.signal.aborted));
    assert.ok(env.requests.slice(previous.length).every((request) => !new URL(request.path, 'https://example.com').searchParams.has('disk')));
  } finally { env.cleanup(); }
});

test('optional chart stays visible when one of its series has points', async () => {
  const env = setup();
  try {
    env.requests.forEach((request) => env.respond(request, 200, request.path.includes('metric=pressure_io_percent&')
      ? [{t: '2026-11-05T12:00:00Z', avg: 25, max: 50}] : []));
    await flush();
    assert.ok(env.root.querySelector('#history-chart-pressure_cpu_percent'));
  } finally { env.cleanup(); }
});

test('optional host charts hide empty data, keep rendered charts while loading, and show errors', async () => {
  const env = setup();
  const ids = ['inode_used_percent', 'disk_read_bytes_per_second', 'disk_busy_percent', 'pressure_cpu_percent', 'temperature_celsius'];
  try {
    env.requests.forEach((request) => env.respond(request));
    await flush();
    const charts = env.root.querySelector('.history-grid');
    for (const id of ids) assert.equal(charts.querySelector(`#history-chart-${id}`), null);
    env.root.querySelector('#history-range-1h').click();
    env.requests.slice(14).forEach((request) => env.respond(request, 200, [{t: '2026-11-05T12:00:00Z', avg: 25, max: 50}]));
    await flush();
    for (const id of ids) assert.ok(charts.querySelector(`#history-chart-${id}`));
    assert.match(charts.querySelector('#history-chart-temperature_celsius').textContent, copyPattern('nodes.celsius', {value: '20'}));
    const rendered = [...charts.children];
    env.root.querySelector('#history-range-6h').click();
    rendered.forEach((section, i) => assert.equal(charts.children[i], section));
    env.requests.slice(28).forEach((request) => env.respond(request, request.path.includes('metric=temperature_celsius&') ? 503 : 200));
    await flush();
    assert.ok(charts.querySelector('#history-retry-temperature_celsius'));
    assert.match(charts.textContent, copyPattern('nodes.historyLoadError', {error: copy('errors.internal_error')}));
    for (const id of ids.filter((id) => id !== 'temperature_celsius')) assert.equal(charts.querySelector(`#history-chart-${id}`), null);
  } finally { env.cleanup(); }
});


test('range switch retains the exact old charts and focus while busy, then replaces with new data or errors', async () => {
  const env = setup();
  try {
    env.requests.forEach((request) => env.respond(request, 200, [{t: '2026-11-05T12:00:00Z', avg: 25, max: 50}]));
    await flush();
    const charts = env.root.querySelector('.history-grid');
    const rendered = [...charts.children];
    const cpu = charts.querySelector('#history-chart-cpu_percent');
    cpu.focus();
    env.root.querySelector('#history-range-1h').click();
    assert.equal(charts.getAttribute('aria-busy'), 'true');
    assert.deepEqual(charts.children, rendered);
    assert.equal(env.document.activeElement, cpu);
    env.respond(env.requests[14], 503);
    await flush();
    assert.deepEqual(charts.children, rendered, 'one response cannot clear the still-loading grid');
    env.requests.slice(15).forEach((request) => env.respond(request, 200, [{t: '2026-11-05T12:00:00Z', avg: 50, max: 60}]));
    await flush();
    assert.equal(charts.getAttribute('aria-busy'), 'false');
    assert.notEqual(charts.children[0], rendered[0]);
    assert.ok(charts.querySelector('#history-retry-cpu_percent'));
    assert.match(env.root.textContent, copyPattern('nodes.historyPartial'));
  } finally { env.cleanup(); }
});

test('short-span notice reuses the one-hour range control and persistence', async () => {
  const env = setup();
  const stored = [];
  globalThis.localStorage.setItem = (...args) => stored.push(args);
  try {
    const notice = env.root.querySelector('.history-short-span');
    assert.equal(notice.hidden, true);
    const sample = {t: '2026-11-05T12:00:00Z', avg: 25, max: 50};
    env.requests.forEach((request) => env.respond(request, 200, [sample]));
    await flush();
    assert.equal(notice.hidden, false);
    assert.equal(notice.querySelector('span').textContent, copy('nodes.historyShortSpan'));
    assert.equal(notice.querySelectorAll('button').length, 1);
    const action = notice.querySelector('button');
    assert.equal(action.textContent, copy('nodes.historyShowHour'));
    action.click();
    assert.equal(env.root.querySelector('#history-range-1h').getAttribute('aria-pressed'), 'true');
    assert.equal(env.document.activeElement, env.root.querySelector('#history-range-1h'));
    assert.deepEqual(stored, [['orbitbeacon.history.range', '1h']]);
    assert.ok(env.requests.slice(14).every((request) => request.path.endsWith('range=1h')));
    env.requests.slice(14).forEach((request) => env.respond(request, 200, [sample]));
    await flush();
    assert.equal(notice.hidden, true);
  } finally { env.cleanup(); }
});

test('short-span threshold uses valid data across metrics; empty and broad spans hide it', async () => {
  for (const [minutes, visible] of [[null, false], [71, true], [72, false], [120, false]]) {
    const env = setup();
    try {
      const start = Date.parse('2026-11-05T12:00:00Z');
      env.requests.forEach((request, index) => env.respond(request, 200, minutes === null ? [] : [
        {t: new Date(start + (index === 1 ? minutes * 60000 : 0)).toISOString(), avg: 25, max: 50},
        {t: 'invalid', avg: 25, max: 50}
      ]));
      await flush();
      assert.equal(env.root.querySelector('.history-short-span').hidden, !visible, String(minutes));
    } finally { env.cleanup(); }
  }
});
