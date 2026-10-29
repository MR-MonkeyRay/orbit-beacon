import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {flush, TestElement} from './console-dom.js';
import {frame, liveHarness, response, stream} from './live-harness.js';

const entry = new URL('../src/public/main.js', import.meta.url);
const source = (await readFile(entry, 'utf8')).replace("import './public.css';", '')
  .replace(/from '(\.\.?\/[^']+)'/g, (_match, path) => `from '${new URL(path, entry).href}'`);
let run = 0;
const payload = (name = 'edge', extra = {}) => ({
  nodes: [{name, region: '', provider: '', status: 'online', last_seen: '2026-10-27T12:34:56Z',
    cpu_percent: 25, memory_percent: null, load1: null, uptime_seconds: 3600,
    metric_status: {cpu_percent: 'ok', memory_percent: 'unavailable', load1: 'unavailable', uptime_seconds: 'ok'}}],
  generated_at: '2026-10-27T12:34:56Z', has_more: false, next_offset: null, ...extra
});
const snapshot = (data = payload(), interval = 10) => frame('snapshot', {data, update_interval_seconds: interval});
class PublicElement extends TestElement {
  constructor(tag) { super(tag); this.style.setProperty = (key, value) => { this.style[key] = value; }; }
  get lastChild() { return this.children.at(-1); }
  getBoundingClientRect() { return {}; }
}
async function setup(t, respond) {
  const env = liveHarness(t, respond);
  env.document.createElement = (tag) => new PublicElement(tag);
  env.document.createElementNS = (_namespace, tag) => new PublicElement(tag);
  const ids = ['nodes', 'empty', 'empty-text', 'retry', 'headline', 'summary', 'updated', 'announcement', 'refresh', 'countdown', 'filters', 'pagination', 'previous', 'next', 'page', 'constellation', 'theme'];
  const elements = Object.fromEntries(ids.map((id) => {
    const element = new PublicElement('div'); element.id = id; env.body.append(element); return [id, element];
  }));
  await import(`data:text/javascript;base64,${Buffer.from(`${source}\n// run ${++run}`).toString('base64')}`);
  await flush();
  return {...env, elements};
}

test('public WS snapshots render values; heartbeats and ordinary snapshots do not repeat ARIA announcements', async (t) => {
  const ui = await setup(t);
  const socket = ui.sockets[0]; socket.open(); socket.message(snapshot(payload('first')));
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'first');
  assert.match(ui.elements.updated.textContent, /更新于 .* · 自动更新/);
  assert.equal(ui.elements.countdown.classList.contains('running'), false);
  const announced = ui.elements.announcement.textContent;
  const updated = ui.elements.updated.textContent;
  socket.message(frame('heartbeat', {server_time: '2026-10-28T00:00:10Z'}));
  assert.equal(ui.elements.updated.textContent, updated);
  assert.equal(ui.elements.announcement.textContent, announced);
  socket.message(snapshot(payload('second')));
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'second');
  assert.equal(ui.elements.announcement.textContent, announced);
  assert.equal(ui.requests.length, 0);
});

test('public fallback is transparent and HTTP countdown uses the server interval without heartbeat announcements', async (t) => {
  const ui = await setup(t, (request) => request.url.includes('/stream?') ? response({}, {status: 503}) : response(snapshot(payload('polled'), 60)));
  ui.sockets[0].fail(); await flush();
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'polled');
  assert.equal(ui.elements.countdown.classList.contains('running'), true);
  assert.match(ui.elements.updated.textContent, /60 秒后刷新/);
  const announced = ui.elements.announcement.textContent;
  ui.shiftWall(3600000);
  await ui.advance(30000);
  assert.equal(ui.requests.length, 2);
  assert.equal(ui.elements.announcement.textContent, announced);
  assert.match(ui.elements.updated.textContent, /30 秒后刷新/);
  await ui.advance(30000);
  assert.equal(ui.requests.length, 3);
  assert.doesNotMatch(ui.elements.updated.textContent, /WS|SSE|HTTP/);
});

test('public broken channel retains values and online presence until the next validated withdrawal snapshot', async (t) => {
  const sse = stream(); const ui = await setup(t, () => sse.response);
  ui.sockets[0].open(); ui.sockets[0].message(snapshot(payload('retained')));
  const card = ui.elements.nodes.children[0]; const cpu = card.querySelector('.gauge-value');
  ui.sockets[0].end(); await flush();
  assert.equal(ui.elements.nodes.children[0], card);
  assert.equal(cpu.textContent, '25.0%'); assert.equal(card.dataset.status, 'online');
  assert.equal(card.querySelectorAll('dd')[1].textContent, '1小时 0分钟');
  assert.equal(card.querySelector('.gauge').classList.contains('metric-stale'), true);
  assert.match(ui.elements.summary.textContent, /连接中断，正在重连；当前显示最后一次数据。/);
  sse.frame(snapshot(payload('', {nodes: []}))); await flush();
  assert.equal(ui.elements.nodes.children.length, 0);
  assert.equal(ui.elements['empty-text'].textContent, '暂无公开的节点。');
  assert.equal(ui.elements.empty.hidden, false);
});

test('public invalid snapshot cannot clear an accepted collection or change its values', async (t) => {
  const sse = stream(); const ui = await setup(t, () => sse.response);
  ui.sockets[0].open(); ui.sockets[0].message(snapshot(payload('accepted')));
  ui.sockets[0].message(snapshot(payload('invalid', {has_more: true, next_offset: 0}))); await flush();
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'accepted');
  assert.equal(ui.elements.nodes.querySelector('.gauge-value').textContent, '25.0%');
  assert.equal(ui.elements.nodes.children[0].dataset.status, 'online');
});

