import {flush, installDOM} from './console-dom.js';

export function liveHarness(t, respond = () => { throw new Error('offline'); }) {
  const dom = installDOM();
  globalThis.location.href = 'https://example.test/';
  const originals = new Map(['window', 'fetch', 'WebSocket', 'performance', 'setTimeout', 'clearTimeout'].map((key) => [key, globalThis[key]]));
  const originalNow = Date.now;
  const window = globalThis.window = new EventTarget();
  const timers = new Map();
  const sockets = [];
  const requests = [];
  const order = [];
  let time = Date.parse('2026-10-28T00:00:00Z');
  const started = time;
  let wallOffset = 0;
  globalThis.performance = {now: () => time - started};
  let timerID = 0;
  Date.now = () => time + wallOffset;
  globalThis.setTimeout = (callback, delay = 0) => { timers.set(++timerID, {callback, at: time + delay}); return timerID; };
  globalThis.clearTimeout = (id) => timers.delete(id);
  globalThis.WebSocket = class extends EventTarget {
    constructor(url) { super(); this.url = url; this.readyState = 0; sockets.push(this); order.push('ws:start'); }
    open() { this.readyState = 1; this.dispatchEvent(new Event('open')); }
    message(frame) { this.dispatchEvent(Object.assign(new Event('message'), {data: typeof frame === 'string' ? frame : JSON.stringify(frame)})); }
    fail() { this.dispatchEvent(new Event('error')); }
    end() { this.readyState = 3; this.dispatchEvent(new Event('close')); }
    close() { this.closed = true; this.readyState = 3; order.push('ws:close'); }
  };
  globalThis.fetch = async (url, options) => {
    const request = {url, ...options};
    requests.push(request);
    order.push(url.includes('stream') ? 'sse:start' : 'http:start');
    return respond(request, requests.length);
  };
  const advance = async (milliseconds) => {
    const target = time + milliseconds;
    for (;;) {
      const next = [...timers].filter(([, timer]) => timer.at <= target).sort((a, b) => a[1].at - b[1].at)[0];
      if (!next) break;
      time = next[1].at;
      timers.delete(next[0]);
      next[1].callback();
      await flush();
    }
    time = target;
    await flush();
  };
  t.after(async () => {
    window.dispatchEvent(new Event('pagehide'));
    await flush();
    Date.now = originalNow;
    for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; }
    dom.restore();
  });
  return {...dom, window, timers, sockets, requests, order, advance, shiftWall: (milliseconds) => { wallOffset += milliseconds; }};
}

export const frame = (type = 'snapshot', extra = {}) => ({type, server_time: '2026-10-28T00:00:00Z', update_interval_seconds: 10,
  ...(type === 'snapshot' ? {data: {value: 1}} : {}), ...extra});
export const response = (value, init) => new Response(JSON.stringify(value), init);
export function stream() {
  let source;
  let cancelled = false;
  const body = new ReadableStream({start(controller) { source = controller; }, cancel() { cancelled = true; }});
  return {response: new Response(body, {headers: {'Content-Type': 'text/event-stream'}}),
    frame(value) { source.enqueue(new TextEncoder().encode(`event: message\ndata: ${JSON.stringify(value)}\n\n`)); },
    text(value) { source.enqueue(new TextEncoder().encode(value)); },
    bytes(value) { source.enqueue(value); },
    end() { source.close(); }, error() { source.error(new Error('connection lost')); },
    get cancelled() { return cancelled; }};
}
