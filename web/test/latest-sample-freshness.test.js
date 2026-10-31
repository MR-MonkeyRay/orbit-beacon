import test from 'node:test';
import assert from 'node:assert/strict';
import {installDOM, elements} from './console-dom.js';

const received = '2026-10-28T10:00:00.000Z';
const source = (age = 0, interval = 10000) => ({age_ms: age, source_interval_ms: interval});
const snapshot = (extra = {}) => ({
  received_at: received, sampled_at: received, server_time: received, seq: 4,
  system: {agent_version: 'test-agent'},
  metrics: {cpu_percent: 25, memory_total_bytes: 4096, memory_available_bytes: 1024,
    load1: 1, load5: 2, load15: 3, uptime_seconds: 3600, disks: [], networks: [],
    metric_status: {cpu_percent: 'ok', memory_total_bytes: 'ok', memory_available_bytes: 'ok',
      load1: 'ok', load5: 'ok', load15: 'ok', uptime_seconds: 'ok'},
    sources: {cpu: source(), memory: source(), load: source(), uptime: source()}}, ...extra
});

async function setup(t, latest = snapshot()) {
  const dom = installDOM();
  const originals = new Map(['window', 'performance', 'setTimeout', 'clearTimeout'].map((key) => [key, globalThis[key]]));
  const window = globalThis.window = new EventTarget();
  let now = 0;
  let id = 0;
  const timers = new Map();
  globalThis.performance = {now: () => now};
  globalThis.setTimeout = (callback, delay) => { timers.set(++id, {callback, at: now + delay, delay}); return id; };
  globalThis.clearTimeout = (key) => timers.delete(key);
  const page = new AbortController();
  t.after(() => {
    page.abort();
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    dom.restore();
  });
  const {latestSamplePanel} = await import('../src/console/pages/latest-sample.js');
  const view = latestSamplePanel({agent_version: 'fallback'}, latest, {signal: page.signal});
  dom.body.append(view.element);
  const advance = (milliseconds) => {
    now += milliseconds;
    for (const [key, timer] of [...timers]) if (timer.at <= now) { timers.delete(key); timer.callback(); }
  };
  const row = (label) => view.element.querySelectorAll('dt').find((entry) => entry.textContent === label)?.parentElement;
  return {...dom, window, page, view, timers, row, advance};
}

test('latest sample expires just after the 45-second boundary without changing numeric or uptime nodes', async (t) => {
  const ui = await setup(t);
  const cpu = ui.row('CPU').querySelector('.sample-value');
  const uptime = ui.row('Uptime').querySelector('.sample-value');
  const originalCPU = cpu.textContent;
  const originalUptime = uptime.textContent;
  assert.equal(originalCPU, '25%');
  assert.equal(originalUptime, '1h 0m');
  assert.equal(ui.timers.size, 1);
  assert.equal([...ui.timers.values()][0].delay, 45001);
  ui.advance(45000);
  assert.equal(cpu.classList.contains('metric-stale'), false);
  ui.advance(1);
  assert.equal(cpu.classList.contains('metric-stale'), true);
  assert.match(ui.row('CPU').textContent, /Stale/);
  assert.equal(ui.row('CPU').querySelector('.sample-value'), cpu);
  assert.equal(cpu.textContent, originalCPU);
  assert.equal(uptime.textContent, originalUptime);
  const last = ui.view.element.querySelector('.last-report');
  assert.equal(last.hidden, false);
  assert.match(last.textContent, /Last report Oct 28, 2026/);
  assert.equal(last.querySelector('time').getAttribute('datetime'), received);
  assert.equal(ui.timers.size, 0);
});

test('latest sample honors original error and stale statuses without using earlier successful fields', async (t) => {
  const latest = snapshot();
  latest.metrics.metric_status.cpu_percent = 'error';
  latest.metrics.metric_status.uptime_seconds = 'stale';
  latest.metrics.metric_status.load5 = 'unsupported';
  const ui = await setup(t, latest);
  assert.equal(ui.row('CPU').querySelector('.sample-value').textContent, '—');
  assert.equal(ui.row('Uptime').querySelector('.sample-value').textContent, '1h 0m');
  assert.equal(ui.row('Uptime').querySelector('.sample-value').classList.contains('metric-stale'), true);
  assert.doesNotMatch(ui.row('Load (1 / 5 / 15 min)').textContent, /2\.00/);
  const next = snapshot();
  next.metrics.cpu_percent = null;
  next.metrics.metric_status.cpu_percent = 'error';
  ui.view.snapshot(next);
  assert.equal(ui.row('CPU').querySelector('.sample-value').textContent, '—');
});