test('public initial failure is a real loading error, never a fabricated empty collection', async (t) => {
  const ui = await setup(t, () => { throw new Error('offline'); });
  ui.sockets[0].fail(); await flush();
  assert.equal(ui.elements.nodes.children.length, 0);
  assert.equal(ui.elements.empty.hidden, false);
  assert.equal(ui.elements['empty-text'].textContent, '加载失败，请检查网络后重试。');
  assert.doesNotMatch(ui.elements.headline.textContent, /暂无公开/);
});

test('public refresh ignores late callbacks and starts WS again, not a second subscription', async (t) => {
  const ui = await setup(t);
  ui.sockets[0].open(); ui.sockets[0].message(snapshot(payload('first')));
  ui.elements.refresh.click();
  assert.equal(ui.sockets[0].closed, true); assert.equal(ui.sockets.length, 2);
  ui.sockets[0].message(snapshot(payload('obsolete')));
  ui.sockets[1].open(); ui.sockets[1].message(snapshot(payload('current')));
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'current');
});

test('public page changes close the old scope and validate the new pagination cursor', async (t) => {
  const ui = await setup(t, () => new Promise(() => {}));
  ui.sockets[0].open(); ui.sockets[0].message(snapshot(payload('page one', {has_more: true, next_offset: 50})));
  ui.elements.next.click();
  assert.equal(ui.sockets[0].closed, true);
  assert.match(ui.sockets[1].url, /offset=50/);
  ui.sockets[0].message(snapshot(payload('obsolete')));
  ui.sockets[1].open(); ui.sockets[1].message(snapshot(payload('bad cursor', {has_more: true, next_offset: 50})));
  await flush();
  assert.equal(ui.elements.nodes.querySelector('h2'), null);
  assert.equal(ui.elements['empty-text'].textContent, '加载失败，请检查网络后重试。');
});

test('public filters survive valid patches and keep filtering the current page', async (t) => {
  const ui = await setup(t); ui.sockets[0].open();
  const data = payload('online'); const offline = {...data.nodes[0], name: 'offline', status: 'offline'}; data.nodes.push(offline);
  ui.sockets[0].message(snapshot(data));
  ui.elements.filters.children[2].click();
  assert.equal(ui.elements.nodes.children[0].hidden, true); assert.equal(ui.elements.nodes.children[1].hidden, false);
  ui.sockets[0].message(snapshot(data));
  assert.equal(ui.elements.filters.children[2].getAttribute('aria-pressed'), 'true');
  assert.equal(ui.elements.nodes.children[0].hidden, true);
});

test('public hide and BFCache preserve stale values and resume with a fresh WS scope', async (t) => {
  const ui = await setup(t); ui.sockets[0].open(); ui.sockets[0].message(snapshot(payload('visible')));
  ui.document.hidden = true; ui.document.dispatchEvent(new Event('visibilitychange'));
  ui.sockets[0].message(snapshot(payload('late')));
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'visible'); assert.equal(ui.timers.size, 0);
  ui.document.hidden = false; ui.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(ui.sockets.length, 2);
  ui.window.dispatchEvent(new Event('pagehide')); assert.equal(ui.timers.size, 0);
  ui.window.dispatchEvent(Object.assign(new Event('pageshow'), {persisted: true}));
  assert.equal(ui.sockets.length, 3);
  ui.sockets[2].open(); ui.sockets[2].message(snapshot(payload('restored')));
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'restored');
});

test('public stale/offline cards retain last values and null last_seen hides the entire time line', async (t) => {
  const ui = await setup(t); const socket = ui.sockets[0]; socket.open();
  const data = payload('offline'); data.nodes[0].status = 'offline';
  data.nodes[0].metric_status.cpu_percent = 'stale'; data.nodes[0].metric_status.uptime_seconds = 'stale';
  socket.message(snapshot(data));
  const card = ui.elements.nodes.children[0];
  assert.equal(card.hidden, false); assert.equal(card.querySelector('.gauge-value').textContent, '25.0%');
  assert.equal(card.querySelectorAll('dd')[1].textContent, '1小时 0分钟');
  assert.equal(card.querySelector('.gauge').classList.contains('metric-stale'), true);
  assert.match(card.querySelector('.last-report').textContent, /最后数据 ·/);
  assert.equal(card.querySelector('time').getAttribute('datetime'), data.nodes[0].last_seen);
  assert.equal(card.querySelectorAll('.gauge-value')[1].textContent, '—');
  data.nodes[0].last_seen = null; socket.message(snapshot(data));
  assert.equal(card.querySelector('.last-report').hidden, true);
});

test('public-page disable clears rendered data and timers before replacing the root URL', async (t) => {
  const ui = await setup(t);
  const redirects = [];
  location.replace = (target) => {
    assert.equal(ui.elements.nodes.children.length, 0);
    assert.equal(ui.elements.summary.textContent, '—');
    assert.equal(ui.sockets[0].closed, true);
    assert.equal(ui.timers.size, 0);
    redirects.push(target);
  };
  ui.sockets[0].open(); ui.sockets[0].message(snapshot(payload('withdrawn')));
  ui.sockets[0].message(frame('error', {code: 'public_page_disabled'}));
  await flush(); await ui.advance(600000);
  assert.deepEqual(redirects, ['/']);
  assert.equal(ui.requests.length, 0);
});
