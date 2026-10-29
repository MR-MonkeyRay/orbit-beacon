import test from 'node:test';
import assert from 'node:assert/strict';
import {createSSEParser, openSSE, StreamSetupError} from '../src/shared/sse.js';

const wire = 'event: message\r\ndata: {"type":"heartbeat"}\r\n\r\ndata: {"type":"close","code":"renew"}\n\n';
const frames = [{event: 'message', data: '{"type":"heartbeat"}'}, {event: 'message', data: '{"type":"close","code":"renew"}'}];
test('bounded SSE parser handles every chunk and CRLF boundary', () => {
  for (let split = 0; split <= wire.length; split++) {
    const parser = createSSEParser();
    assert.deepEqual([...parser.feed(wire.slice(0, split)), ...parser.feed(wire.slice(split))], frames);
  }
  const parser = createSSEParser();
  assert.deepEqual([...wire].flatMap((text) => parser.feed(text)), frames);
});
test('SSE parser joins data, ignores comments and unknown fields, and defaults to message', () => {
  const parser = createSSEParser();
  assert.deepEqual(parser.feed(': comment\r\nid: ignored\nretry: 123\nunknown: ignored\nevent: other\ndata: first\ndata:  second\ndata\n\n'),
    [{event: 'other', data: 'first\n second\n'}]);
  assert.deepEqual(parser.feed('event: abandoned\n\n: comment\n\ndata: default\n\n'), [{event: 'message', data: 'default'}]);
  assert.deepEqual(parser.feed('data:\n\n'), [{event: 'message', data: ''}]);
  assert.deepEqual(parser.feed('data: unfinished'), []);
});
test('SSE parser caps both UTF-8 incomplete lines and accumulated events at one MiB', () => {
  const limit = 1024 * 1024;
  const parser = createSSEParser();
  assert.deepEqual(parser.feed('x'.repeat(limit)), []);
  assert.throws(() => parser.feed('x'), /buffer/);
  const event = createSSEParser();
  const partial = `data: ${'x'.repeat(limit / 2)}\n`;
  event.feed(partial); assert.throws(() => event.feed(partial), /buffer/);
  assert.throws(() => createSSEParser().feed('节'.repeat(Math.ceil(limit / 3))), /buffer/);
  const data = 'x'.repeat(600000);
  assert.deepEqual(createSSEParser().feed(`data: ${data}\n\ndata: ${data}\n\n`), [{event: 'message', data}, {event: 'message', data}]);
});
function mockFetch(t, implementation) {
  const original = globalThis.fetch; globalThis.fetch = implementation; t.after(() => { globalThis.fetch = original; });
}
function bodyStream() {
  let source; let cancelled = false;
  const body = new ReadableStream({start(controller) { source = controller; }, cancel() { cancelled = true; }});
  return {body, source, get cancelled() { return cancelled; }};
}
const options = (onFrame = () => {}, signal = new AbortController().signal) => ({url: '/stream?limit=50&offset=50', credentials: 'omit', signal, onFrame});
const bytes = (text) => new TextEncoder().encode(text);
for (const contentType of ['application/json', 'text/plain', 'text/event-streaming', null]) {
  test(`SSE rejects ${contentType ?? 'missing'} content type and cancels its body`, async (t) => {
    const stream = bodyStream();
    mockFetch(t, async () => new Response(stream.body, {headers: contentType ? {'Content-Type': contentType} : {}}));
    await assert.rejects(openSSE(options()), StreamSetupError); assert.equal(stream.cancelled, true);
  });
}
for (const status of [401, 404, 503]) {
  test(`SSE exposes HTTP ${status} for the shared channel policy`, async (t) => {
    mockFetch(t, async () => new Response('{}', {status}));
    await assert.rejects(openSSE(options()), (error) => error instanceof StreamSetupError && error.response.status === status);
  });
}
test('SSE preserves explicit public-page-disabled setup codes before discarding the body', async (t) => {
  mockFetch(t, async () => new Response(JSON.stringify({code: 'public_page_disabled'}), {status: 403}));
  await assert.rejects(openSSE(options()), (error) => error instanceof StreamSetupError
    && error.response.status === 403 && error.errorCode === 'public_page_disabled' && !error.response.body.locked);
});
test('SSE sends explicit credentials and decodes split UTF-8; handler termination cancels reader', async (t) => {
  const stream = bodyStream(); const received = [];
  const request = options((frame) => { received.push(frame); return received.length < 2; });
  mockFetch(t, async (url, config) => {
    assert.equal(url, request.url);
    assert.deepEqual(config, {credentials: 'omit', cache: 'no-store', signal: request.signal, headers: {Accept: 'text/event-stream'}});
    return new Response(stream.body, {headers: {'Content-Type': 'Text/Event-Stream; charset=utf-8'}});
  });
  const pending = openSSE(request);
  const input = bytes('event: message\r\ndata: {"name":"节点"}\r\n\r\ndata: close\n\ndata: ignored\n\n');
  for (const byte of input) stream.source.enqueue(Uint8Array.of(byte));
  await pending;
  assert.deepEqual(received, [{event: 'message', data: '{"name":"节点"}'}, {event: 'message', data: 'close'}]);
  assert.equal(stream.cancelled, true); assert.equal(stream.body.locked, false);
});
test('SSE abort cancels pending read and releases lock', async (t) => {
  const stream = bodyStream(); mockFetch(t, async () => new Response(stream.body, {headers: {'Content-Type': 'text/event-stream'}}));
  const active = new AbortController(); const pending = openSSE(options(() => assert.fail('late frame'), active.signal));
  await Promise.resolve(); active.abort();
  await assert.rejects(pending, {name: 'AbortError'});
  assert.equal(stream.cancelled, true); assert.equal(stream.body.locked, false);
});
test('SSE ignores late setup responses and frame callbacks after abort', async (t) => {
  const stream = bodyStream(); let resolve;
  mockFetch(t, () => new Promise((done) => { resolve = done; }));
  const active = new AbortController(); const pending = openSSE(options(() => assert.fail('late frame'), active.signal));
  active.abort(); resolve(new Response(stream.body, {headers: {'Content-Type': 'text/event-stream'}}));
  await assert.rejects(pending, {name: 'AbortError'}); assert.equal(stream.cancelled, true);
});
for (const failure of ['EOF', 'read', 'UTF-8', 'buffer', 'handler']) {
  test(`SSE ${failure} errors release their reader`, async (t) => {
    const stream = bodyStream(); mockFetch(t, async () => new Response(stream.body, {headers: {'Content-Type': 'text/event-stream'}}));
    const pending = openSSE(options(() => { if (failure === 'handler') throw new Error('invalid payload'); }));
    if (failure === 'EOF') stream.source.close();
    else if (failure === 'read') stream.source.error(new Error('read failed'));
    else if (failure === 'UTF-8') stream.source.enqueue(Uint8Array.of(0xff));
    else if (failure === 'buffer') stream.source.enqueue(bytes('x'.repeat(1024 * 1024 + 1)));
    else stream.source.enqueue(bytes('data: {}\n\n'));
    await assert.rejects(pending); assert.equal(stream.body.locked, false);
  });
}
