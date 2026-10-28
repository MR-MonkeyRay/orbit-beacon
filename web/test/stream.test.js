import test from 'node:test';
import assert from 'node:assert/strict';
import {createSSEParser, openStream, StreamSetupError} from '../src/public/stream.js';

test('SSE parser handles every chunk boundary inside lines and CRLF pairs', () => {
  const input = 'event: nodes\r\ndata: {"nodes":[]}\r\n\r\nevent: close\ndata: {}\n\n';
  const expected = [{event: 'nodes', data: '{"nodes":[]}'}, {event: 'close', data: '{}'}];
  for (let split = 0; split <= input.length; split += 1) {
    const parser = createSSEParser();
    assert.deepEqual([...parser.feed(input.slice(0, split)), ...parser.feed(input.slice(split))], expected, `split ${split}`);
  }
  const parser = createSSEParser();
  assert.deepEqual([...input].flatMap((character) => parser.feed(character)), expected);
});

test('SSE parser joins data lines, ignores comments and unknown fields, and preserves unknown event names', () => {
  const parser = createSSEParser();
  assert.deepEqual(parser.feed(': comment\r\nid: ignored\nretry: 123\nunknown: ignored\nevent: other\ndata: first\ndata:  second\ndata\n\n'),
    [{event: 'other', data: 'first\n second\n'}]);
  assert.deepEqual(parser.feed('event: abandoned\n\n: comment\n\ndata: default\n\n'), [{event: 'message', data: 'default'}]);
  assert.deepEqual(parser.feed('data:\n\n'), [{event: 'message', data: ''}]);
  assert.deepEqual(parser.feed('data: unfinished'), []);
});

test('SSE parser caps incomplete lines and accumulated multi-line events at one MiB', () => {
  const limit = 1024 * 1024;
  const line = createSSEParser();
  assert.deepEqual(line.feed('x'.repeat(limit)), []);
  assert.throws(() => line.feed('x'), /buffer/);
  const event = createSSEParser();
  const partial = `data: ${'x'.repeat(limit / 2)}\n`;
  event.feed(partial);
  assert.throws(() => event.feed(partial), /buffer/);
  assert.throws(() => createSSEParser().feed('节'.repeat(Math.ceil(limit / 3))), /buffer/);
});

test('SSE parser resets its cap between completed events, including within a large chunk', () => {
  const parser = createSSEParser();
  const data = 'x'.repeat(600000);
  assert.deepEqual(parser.feed(`data: ${data}\n\ndata: ${data}\n\n`), [{event: 'message', data}, {event: 'message', data}]);
});

function mockFetch(t, implementation) {
  const original = globalThis.fetch;
  globalThis.fetch = implementation;
  t.after(() => { globalThis.fetch = original; });
}

function bodyStream() {
  let source;
  let cancelled = false;
  const body = new ReadableStream({start(controller) { source = controller; }, cancel() { cancelled = true; }});
  return {body, source, get cancelled() { return cancelled; }};
}

const options = (onFrame = () => {}, signal = new AbortController().signal) => ({offset: 50, limit: 50, signal, onFrame});
const bytes = (text) => new TextEncoder().encode(text);

for (const contentType of ['application/json', 'text/plain', 'text/event-streaming', null]) {
  test(`openStream rejects ${contentType ?? 'missing'} content type as a setup failure`, async (t) => {
    const stream = bodyStream();
    mockFetch(t, async () => new Response(stream.body, {headers: contentType ? {'Content-Type': contentType} : {}}));
    await assert.rejects(openStream(options()), StreamSetupError);
    assert.equal(stream.cancelled, true);
  });
}

for (const status of [404, 503]) {
  test(`openStream rejects HTTP ${status} as a setup failure`, async (t) => {
    mockFetch(t, async () => new Response('{}', {status, headers: {'Content-Type': 'application/json', 'Retry-After': '10'}}));
    await assert.rejects(openStream(options()), StreamSetupError);
  });
}

test('openStream reports fetch and missing body failures as setup failures', async (t) => {
  mockFetch(t, async () => { throw new TypeError('offline'); });
  await assert.rejects(openStream(options()), StreamSetupError);
  globalThis.fetch = async () => new Response(null, {headers: {'Content-Type': 'text/event-stream'}});
  await assert.rejects(openStream(options()), StreamSetupError);
});

