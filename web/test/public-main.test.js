import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {flush, installDOM, TestElement} from './console-dom.js';

const entry = new URL('../src/public/main.js', import.meta.url);
const source = (await readFile(entry, 'utf8')).replace("import './public.css';", '')
  .replace(/from '(\.\.?\/[^']+)'/g, (_match, path) => `from '${new URL(path, entry).href}'`);
let run = 0;
const payload = (name = 'edge', extra = {}) => ({
  nodes: [{name, region: '', provider: '', status: 'online', cpu_percent: null, memory_percent: null, load1: null, uptime_seconds: null}],
  generated_at: '2026-10-27T12:34:56Z', has_more: false, next_offset: null, ...extra
});

class PublicElement extends TestElement {
  constructor(tag) { super(tag); this.style.setProperty = () => {}; }
  get lastChild() { return this.children.at(-1); }
  getBoundingClientRect() { return {}; }
}

async function setup(t, respond) {
  const dom = installDOM();
  dom.document.createElement = (tag) => new PublicElement(tag);
  dom.document.createElementNS = (_namespace, tag) => new PublicElement(tag);
  const ids = ['nodes', 'empty', 'empty-text', 'retry', 'headline', 'summary', 'updated', 'announcement', 'refresh', 'countdown', 'filters', 'pagination', 'previous', 'next', 'page', 'constellation', 'theme'];
  const elements = Object.fromEntries(ids.map((id) => {
    const element = new PublicElement('div');
    element.id = id;
    dom.body.append(element);
    return [id, element];
  }));
  const originals = new Map(['window', 'fetch', 'setTimeout', 'clearTimeout'].map((key) => [key, globalThis[key]]));
  const window = globalThis.window = new EventTarget();
  const timers = new Map();
  let timerID = 0;
  globalThis.setTimeout = (callback, delay) => { timers.set(++timerID, {callback, delay}); return timerID; };
  globalThis.clearTimeout = (id) => timers.delete(id);
  const requests = [];
  globalThis.fetch = async (url, options) => {
    const request = {url, ...options};
    requests.push(request);
    return respond(request, requests.length);
  };
  t.after(async () => {
    window.dispatchEvent(new Event('pagehide'));
    await flush();
    for (const [key, value] of originals) {
      if (value === undefined) delete globalThis[key]; else globalThis[key] = value;
    }
    dom.restore();
  });
  const tick = async (delay) => {
    const pending = [...timers].filter(([, timer]) => timer.delay === delay);
    assert.ok(pending.length, `expected ${delay}ms timer`);
    for (const [id, timer] of pending) { timers.delete(id); timer.callback(); }
    await flush();
  };
  await import(`data:text/javascript;base64,${Buffer.from(`${source}\n// run ${++run}`).toString('base64')}`);
  await flush();
  return {document: dom.document, window, elements, requests, timers, tick};
}

function stream() {
  let controller;
  let cancelled = false;
  const body = new ReadableStream({start(value) { controller = value; }, cancel() { cancelled = true; }});
  return {
    response: new Response(body, {headers: {'Content-Type': 'text/event-stream'}}),
    frame: (event, data) => controller.enqueue(new TextEncoder().encode(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`)),
    text: (value) => controller.enqueue(new TextEncoder().encode(value)),
    end: () => controller.close(),
    error: () => controller.error(new Error('connection lost')),
    get cancelled() { return cancelled; }
  };
}

test('public stream renders validated frames, announces only the first and reconnects on close without clearing', async (t) => {
  const first = stream();
  const second = stream();
  const ui = await setup(t, (_request, index) => index === 1 ? first.response : second.response);
  assert.equal(ui.requests[0].url, '/api/public/v1/stream?limit=50&offset=0');
  assert.ok([...ui.timers.values()].some(({delay}) => delay === 8000));
  first.frame('nodes', payload('first'));
  await flush();
  assert.equal(ui.elements.countdown.dataset.state, 'live');
  assert.equal(ui.elements.countdown.classList.contains('running'), false);
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'first');
  assert.match(ui.elements.announcement.textContent, /全部在线/);
  assert.ok([...ui.timers.values()].some(({delay}) => delay === 18000));
  assert.equal(ui.timers.size, 1);
  const deadline = [...ui.timers.keys()][0];
  first.text(': heartbeat\n\n');
  await flush();
  assert.equal([...ui.timers.keys()][0], deadline);
  first.frame('nodes', payload('second'));
  await flush();
  assert.equal(ui.elements.announcement.textContent, '');
  assert.equal(ui.timers.size, 1);
  assert.notEqual([...ui.timers.keys()][0], deadline);
  const displayed = ui.elements.nodes.children[0];
  first.frame('close', {});
  await flush();
  assert.equal(ui.requests.length, 2);
  assert.equal(first.cancelled, true);
  assert.equal(ui.elements.nodes.children[0], displayed);
  assert.equal(ui.elements.countdown.dataset.state, 'live');
  second.frame('nodes', payload('third'));
  await flush();
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'third');
  assert.equal(ui.elements.announcement.textContent, '');
});

for (const failure of ['status', 'content-type', 'fetch']) {
  test(`public stream ${failure} setup failure falls back to polling until manual refresh`, async (t) => {
    const live = stream();
    let attempts = 0;
    const ui = await setup(t, (request) => {
      if (request.url.includes('/nodes?')) return new Response(JSON.stringify(payload('polled')));
      if (++attempts > 1) return live.response;
      if (failure === 'fetch') throw new Error('offline');
      return new Response('{}', {status: failure === 'status' ? 503 : 200, headers: {'Content-Type': 'application/json'}});
    });
    assert.equal(ui.requests.length, 2);
    assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'polled');
    assert.equal(ui.elements.countdown.dataset.state, 'ok');
    assert.equal(ui.elements.countdown.classList.contains('running'), true);
    await ui.tick(10000);
    assert.equal(ui.requests.length, 3);
    assert.ok(ui.requests[2].url.includes('/nodes?'));
    ui.elements.refresh.click();
    await flush();
    assert.equal(attempts, 2);
    live.frame('nodes', payload('live again'));
    await flush();
    assert.equal(ui.elements.countdown.dataset.state, 'live');
  });
}

for (const failure of ['invalid payload', 'invalid JSON', 'unknown event', 'invalid close', 'EOF', 'read error', 'timeout']) {
  test(`public stream ${failure} clears displayed data and retries the stream after ten seconds`, async (t) => {
    const first = stream();
    const second = stream();
    const ui = await setup(t, (_request, index) => index === 1 ? first.response : second.response);
    first.frame('nodes', payload('visible'));
    await flush();
    if (failure === 'invalid payload') first.frame('nodes', payload('secret', {has_more: true, next_offset: 0}));
    else if (failure === 'invalid JSON') first.text('event: nodes\ndata: {\n\n');
    else if (failure === 'unknown event') first.frame('other', {});
    else if (failure === 'invalid close') first.frame('close', {unexpected: true});
    else if (failure === 'EOF') first.end();
    else if (failure === 'read error') first.error();
    else await ui.tick(18000);
    await flush();
    assert.equal(ui.elements.nodes.children.length, 0);
    assert.equal(ui.elements.countdown.dataset.state, 'error');
    assert.equal(ui.elements.retry.hidden, false);
    assert.match(ui.elements.summary.textContent, /10 秒/);
    assert.equal(ui.requests.length, 1);
    await ui.tick(10000);
    assert.equal(ui.requests.length, 2);
    assert.ok(ui.requests[1].url.includes('/stream?'));
  });
}

test('public stream first-frame timeout is a failure, and comments do not renew liveness', async (t) => {
  const first = stream();
  const ui = await setup(t, () => first.response);
  first.text(': heartbeat\n\n');
  await flush();
  await ui.tick(8000);
  assert.equal(first.cancelled, true);
  assert.equal(ui.elements.countdown.dataset.state, 'error');
  assert.equal(ui.requests.length, 1);
});

test('public stream setup timeout shows failure without falling back to polling', async (t) => {
  const ui = await setup(t, (request) => new Promise((_resolve, reject) => {
    request.signal.addEventListener('abort', () => reject(request.signal.reason), {once: true});
  }));
  await ui.tick(8000);
  assert.equal(ui.requests[0].signal.aborted, true);
  assert.equal(ui.elements.countdown.dataset.state, 'error');
  assert.equal(ui.elements.retry.hidden, false);
  assert.equal(ui.requests.length, 1);
  assert.ok([...ui.timers.values()].some(({delay}) => delay === 10000));
});

for (const failure of ['invalid payload', 'EOF', 'read error']) {
  test(`public stream ${failure} before the first nodes frame is a failure without polling fallback`, async (t) => {
    const first = stream();
    const second = stream();
    const ui = await setup(t, (_request, index) => index === 1 ? first.response : second.response);
    if (failure === 'invalid payload') first.frame('nodes', {});
    else if (failure === 'EOF') first.end();
    else first.error();
    await flush();
    assert.equal(ui.elements.countdown.dataset.state, 'error');
    assert.equal(ui.requests.length, 1);
    ui.elements.retry.click();
    await flush();
    assert.ok(ui.requests[1].url.includes('/stream?'));
    second.frame('nodes', payload('recovered'));
    await flush();
    assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'recovered');
    assert.match(ui.elements.announcement.textContent, /全部在线/);
  });
}

test('public stream refresh ignores a late response from the cancelled setup request', async (t) => {
  const old = stream();
  const current = stream();
  let resolve;
  const pending = new Promise((done) => { resolve = done; });
  const ui = await setup(t, (_request, index) => index === 1 ? pending : current.response);
  ui.elements.refresh.click();
  await flush();
  current.frame('nodes', payload('current'));
  await flush();
  resolve(old.response);
  await flush();
  assert.equal(ui.requests[0].signal.aborted, true);
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'current');
  assert.equal(ui.elements.countdown.dataset.state, 'live');
  assert.equal(ui.requests.length, 2);
});

test('public stream page changes abort the old reader and validate the new offset', async (t) => {
  const first = stream();
  const second = stream();
  const ui = await setup(t, (_request, index) => index === 1 ? first.response : second.response);
  first.frame('nodes', payload('page one', {has_more: true, next_offset: 50}));
  await flush();
  first.frame('nodes', payload('late page one'));
  ui.elements.next.click();
  await flush();
  assert.equal(ui.requests[0].signal.aborted, true);
  assert.equal(first.cancelled, true);
  assert.equal(ui.requests[1].url, '/api/public/v1/stream?limit=50&offset=50');
  assert.equal(ui.elements.nodes.querySelector('h2'), null);
  second.frame('nodes', payload('bad cursor', {has_more: true, next_offset: 50}));
  await flush();
  assert.equal(ui.elements.nodes.children.length, 0);
  assert.equal(ui.elements.countdown.dataset.state, 'error');
});

test('public stream pauses and clears on hidden/pagehide, resumes on visibility and persisted pageshow', async (t) => {
  const streams = [stream(), stream(), stream()];
  const ui = await setup(t, (_request, index) => streams[index - 1].response);
  streams[0].frame('nodes', payload('visible'));
  await flush();
  streams[0].frame('nodes', payload('late update'));
  ui.document.hidden = true;
  ui.document.dispatchEvent(new Event('visibilitychange'));
  await flush();
  assert.equal(ui.elements.nodes.children.length, 0);
  assert.equal(ui.requests[0].signal.aborted, true);
  assert.equal(ui.timers.size, 0);
  assert.equal(ui.elements.refresh.hasAttribute('aria-busy'), false);
  ui.document.hidden = false;
  ui.document.dispatchEvent(new Event('visibilitychange'));
  await flush();
  streams[1].frame('nodes', payload('visible again'));
  await flush();
  assert.equal(ui.elements.nodes.querySelector('h2').textContent, 'visible again');
  ui.window.dispatchEvent(new Event('pagehide'));
  await flush();
  assert.equal(ui.elements.nodes.children.length, 0);
  assert.equal(ui.requests[1].signal.aborted, true);
  assert.equal(ui.timers.size, 0);
  ui.window.dispatchEvent(Object.assign(new Event('pageshow'), {persisted: true}));
  await flush();
  assert.equal(ui.requests.length, 3);
  streams[2].frame('nodes', payload('restored'));
  await flush();
  assert.equal(ui.elements.countdown.dataset.state, 'live');
});
