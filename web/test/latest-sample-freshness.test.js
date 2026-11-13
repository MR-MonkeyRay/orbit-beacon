import {reducedMotion} from '../src/shared/dom.js';
import test from 'node:test';
import assert from 'node:assert/strict';
import {copy, copyPattern, installDOM, elements} from './console-dom.js';

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
  const motion = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => true});
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
    Object.defineProperty(reducedMotion, 'matches', motion);
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
  const cpu = ui.row(copy('nodes.cpu')).querySelector('.sample-value');
  const uptime = ui.row(copy('nodes.uptime')).querySelector('.sample-value');
  const originalCPU = cpu.textContent;
  const originalUptime = uptime.textContent;
  assert.equal(originalCPU, '25%');
  assert.equal(originalUptime, copy('nodes.duration.hours', {hours: 1, minutes: 0}));
  assert.equal(ui.timers.size, 1);
  assert.equal([...ui.timers.values()][0].delay, 45001);
  ui.advance(45000);
  assert.equal(cpu.classList.contains('metric-stale'), false);
  ui.advance(1);
  assert.equal(cpu.classList.contains('metric-stale'), true);
  assert.match(ui.row(copy('nodes.cpu')).textContent, copyPattern('nodes.stale'));
  assert.equal(ui.row(copy('nodes.cpu')).querySelector('.sample-value'), cpu);
  assert.equal(cpu.textContent, originalCPU);
  assert.equal(uptime.textContent, originalUptime);
  assert.equal(ui.timers.size, 0);
});

test('latest sample honors original error and stale statuses without using earlier successful fields', async (t) => {
  const latest = snapshot();
  latest.metrics.metric_status.cpu_percent = 'error';
  latest.metrics.metric_status.uptime_seconds = 'stale';
  latest.metrics.metric_status.load5 = 'unsupported';
  const ui = await setup(t, latest);
  assert.equal(ui.row(copy('nodes.cpu')).querySelector('.sample-value').textContent, '—');
  assert.equal(ui.row(copy('nodes.uptime')).querySelector('.sample-value').textContent, copy('nodes.duration.hours', {hours: 1, minutes: 0}));
  assert.equal(ui.row(copy('nodes.uptime')).querySelector('.sample-value').classList.contains('metric-stale'), true);
  assert.doesNotMatch(ui.row(copy('nodes.loadMinute')).textContent, /2\.00/);
  const next = snapshot();
  next.metrics.cpu_percent = null;
  next.metrics.metric_status.cpu_percent = 'error';
  ui.view.snapshot(next);
  assert.equal(ui.row(copy('nodes.cpu')).querySelector('.sample-value').textContent, '—');
});

test('latest sample deadline uses original age and interval, not browser wall time or a later clock heartbeat', async (t) => {
  const latest = snapshot();
  latest.metrics.sources.cpu = source(10000, 20000);
  const ui = await setup(t, latest);
  ui.view.clock('2026-10-28T10:00:50.000Z');
  const cpu = ui.row(copy('nodes.cpu')).querySelector('.sample-value');
  assert.equal(cpu.classList.contains('metric-stale'), false);
  assert.equal([...ui.timers.values()][0].delay, 1);
  ui.advance(1);
  assert.equal(cpu.classList.contains('metric-stale'), true);
  assert.equal(cpu.textContent, '25%');
  ui.view.clock(received);
  assert.equal(cpu.classList.contains('metric-stale'), true);
  assert.equal(ui.timers.size, 0);
});

test('latest freshness stops while hidden and pagehidden, recomputes on restore, and rejects aborted callbacks', async (t) => {
  const ui = await setup(t);
  const cpu = ui.row(copy('nodes.cpu')).querySelector('.sample-value');
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
  assert.equal(ui.row(copy('nodes.cpu')).querySelector('.sample-value').classList.contains('metric-stale'), true);
  ui.page.abort();
  const before = ui.view.element.textContent;
  ui.view.clock('2026-10-28T10:02:00Z');
  ui.view.snapshot(snapshot({seq: 999}));
  old();
  assert.equal(ui.view.element.textContent, before);
  assert.equal(ui.timers.size, 0);
});

test('latest sample without a received time still renders and leaves the last report to the page header', async (t) => {
  const ui = await setup(t, snapshot({received_at: null}));
  assert.equal(ui.view.element.querySelector('.last-report'), null);
  assert.equal(ui.row(copy('nodes.cpu')).querySelector('.sample-value').textContent, '—');
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
  assert.equal(ui.row(copy('nodes.disk')).querySelector('.sample-value').textContent, '25%');
  assert.equal(ui.row(copy('nodes.network')).querySelector('.sample-value').textContent, '—');
  ui.advance(45001);
  assert.equal(ui.row(copy('nodes.cpu')).querySelector('.sample-value').classList.contains('metric-stale'), true);
  assert.equal(ui.row(copy('nodes.disk')).querySelector('.sample-value').classList.contains('metric-stale'), false);
  assert.equal(ui.timers.size, 1);
  ui.advance(134999);
  assert.equal(ui.row(copy('nodes.disk')).querySelector('.sample-value').classList.contains('metric-stale'), false);
  ui.advance(1);
  assert.equal(ui.row(copy('nodes.disk')).querySelector('.sample-value').classList.contains('metric-stale'), true);
  assert.equal(ui.row(copy('nodes.disk')).querySelector('.sample-value').textContent, '25%');
});