test('openStream sends the required fetch options and dispatches split UTF-8 frames', async (t) => {
  const stream = bodyStream();
  const frames = [];
  const request = options((frame) => frames.push(frame));
  mockFetch(t, async (url, config) => {
    assert.equal(url, '/api/public/v1/stream?limit=50&offset=50');
    assert.deepEqual(config, {credentials: 'omit', cache: 'no-store', signal: request.signal, headers: {Accept: 'text/event-stream'}});
    return new Response(stream.body, {headers: {'Content-Type': 'Text/Event-Stream; charset=utf-8'}});
  });
  const result = openStream(request);
  const encoded = bytes('event: nodes\r\ndata: {"name":"节点"}\r\n\r\nevent: close\ndata: {}\n\n');
  for (const byte of encoded) stream.source.enqueue(Uint8Array.of(byte));
  await result;
  assert.deepEqual(frames, [{event: 'nodes', data: '{"name":"节点"}'}, {event: 'close', data: '{}'}]);
  assert.equal(stream.cancelled, true);
  assert.equal(stream.body.locked, false);
});

test('openStream stops on close without waiting for EOF or dispatching trailing frames', async (t) => {
  const stream = bodyStream();
  mockFetch(t, async () => new Response(stream.body, {headers: {'Content-Type': 'text/event-stream'}}));
  const frames = [];
  const pending = openStream(options((frame) => frames.push(frame)));
  stream.source.enqueue(bytes('event: close\ndata: {}\n\nevent: nodes\ndata: stale\n\n'));
  await pending;
  assert.deepEqual(frames, [{event: 'close', data: '{}'}]);
  assert.equal(stream.cancelled, true);
});

test('openStream abort stops a pending read and releases the reader', async (t) => {
  const stream = bodyStream();
  mockFetch(t, async () => new Response(stream.body, {headers: {'Content-Type': 'text/event-stream'}}));
  const active = new AbortController();
  const pending = openStream(options(() => assert.fail('unexpected frame'), active.signal));
  await Promise.resolve();
  assert.equal(stream.body.locked, true);
  active.abort();
  await assert.rejects(pending, {name: 'AbortError'});
  assert.equal(stream.cancelled, true);
  assert.equal(stream.body.locked, false);
});

test('openStream abort in a frame handler stops further frames in the same chunk', async (t) => {
  const stream = bodyStream();
  mockFetch(t, async () => new Response(stream.body, {headers: {'Content-Type': 'text/event-stream'}}));
  const active = new AbortController();
  const frames = [];
  const pending = openStream(options((frame) => { frames.push(frame); active.abort(); }, active.signal));
  stream.source.enqueue(bytes('event: nodes\ndata: first\n\nevent: nodes\ndata: second\n\n'));
  await assert.rejects(pending, {name: 'AbortError'});
  assert.deepEqual(frames, [{event: 'nodes', data: 'first'}]);
});

test('openStream rejects already aborted signals without fetching and discards late fetch responses', async (t) => {
  const active = new AbortController();
  active.abort();
  mockFetch(t, () => assert.fail('fetch after abort'));
  await assert.rejects(openStream(options(undefined, active.signal)), {name: 'AbortError'});
  const later = new AbortController();
  const stream = bodyStream();
  let resolve;
  globalThis.fetch = () => new Promise((done) => { resolve = done; });
  const pending = openStream(options(() => assert.fail('stale frame'), later.signal));
  later.abort();
  resolve(new Response(stream.body, {headers: {'Content-Type': 'text/event-stream'}}));
  await assert.rejects(pending, {name: 'AbortError'});
  assert.equal(stream.cancelled, true);
});

for (const failure of ['EOF', 'read', 'UTF-8', 'buffer', 'handler']) {
  test(`openStream ${failure} failure is a stream failure rather than setup failure`, async (t) => {
    const stream = bodyStream();
    mockFetch(t, async () => new Response(stream.body, {headers: {'Content-Type': 'text/event-stream'}}));
    const pending = openStream(options(() => { if (failure === 'handler') throw new Error('invalid payload'); }));
    if (failure === 'EOF') stream.source.close();
    else if (failure === 'read') stream.source.error(new Error('read failed'));
    else if (failure === 'UTF-8') stream.source.enqueue(Uint8Array.of(0xff));
    else if (failure === 'buffer') stream.source.enqueue(bytes('x'.repeat(1024 * 1024 + 1)));
    else stream.source.enqueue(bytes('event: nodes\ndata: {}\n\n'));
    await assert.rejects(pending, (error) => error instanceof Error && !(error instanceof StreamSetupError));
    assert.equal(stream.body.locked, false);
  });
}
