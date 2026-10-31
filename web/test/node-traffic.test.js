import test from 'node:test';
import assert from 'node:assert/strict';
import {
  cycleDateLabel,
  fillTrafficDays,
  parseTrafficBytes,
  trafficPanel,
  trafficQuota
} from '../src/console/pages/node-traffic.js';
import {setNavigationSignal} from '../src/console/api.js';
import {elements, flush, installDOM} from './console-dom.js';

const emptyTraffic = () => ({
  billing_day: 1,
  quota_bytes: null,
  cycle: {
    start: '2026-10-01T00:00:00Z',
    end: '2026-11-01T00:00:00Z',
    rx_bytes: '0',
    tx_bytes: '0',
    total_bytes: '0'
  },
  daily: [],
  updated_at: null
});

function setupPanel() {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = (path, options) => new Promise((resolve) => requests.push({path, options, resolve}));
  const navigation = new AbortController();
  const page = new AbortController();
  setNavigationSignal(navigation.signal);
  const root = trafficPanel('node/id', page.signal);
  dom.body.append(root);
  const respond = (request, body, status = 200) => request.resolve(new Response(JSON.stringify(body), {status}));
  return {
    ...dom, navigation, page, root, requests, respond,
    cleanup() {
      page.abort();
      navigation.abort();
      setNavigationSignal(null);
      globalThis.fetch = originalFetch;
      dom.restore();
    }
  };
}

test('traffic byte parsing accepts only non-negative decimal strings', () => {
  assert.equal(parseTrafficBytes('0'), 0n);
  assert.equal(parseTrafficBytes('000123'), 123n);
  assert.equal(parseTrafficBytes('900719925474099312345678901234567890'), 900719925474099312345678901234567890n);
  for (const value of [null, undefined, 12, 12n, '', ' 12', '12\n', '+12', '-1', '1.5', '1e3', 'NaN', 'Infinity']) {
    assert.equal(parseTrafficBytes(value), 0n, String(value));
  }
});

test('traffic days fill a 30-day UTC window across year and leap-day boundaries', () => {
  const year = fillTrafficDays([
    {day: '2026-12-03', rx_bytes: '99', tx_bytes: '99'},
    {day: '2026-12-31', rx_bytes: '12', tx_bytes: '34'},
    {day: '2027-01-02', rx_bytes: '5', tx_bytes: '6'},
    {day: '2027-01-03', rx_bytes: '99', tx_bytes: '99'}
  ], new Date('2027-01-02T23:59:59-08:00'));
  // 给定的时间是 UTC 的 1 月 3 日，窗口按 UTC 日期计算。
  assert.equal(year.length, 30);
  assert.equal(year[0].day, '2026-12-05');
  assert.equal(year.at(-1).day, '2027-01-03');
  assert.deepEqual(year.find((item) => item.day === '2026-12-31'), {day: '2026-12-31', rx_bytes: 12n, tx_bytes: 34n});
  assert.deepEqual(year.find((item) => item.day === '2027-01-01'), {day: '2027-01-01', rx_bytes: 0n, tx_bytes: 0n});
  assert.deepEqual(year.at(-1), {day: '2027-01-03', rx_bytes: 99n, tx_bytes: 99n});
  assert.equal(year.some((item) => item.day === '2026-12-03'), false);

  const leap = fillTrafficDays([{day: '2028-02-29', rx_bytes: '1', tx_bytes: '2'}], new Date('2028-03-01T12:00:00Z'));
  assert.equal(leap[0].day, '2028-02-01');
  assert.equal(leap.at(-1).day, '2028-03-01');
  assert.deepEqual(leap.at(-2), {day: '2028-02-29', rx_bytes: 1n, tx_bytes: 2n});
});

test('cycle labels use inclusive UTC dates and handle invalid cycles', () => {
  assert.equal(cycleDateLabel({start: '2026-10-01T00:00:00Z', end: '2026-11-01T00:00:00Z'}),
    'Oct 1, 2026 – Oct 31, 2026 (UTC)');
  assert.equal(cycleDateLabel({start: '2028-02-01T00:00:00Z', end: '2028-03-01T00:00:00Z'}),
    'Feb 1, 2028 – Feb 29, 2028 (UTC)');
  assert.equal(cycleDateLabel({start: 'bad', end: '2026-11-01T00:00:00Z'}), 'Cycle dates unavailable');
  assert.equal(cycleDateLabel(null), 'Cycle dates unavailable');
});

test('quota percentage and states use exact bigint comparisons without non-finite results', () => {
  assert.deepEqual(trafficQuota('79', '100'), {percent: 79, state: 'normal'});
  assert.deepEqual(trafficQuota('80', '100'), {percent: 80, state: 'warning'});
  assert.deepEqual(trafficQuota(999n, 1000n), {percent: 99.9, state: 'warning'});
  assert.deepEqual(trafficQuota('100', '100'), {percent: 100, state: 'over'});
  assert.equal(trafficQuota('101', '100').state, 'over');
  assert.deepEqual(trafficQuota('1000000000000000000000000000000000000000', '1'), {percent: 900719925474099.1, state: 'over'});
  assert.deepEqual(trafficQuota('0', '0'), {percent: 0, state: 'normal'});
  assert.deepEqual(trafficQuota('1', '0'), {percent: 100, state: 'over'});

  const hugeQuota = '1' + '0'.repeat(400);
  const hugeUsed = '8' + '0'.repeat(399);
  const huge = trafficQuota(hugeUsed, hugeQuota);
  assert.deepEqual(huge, {percent: 80, state: 'warning'});
  assert.equal(Number.isFinite(huge.percent), true);
});