test('single disk folds capacity and inode percent into the tile', async (t) => {
  const latest = snapshot();
  latest.metrics.disks = [{path: '/', total_bytes: 4096, used_bytes: 1024, inodes_total: 100, inodes_used: 25}];
  latest.metrics.metric_status['disk:/'] = 'ok';
  latest.metrics.sources['disk:/'] = source(0, 60000);
  const ui = await setup(t, latest);
  const value = () => ui.row(copy('nodes.disk')).querySelector('.sample-value').textContent;
  assert.equal(value(), '25%');
  assert.match(ui.row(copy('nodes.disk')).querySelector('.sample-detail').textContent, /Inodes 25%/);
  latest.metrics.disks[0].inodes_total = null;
  latest.metrics.disks[0].inodes_used = null;
  ui.view.snapshot(latest);
  assert.equal(value(), '25%');
});

test('optional host sample fields keep their place and show missing values as dashes', async (t) => {
  const latest = snapshot();
  const ui = await setup(t, latest);
  const keys = ['nodes.diskIO', 'nodes.pressure', 'nodes.temperature'];
  for (const key of keys) assert.match(ui.row(copy(key)).textContent, /—/);
  Object.assign(latest.metrics, {disk_io: {read_bytes_per_sec: 1024, write_bytes_per_sec: 2048, busy_percent: 25},
    pressure: {cpu: 1, memory: null, io: 3}, temperature_celsius: 48.5});
  ui.view.snapshot(latest);
  assert.deepEqual(ui.row(copy('nodes.diskIO')).querySelectorAll('.sample-value').map((element) => element.textContent), ['1 KiB/s', '2 KiB/s', '25%']);
  assert.match(ui.row(copy('nodes.pressure')).textContent, copyPattern('nodes.pressureSummary', {cpu: '1%', memory: '—', io: '3%'}));
  assert.match(ui.row(copy('nodes.temperature')).textContent, copyPattern('nodes.celsius', {value: '48.5'}));
  Object.assign(latest.metrics, {pressure: {cpu: null, memory: null, io: null}});
  ui.view.snapshot(latest);
  assert.match(ui.row(copy('nodes.pressure')).textContent, /—/);
  Object.assign(latest.metrics, {disk_io: null, pressure: null, temperature_celsius: null});
  ui.view.snapshot(latest);
  for (const key of keys) assert.match(ui.row(copy(key)).textContent, /—/);
});

