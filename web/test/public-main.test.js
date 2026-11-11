import test from 'node:test';
import assert from 'node:assert/strict';
import {reducedMotion} from '../src/shared/dom.js';
import {copy, flush, TestElement} from './console-dom.js';
import {frame, liveHarness, response, stream} from './live-harness.js';

const loadRenderers = async () => ({
  ...await import('../themes/default/cards.js'),
  ...await import('../themes/default/constellation.js'),
  ...await import('../themes/default/announcements.js')
});
const payload = (name = 'edge', extra = {}) => ({
  announcements: [],
  nodes: [{name, region: '', provider: '', status: 'online', last_seen: '2026-10-27T12:34:56Z',
    availability: {start_day: '2026-09-28', percent: null, days: Array(30).fill(null)},
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
async function setup(t, respond, customize) {
  const env = liveHarness(t, respond);
  globalThis.localStorage.getItem = (key) => key === 'orbitbeacon-locale' ? 'en' : null;
  env.document.createElement = (tag) => new PublicElement(tag);
  env.document.createElementNS = (_namespace, tag) => new PublicElement(tag);
  const ids = ['announcements', 'nodes', 'empty', 'empty-text', 'retry', 'headline', 'summary', 'updated', 'announcement', 'refresh', 'countdown', 'filters', 'pagination', 'previous', 'next', 'page', 'constellation', 'theme', 'top-actions', 'locale-error'];
  const elements = Object.fromEntries(ids.map((id) => {
    const element = new PublicElement('div'); element.id = id; env.body.append(element); return [id, element];
  }));
  const {mountPublicPage} = await import('../src/public/page.js');
  const renderers = await loadRenderers();
  mountPublicPage(customize ? customize(renderers) : renderers);
  await flush();
  return {...env, elements};
}

test('public WS snapshots render values; heartbeats and ordinary snapshots do not repeat ARIA announcements', async (t) => {
  const ui = await setup(t);
  const socket = ui.sockets[0]; socket.open(); socket.message(snapshot(payload('first')));
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'first');
  assert.notEqual(ui.elements.updated.querySelector('.update-time').textContent, '');
  assert.notEqual(ui.elements.updated.querySelector('.update-mode').textContent, '');
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

test('public page turn keeps the announcement element until the next page replaces it', async (t) => {
  const ui = await setup(t);
  const notice = {level: 'warning', title: 'maintenance', body: 'tonight', starts_at: '2026-10-27T00:00:00Z', ends_at: null};
  ui.sockets[0].open(); ui.sockets[0].message(snapshot(payload('one', {announcements: [notice], has_more: true, next_offset: 50})));
  const article = ui.elements.announcements.children[0];
  ui.elements.next.click();
  assert.equal(ui.elements.announcements.children[0], article);
  assert.ok(document.title.startsWith(copy('public.page', {page: 2})));
  ui.sockets[1].open(); ui.sockets[1].message(snapshot(payload('two', {announcements: [notice]})));
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'two');
  assert.equal(ui.elements.announcements.children[0], article);
  ui.elements.previous.click();
  ui.sockets[2].open(); ui.sockets[2].message(snapshot(payload('one', {has_more: true, next_offset: 50})));
  assert.equal(ui.elements.announcements.children.length, 0);
});

test('public fallback is transparent and HTTP countdown uses the server interval without heartbeat announcements', async (t) => {
  const ui = await setup(t, (request) => request.url.includes('/stream?') ? response({}, {status: 503}) : response(snapshot(payload('polled'), 60)));
  ui.sockets[0].fail(); await flush();
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'polled');
  assert.equal(ui.elements.countdown.classList.contains('running'), true);
  assert.match(ui.elements.updated.querySelector('.update-mode').textContent, /60/);
  const announced = ui.elements.announcement.textContent;
  ui.shiftWall(3600000);
  await ui.advance(30000);
  assert.equal(ui.requests.length, 2);
  assert.equal(ui.elements.announcement.textContent, announced);
  assert.match(ui.elements.updated.querySelector('.update-mode').textContent, /30/);
  await ui.advance(30000);
  assert.equal(ui.requests.length, 3);
  assert.doesNotMatch(ui.elements.updated.textContent, /WS|SSE|HTTP/);
});

test('public broken channel retains values and online presence until the next validated withdrawal snapshot', async (t) => {
  const sse = stream(); const ui = await setup(t, () => sse.response);
  ui.sockets[0].open(); ui.sockets[0].message(snapshot(payload('retained')));
  const card = ui.elements.nodes.children[0]; const cpu = card.querySelector('.gauge-value');
  const uptime = card.querySelectorAll('dd')[1].textContent;
  ui.sockets[0].end(); await flush();
  assert.equal(ui.elements.nodes.children[0], card);
  assert.equal(cpu.textContent, '25%'); assert.equal(card.dataset.status, 'online');
  assert.equal(card.querySelectorAll('dd')[1].textContent, uptime);
  assert.equal(card.querySelector('.gauge').classList.contains('metric-stale'), true);
  assert.equal(card.querySelector('.last-report').hidden, false);
  sse.frame(snapshot(payload('', {nodes: []}))); await flush();
  assert.equal(ui.elements.nodes.children.length, 0);
  assert.equal(ui.elements.headline.textContent, copy('public.heading'));
  assert.equal(ui.elements.retry.hidden, true);
  // 空集合只由摘要行说明一次；空态盒子保持隐藏。
  assert.equal(ui.elements.empty.hidden, true);
  assert.equal(ui.elements.summary.textContent, copy('public.empty.all'));
  sse.frame(snapshot(payload('returned'))); await flush();
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'returned');
});

test('public invalid snapshot cannot clear an accepted collection or change its values', async (t) => {
  const sse = stream(); const ui = await setup(t, () => sse.response);
  ui.sockets[0].open(); ui.sockets[0].message(snapshot(payload('accepted')));
  ui.sockets[0].message(snapshot(payload('invalid', {has_more: true, next_offset: 0}))); await flush();
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'accepted');
  assert.equal(ui.elements.nodes.querySelector('.gauge-value').textContent, '25%');
  assert.equal(ui.elements.nodes.children[0].dataset.status, 'online');
});

test('public initial failure is a real loading error, never a fabricated empty collection', async (t) => {
  const ui = await setup(t, () => { throw new Error('offline'); });
  ui.sockets[0].fail(); await flush();
  assert.equal(ui.elements.nodes.children.length, 0);
  assert.equal(ui.elements.empty.hidden, false);
  assert.notEqual(ui.elements['empty-text'].textContent, '');
  assert.equal(ui.elements.retry.hidden, false);
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
  assert.equal(ui.elements.empty.hidden, false);
  assert.equal(ui.elements.retry.hidden, false);
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

test('public empty box explains an empty filter but not an empty collection', async (t) => {
  const ui = await setup(t); ui.sockets[0].open();
  const data = payload('online'); data.nodes.push({...data.nodes[0], name: 'offline', status: 'offline'});
  ui.sockets[0].message(snapshot(data));
  ui.elements.filters.children[2].click();
  ui.sockets[0].message(snapshot(payload('online')));
  assert.equal(ui.elements.empty.hidden, false);
  assert.equal(ui.elements['empty-text'].textContent, copy('public.filter.empty.offline'));
  ui.sockets[0].message(snapshot(payload('', {nodes: []})));
  assert.equal(ui.elements.empty.hidden, true);
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
  assert.equal(card.hidden, false); assert.equal(card.querySelector('.gauge-value').textContent, '25%');
  assert.notEqual(card.querySelectorAll('dd')[1].textContent, '—');
  assert.equal(card.querySelector('.gauge').classList.contains('metric-stale'), true);
  assert.equal(card.querySelector('.last-report').hidden, false);
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
    assert.equal(ui.elements.summary.textContent, copy('public.loading'));
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

test('public gauges clear both ARIA values when a metric becomes missing', async (t) => {
  const ui = await setup(t);
  const socket = ui.sockets[0]; socket.open();
  const data = payload();
  data.nodes[0].cpu_percent = 80;
  socket.message(snapshot(data));
  const meter = ui.elements.nodes.querySelector('.track');
  assert.equal(meter.getAttribute('aria-valuenow'), '80.0');
  assert.equal(meter.getAttribute('aria-valuetext'), '80%');
  data.nodes[0].metric_status.cpu_percent = 'stale';
  socket.message(snapshot(data));
  assert.equal(meter.getAttribute('aria-valuetext'), copy('public.metric.staleValue', {value: '80%', note: copy('public.metric.staleNote')}));
  data.nodes[0].cpu_percent = null;
  data.nodes[0].metric_status.cpu_percent = 'unavailable';
  socket.message(snapshot(data));
  assert.equal(meter.getAttribute('aria-valuenow'), null);
  assert.equal(meter.getAttribute('aria-valuetext'), null);
});


test('reduced-motion HTTP refresh and loading state work without any animation completion events', async (t) => {
  const descriptor = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => true});
  t.after(() => Object.defineProperty(reducedMotion, 'matches', descriptor));
  let release;
  const ui = await setup(t, (request) => request.url.includes('/stream?') ? response({}, {status: 503})
    : new Promise((resolve) => { release = () => resolve(response(snapshot(payload(), 10))); }));
  assert.equal(ui.elements.refresh.getAttribute('aria-busy'), 'true');
  assert.equal(ui.elements.announcement.textContent, copy('public.loading'));
  ui.sockets[0].fail(); await flush();
  release(); await flush();
  assert.equal(ui.elements.refresh.hasAttribute('aria-busy'), false);
  assert.equal(ui.elements.nodes.children.length, 1);
  assert.match(ui.elements.updated.textContent, /10/);
  await ui.advance(5000);
  assert.match(ui.elements.updated.textContent, /5/);
  await ui.advance(5000);
  assert.equal(ui.requests.length, 3, 'next HTTP poll is independent of CSS animationend');
  release(); await flush();
  assert.equal(ui.elements.refresh.hasAttribute('aria-busy'), false);
});

test('theme renderers receive validated snapshots, stale state and terminal withdrawal without owning transport', async (t) => {
  const events = [];
  const ui = await setup(t, () => new Promise(() => {}), (renderers) => ({
    ...renderers,
    createCard() {
      const card = renderers.createCard();
      const update = card.update;
      card.update = (node) => { events.push(['card', node.name]); update(node); };
      const setStale = card.setStale;
      card.setStale = () => { events.push(['stale']); setStale(); };
      return card;
    },
    createConstellation(container) {
      const constellation = renderers.createConstellation(container);
      return {
        update(nodes) { events.push(['overview', nodes.length]); constellation.update(nodes); },
        clear() { events.push(['clear']); constellation.clear(); }
      };
    },
    announcementRenderer(container) {
      const render = renderers.announcementRenderer(container);
      return (items) => { events.push(['announcements', items.length]); render(items); };
    }
  }));
  const socket = ui.sockets[0]; socket.open();
  socket.message(snapshot(payload('theme node')));
  assert.deepEqual(events, [['announcements', 0], ['card', 'theme node'], ['overview', 1]]);
  socket.message(snapshot(payload('rejected', {next_offset: 50})));
  await flush();
  assert.ok(events.some(([kind]) => kind === 'stale'));
  assert.ok(!events.some(([, name]) => name === 'rejected'));
  location.replace = (target) => { events.push(['redirect', target]); };
  // The replacement SSE request is deliberately pending; a new refresh opens WS.
  ui.elements.refresh.click();
  const next = ui.sockets.at(-1); next.open();
  next.message(frame('error', {code: 'public_page_disabled'}));
  await flush();
  assert.deepEqual(events.slice(-3), [['announcements', 0], ['clear'], ['redirect', '/']]);
});


test('public summary banner reports offline and unknown counts and only writes when its summary changes', async (t) => {
  const ui = await setup(t); const socket = ui.sockets[0]; socket.open();
  const summary = ui.elements.summary;
  const descriptor = Object.getOwnPropertyDescriptor(TestElement.prototype, 'textContent');
  let writes = 0;
  Object.defineProperty(summary, 'textContent', {get() { return descriptor.get.call(this); }, set(value) { writes++; descriptor.set.call(this, value); }});
  const data = payload();
  socket.message(snapshot(data));
  assert.equal(summary.dataset.tone, 'ok');
  assert.equal(summary.getAttribute('aria-live'), 'polite');
  assert.equal(summary.getAttribute('role'), 'status');
  assert.equal(summary.textContent, copy('public.summary.allOnline', {count: 1}));
  const before = writes;
  data.nodes[0].cpu_percent = 95;
  socket.message(snapshot(data));
  socket.message(frame('heartbeat'));
  assert.equal(writes, before);
  data.nodes.push({...data.nodes[0], name: 'down', status: 'offline'});
  socket.message(snapshot(data));
  assert.equal(summary.dataset.tone, 'warn');
  assert.equal(summary.textContent, copy('public.summary.offline', {count: 1}));
  data.nodes[0].status = 'offline'; socket.message(snapshot(data));
  assert.equal(summary.dataset.tone, 'bad');
  assert.equal(summary.textContent, copy('public.summary.offline', {count: 2}));
  data.nodes[0].status = 'unknown'; socket.message(snapshot(data));
  assert.equal(summary.dataset.tone, 'warn');
  assert.match(summary.textContent, /1 node offline · 1 node unknown/);
  socket.message(snapshot(payload('', {nodes: []})));
  assert.equal(summary.dataset.tone, 'neutral');
});

test('public card percentages round while threshold classes use shared strict boundaries and keep exact ARIA values', async (t) => {
  const ui = await setup(t); const socket = ui.sockets[0]; socket.open();
  const data = payload(); socket.message(snapshot(data));
  const card = ui.elements.nodes.children[0];
  const gauge = card.querySelector('.gauge'); const track = card.querySelector('.track');
  for (const [value, text, warn, bad] of [[44.4, '44%', false, false], [80, '80%', false, false], [80.1, '80%', true, false], [90, '90%', true, false], [90.1, '90%', false, true], [95.8, '96%', false, true]]) {
    data.nodes[0].cpu_percent = value; data.nodes[0].memory_percent = value;
    data.nodes[0].metric_status.memory_percent = 'ok'; socket.message(snapshot(data));
    assert.equal(gauge.querySelector('.gauge-value').textContent, text);
    assert.equal(track.getAttribute('aria-valuenow'), value.toFixed(1));
    assert.equal(track.getAttribute('aria-valuetext'), text);
    for (const meter of card.querySelectorAll('.gauge')) {
      assert.equal(meter.classList.contains('usage-warn'), warn);
      assert.equal(meter.classList.contains('usage-bad'), bad);
    }
  }
  data.nodes[0].cpu_percent = null; data.nodes[0].metric_status.cpu_percent = 'unavailable'; socket.message(snapshot(data));
  assert.equal(track.getAttribute('aria-valuenow'), null);
  assert.equal(track.getAttribute('aria-valuetext'), null);
  assert.equal(gauge.classList.contains('usage-bad'), false);
});

test('public search and region headings start above twelve, filter only loaded nodes, survive updates and reset on page turn', async (t) => {
  const ui = await setup(t); const socket = ui.sockets[0]; socket.open();
  const base = payload().nodes[0];
  const nodes = Array.from({length: 13}, (_, i) => ({...base, name: `node ${i}`, region: i % 2 ? 'West' : 'East', provider: i === 12 ? 'Unique Cloud' : 'Provider', status: i === 12 ? 'offline' : 'online'}));
  const search = ui.body.querySelector('.node-search');
  socket.message(snapshot(payload('', {nodes: nodes.slice(0, 12)})));
  assert.equal(search.hidden, true); assert.equal(ui.elements.nodes.querySelectorAll('.region-heading').length, 0);
  socket.message(snapshot(payload('', {nodes, has_more: true, next_offset: 50})));
  assert.equal(search.hidden, false); assert.equal(ui.elements.nodes.querySelectorAll('.region-heading').length, 2);
  const original = ui.elements.nodes.querySelectorAll('.node');
  const shown = () => ui.elements.nodes.querySelectorAll('.node').filter((card) => !card.hidden);
  const input = (value) => { search.value = value; search.dispatchEvent(new Event('input')); };
  input('uNiQuE'); assert.equal(shown().length, 1); assert.equal(shown()[0].querySelector('h2').textContent, 'node 12');
  assert.equal(ui.elements.nodes.querySelectorAll('.region-heading').filter((heading) => !heading.hidden).length, 1);
  input('West'); assert.equal(shown().length, 6);
  input('node 12'); assert.equal(shown().length, 1);
  ui.elements.filters.children[1].click(); assert.equal(shown().length, 0);
  assert.equal(ui.elements['empty-text'].textContent, copy('public.searchEmpty'));
  ui.elements.filters.children[0].click();
  socket.message(snapshot(payload('', {nodes, has_more: true, next_offset: 50})));
  assert.equal(search.value, 'node 12'); assert.equal(shown().length, 1);
  assert.deepEqual(ui.elements.nodes.querySelectorAll('.node'), original);
  ui.elements.next.click(); assert.equal(search.value, ''); assert.equal(search.hidden, true);
  assert.match(ui.sockets.at(-1).url, /offset=50/);
  ui.sockets.at(-1).open(); ui.sockets.at(-1).message(snapshot(payload('other page')));
  assert.equal(ui.elements.nodes.querySelectorAll('.region-heading').length, 0);
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'other page');
});

test('optional region heading renderer receives names; absent hooks retain the built-in themes contract', async (t) => {
  const received = [];
  const ui = await setup(t, undefined, (renderers) => ({...renderers, createRegionHeading(region) {
    received.push(region); const li = new PublicElement('li'); li.className = 'region-heading'; li.textContent = region; return li;
  }}));
  const base = payload().nodes[0]; ui.sockets[0].open();
  const nodes = Array.from({length: 13}, (_, i) => ({...base, name: `n${i}`, region: i % 2 ? 'West' : ''}));
  ui.sockets[0].message(snapshot(payload('', {nodes})));
  assert.deepEqual(received, [copy('public.regionUnknown'), 'West']);
  ui.sockets[0].message(snapshot(payload('', {nodes})));
  assert.equal(received.length, 2);
});