test('traffic panel renders the upgrade empty state only when traffic has never been reported', async () => {
  const env = setupPanel();
  try {
    assert.equal(env.requests.length, 1);
    assert.equal(env.requests[0].path, '/api/v1/nodes/node%2Fid/traffic?days=30');
    env.respond(env.requests[0], emptyTraffic());
    await flush();
    assert.match(env.root.textContent, /no traffic has been recorded yet/i);
    assert.match(env.root.textContent, /agent is upgraded to a version that reports traffic/i);
  } finally { env.cleanup(); }
});

test('traffic panel renders reported zero traffic as an accessible 30-day chart', async () => {
  const env = setupPanel();
  try {
    env.respond(env.requests[0], {...emptyTraffic(), updated_at: '2026-10-27T12:00:00Z'});
    await flush();
    assert.doesNotMatch(env.root.textContent, /no traffic has been recorded yet/i);
    const svg = elements(env.root).find((element) => element.tagName === 'svg' && element.getAttribute('role') === 'group');
    assert.ok(svg, 'daily chart exposes an SVG group');
    assert.match(svg.getAttribute('aria-label'), /traffic/i);
    const inspectable = elements(env.root).filter((element) => element.getAttribute('tabindex') === '0'
      && /received|receive|sent|send/i.test(element.getAttribute('aria-label') || ''));
    assert.equal(inspectable.length, 30);
    assert.ok(inspectable.every((element) => /UTC/.test(element.getAttribute('aria-label'))));
  } finally { env.cleanup(); }
});

test('traffic panel renders quota semantics and populated daily values', async () => {
  const env = setupPanel();
  try {
    env.respond(env.requests[0], {
      ...emptyTraffic(),
      quota_bytes: '1000',
      cycle: {...emptyTraffic().cycle, rx_bytes: '700', tx_bytes: '100', total_bytes: '800'},
      daily: [{day: new Date().toISOString().slice(0, 10), rx_bytes: '700', tx_bytes: '100'}],
      updated_at: '2026-10-27T12:00:00Z'
    });
    await flush();
    const meter = elements(env.root).find((element) => element.tagName === 'meter');
    assert.ok(meter);
    assert.match(meter.getAttribute('aria-label'), /800 B used of 1000 B \(80%\)/);
    assert.ok(elements(env.root).some((element) => element.classList.contains('traffic-quota') && element.classList.contains('warning')));
    assert.doesNotMatch(env.root.textContent, /NaN|Infinity/);
    const today = elements(env.root).find((element) => element.getAttribute('role') === 'img'
      && /Received 700 B, Sent 100 B, Total 800 B/.test(element.getAttribute('aria-label') || ''));
    assert.ok(today);
  } finally { env.cleanup(); }
});

test('traffic panel treats a zero quota as unlimited', async () => {
  const env = setupPanel();
  try {
    env.respond(env.requests[0], {
      ...emptyTraffic(),
      quota_bytes: '0',
      cycle: {...emptyTraffic().cycle, rx_bytes: '700', tx_bytes: '100', total_bytes: '800'},
      updated_at: '2026-10-27T12:00:00Z'
    });
    await flush();
    assert.ok(!elements(env.root).some((element) => element.tagName === 'meter' || element.classList.contains('traffic-quota')));
  } finally { env.cleanup(); }
});

test('traffic panel shows an inline error and retry starts a fresh request', async () => {
  const env = setupPanel();
  try {
    env.respond(env.requests[0], {message: 'Traffic service unavailable'}, 503);
    await flush();
    assert.ok(env.root.querySelector('.history-error'));
    const retry = env.root.querySelector('#traffic-retry');
    assert.ok(retry);
    retry.click();
    assert.equal(env.requests.length, 2);
    env.respond(env.requests[1], emptyTraffic());
    await flush();
    assert.match(env.root.textContent, /no traffic has been recorded yet/i);
    assert.doesNotMatch(env.root.textContent, /Could not load traffic/);
  } finally { env.cleanup(); }
});

test('traffic requests use navigation cancellation and aborted responses cannot repaint', async () => {
  const env = setupPanel();
  try {
    const request = env.requests[0];
    assert.equal(request.options.signal.aborted, false);
    env.navigation.abort(new DOMException('Navigated away', 'AbortError'));
    assert.equal(request.options.signal.aborted, true);
    env.respond(request, {...emptyTraffic(), updated_at: '2026-10-27T12:00:00Z'});
    await flush();
    assert.doesNotMatch(env.root.textContent, /Could not load traffic|Daily traffic|Received|Sent/);
  } finally { env.cleanup(); }
});

test('billing-triggered traffic reload cannot be overwritten by an older pending read', async () => {
  const env = setupPanel();
  try {
    const older = env.requests[0];
    const reload = env.root.reload();
    assert.equal(env.requests.length, 2);
    env.respond(env.requests[1], {...emptyTraffic(), quota_bytes: '1000',
      cycle: {...emptyTraffic().cycle, total_bytes: '800'}, updated_at: '2026-10-28T12:00:00Z'});
    await reload;
    assert.match(env.root.textContent, /800 B \/ 1000 B/);
    env.respond(older, emptyTraffic());
    await flush();
    assert.match(env.root.textContent, /800 B \/ 1000 B/);
    assert.doesNotMatch(env.root.textContent, /No traffic has been recorded yet/);
  } finally { env.cleanup(); }
});