test('tiles choose highest disk, sum interfaces, and keep keyed table nodes across updates', async (t) => {
  const latest = snapshot();
  latest.metrics.disks = [
    {path: '/', total_bytes: 4096, used_bytes: 1024, inodes_total: 100, inodes_used: 25},
    {path: '/data', total_bytes: 1024, used_bytes: 900, inodes_total: 100, inodes_used: 90}
  ];
  latest.metrics.networks = [
    {name: 'eth0', rx_bytes_per_sec: 1024, tx_bytes_per_sec: 512},
    {name: 'eth1', rx_bytes_per_sec: 2048, tx_bytes_per_sec: 256}
  ];
  for (const key of ['disk:/', 'disk:/data', 'network:eth0', 'network:eth1']) {
    latest.metrics.metric_status[key] = 'ok'; latest.metrics.sources[key] = source();
  }
  const ui = await setup(t, latest);
  const primary = ui.view.element.querySelector('.sample-primary');
  assert.equal(primary.children.length, 4);
  assert.equal(primary.querySelectorAll('.usage-meter').length, 3, 'load has no CPU-count denominator');
  assert.equal(ui.row(copy('nodes.cpu')).querySelector('.sample-value').textContent, '25%');
  assert.equal(ui.row(copy('nodes.memory')).querySelector('.sample-value').textContent, '75%');
  assert.equal(ui.row(copy('nodes.memory')).querySelector('.sample-detail').textContent, '3 KiB / 4 KiB');
  const diskRow = ui.row(copy('nodes.disk'));
  assert.equal(diskRow.querySelector('.sample-value').textContent, '87.9%');
  assert.match(diskRow.querySelector('.sample-detail').textContent, /^\/data/);
  assert.deepEqual(ui.row(copy('nodes.network')).querySelectorAll('.sample-value').map((value) => value.textContent), ['3 KiB/s', '768 B/s']);
  const disksTable = ui.view.element.querySelector('.sample-disks');
  const networksTable = ui.view.element.querySelector('.sample-networks');
  assert.equal(disksTable.hidden, false);
  assert.equal(networksTable.hidden, false);
  assert.deepEqual(disksTable.querySelectorAll('tbody')[0].children[0].querySelectorAll('.sample-value').map((value) => value.textContent), ['1 KiB / 4 KiB', '25%', '25%']);
  const originalNodes = elements(primary);
  const originalDiskRow = disksTable.querySelector('tbody').children[0];
  const originalNetworkRow = networksTable.querySelector('tbody').children[0];
  latest.metrics.cpu_percent = 95;
  latest.metrics.disks[0].used_bytes = 4096;
  latest.metrics.networks[0].rx_bytes_per_sec = 3072;
  ui.view.snapshot(latest);
  assert.deepEqual(elements(primary), originalNodes);
  assert.equal(disksTable.querySelector('tbody').children[0], originalDiskRow);
  assert.equal(networksTable.querySelector('tbody').children[0], originalNetworkRow);
  assert.equal(diskRow.querySelector('.sample-value').textContent, '100%');
  assert.match(diskRow.querySelector('.sample-detail').textContent, /^\//);
  assert.equal(ui.row(copy('nodes.cpu')).querySelector('.usage-meter').classList.contains('usage-bad'), true);
  assert.equal(ui.row(copy('nodes.network')).querySelector('.sample-value').textContent, '5 KiB/s');
  latest.metrics.disks[0].total_bytes = 8192;
  ui.view.snapshot(latest);
  assert.equal(originalDiskRow.querySelector('.sample-value').textContent, '4 KiB / 8 KiB', 'unchanged used bytes still update total');
  latest.metrics.disks = latest.metrics.disks.slice(0, 1);
  latest.metrics.networks = latest.metrics.networks.slice(0, 1);
  ui.view.snapshot(latest);
  assert.equal(disksTable.hidden, true);
  assert.equal(networksTable.hidden, true);
  assert.equal(ui.row(copy('nodes.network')).querySelector('.sample-detail').textContent, 'eth0');
  latest.metrics = {};
  ui.view.snapshot(latest);
  assert.equal(primary.querySelectorAll('.sample-value').every((value) => value.textContent === '—'), true);
  assert.equal(ui.row(copy('nodes.network')).querySelectorAll('.sample-value').every((value) => value.textContent === '—'), true);
});

test('usage values and bars share a tween and detach finishes it', async (t) => {
  const ui = await setup(t);
  const motion = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => false});
  const originals = new Map(['requestAnimationFrame', 'cancelAnimationFrame'].map((key) => [key, globalThis[key]]));
  const frames = new Map();
  let id = 0;
  globalThis.requestAnimationFrame = (callback) => { frames.set(++id, callback); return id; };
  globalThis.cancelAnimationFrame = (id) => frames.delete(id);
  t.after(() => { Object.defineProperty(reducedMotion, 'matches', motion); for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; } });
  const cpu = ui.row(copy('nodes.cpu'));
  const meter = cpu.querySelector('.usage-meter');
  const value = cpu.querySelector('.sample-value');
  const next = snapshot(); next.metrics.cpu_percent = 95;
  ui.view.snapshot(next);
  assert.equal(value.textContent, '25%');
  assert.equal(frames.size, 1);
  const callbacks = [...frames.values()]; frames.clear(); callbacks.forEach((callback) => callback(125));
  assert.equal(value.textContent, '60%');
  assert.equal(meter.querySelector('i').style.width, '60%');
  ui.view.detach();
  assert.equal(value.textContent, '95%');
  assert.equal(meter.querySelector('i').style.width, '95%');
  assert.equal(frames.size, 0);
});

test('highest disk errors stay unavailable and one missing network direction does not invent a total', async (t) => {
  const latest = snapshot();
  latest.metrics.disks = [{path: '/', total_bytes: 100, used_bytes: 50}, {path: '/data', total_bytes: 100, used_bytes: 95}];
  latest.metrics.networks = [{name: 'eth0', rx_bytes_per_sec: 100, tx_bytes_per_sec: 50}, {name: 'eth1', rx_bytes_per_sec: null, tx_bytes_per_sec: 50}];
  for (const key of ['disk:/', 'disk:/data', 'network:eth0', 'network:eth1']) {
    latest.metrics.metric_status[key] = 'ok'; latest.metrics.sources[key] = source();
  }
  latest.metrics.metric_status['disk:/data'] = 'error';
  const ui = await setup(t, latest);
  assert.equal(ui.row(copy('nodes.disk')).querySelector('.sample-value').textContent, '—');
  assert.deepEqual(ui.row(copy('nodes.network')).querySelectorAll('.sample-value').map((value) => value.textContent), ['—', '100 B/s']);
  const sampled = ui.row(copy('nodes.sampled')).querySelector('time');
  latest.seq = 5; latest.sampled_at = '2026-10-28T10:00:10.000Z';
  ui.view.snapshot(latest);
  assert.equal(ui.row(copy('nodes.sampled')).querySelector('time'), sampled);
  assert.equal(sampled.getAttribute('datetime'), latest.sampled_at);
  assert.match(sampled.title, copyPattern('nodes.sequence', {value: 5}));
  latest.metrics.metric_status['network:eth1'] = 'error';
  ui.view.snapshot(latest);
  assert.deepEqual(ui.row(copy('nodes.network')).querySelectorAll('.sample-value').map((value) => value.textContent), ['—', '—']);
});