test('latest sample deadline uses original age and interval, not browser wall time or a later clock heartbeat', async (t) => {
  const latest = snapshot();
  latest.metrics.sources.cpu = source(10000, 20000);
  const ui = await setup(t, latest);
  ui.view.clock('2026-10-28T10:00:50.000Z');
  const cpu = ui.row('CPU').querySelector('.sample-value');
  assert.equal(cpu.classList.contains('metric-stale'), false);
  assert.equal([...ui.timers.values()][0].delay, 1);
  ui.advance(1);
  assert.equal(cpu.classList.contains('metric-stale'), true);
  assert.equal(cpu.textContent, '25%');
  assert.equal(ui.view.element.querySelector('.last-report').querySelector('time').getAttribute('datetime'), received);
  ui.view.clock(received);
  assert.equal(cpu.classList.contains('metric-stale'), true);
  assert.equal(ui.timers.size, 0);
});

test('latest freshness stops while hidden and pagehidden, recomputes on restore, and rejects aborted callbacks', async (t) => {
  const ui = await setup(t);
  const cpu = ui.row('CPU').querySelector('.sample-value');
  const old = [...ui.timers.values()][0].callback;
  ui.document.hidden = true;
  ui.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(ui.timers.size, 0);
  ui.advance(46000);
  old();
  assert.equal(cpu.classList.contains('metric-stale'), false);
  ui.document.hidden = false;
  ui.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(cpu.classList.contains('metric-stale'), true);
  ui.view.snapshot(snapshot({received_at: '2026-10-28T10:00:46.000Z', server_time: '2026-10-28T10:00:46.000Z'}));
  assert.equal(ui.timers.size, 1);
  ui.window.dispatchEvent(new Event('pagehide'));
  assert.equal(ui.timers.size, 0);
  ui.advance(46000);
  ui.window.dispatchEvent(Object.assign(new Event('pageshow'), {persisted: true}));
  assert.equal(ui.row('CPU').querySelector('.sample-value').classList.contains('metric-stale'), true);
  ui.page.abort();
  const before = ui.view.element.textContent;
  ui.view.clock('2026-10-28T10:02:00Z');
  ui.view.snapshot(snapshot({seq: 999}));
  old();
  assert.equal(ui.view.element.textContent, before);
  assert.equal(ui.timers.size, 0);
});

test('latest sample no-report and missing received time do not invent a last-report timestamp', async (t) => {
  const ui = await setup(t, snapshot({received_at: null}));
  assert.equal(ui.view.element.querySelector('.last-report').hidden, true);
  assert.equal(ui.row('CPU').querySelector('.sample-value').textContent, '—');
  ui.view.snapshot(null);
  assert.equal(ui.view.element.querySelectorAll('dl').length, 0);
  assert.equal(ui.view.element.querySelectorAll('time').length, 0);
  assert.equal(elements(ui.view.element).some((element) => element.classList.contains('sample-value')), false);
  assert.equal(ui.timers.size, 0);
});

test('latest per-series status masks nonnull failures and respects the disk source cadence', async (t) => {
  const latest = snapshot();
  latest.metrics.disks = [{path: '/', total_bytes: 4096, used_bytes: 1024}];
  latest.metrics.networks = [{name: 'eth0', rx_bytes_per_sec: 1536, tx_bytes_per_sec: 1024}];
  latest.metrics.metric_status['disk:/'] = 'ok';
  latest.metrics.metric_status['network:eth0'] = 'permission_denied';
  latest.metrics.sources['disk:/'] = source(0, 60000);
  latest.metrics.sources['network:eth0'] = source();
  const ui = await setup(t, latest);
  assert.equal(ui.row('Disk').querySelector('.sample-value').textContent, '/: 1 KiB / 4 KiB (25%)');
  assert.equal(ui.row('Network').querySelector('.sample-value').textContent, '—');
  ui.advance(45001);
  assert.equal(ui.row('CPU').querySelector('.sample-value').classList.contains('metric-stale'), true);
  assert.equal(ui.row('Disk').querySelector('.sample-value').classList.contains('metric-stale'), false);
  assert.equal(ui.timers.size, 1);
  ui.advance(134999);
  assert.equal(ui.row('Disk').querySelector('.sample-value').classList.contains('metric-stale'), false);
  ui.advance(1);
  assert.equal(ui.row('Disk').querySelector('.sample-value').classList.contains('metric-stale'), true);
  assert.equal(ui.row('Disk').querySelector('.sample-value').textContent, '/: 1 KiB / 4 KiB (25%)');
});
