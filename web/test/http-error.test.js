import test from 'node:test';
import assert from 'node:assert/strict';
import {responseErrorCode} from '../src/shared/http-error.js';

const encoded = (text) => new TextEncoder().encode(text);
test('403 code parsing is bounded by bytes and releases its reader', async () => {
  for (const size of [65536, 65537]) {
    const prefix = '{"code":"public_page_disabled","padding":"';
    const text = prefix + 'x'.repeat(size - prefix.length - 2) + '"}';
    const body = new ReadableStream({start(controller) { controller.enqueue(encoded(text)); controller.close(); }});
    const response = new Response(body, {status: 403});
    assert.equal(await responseErrorCode(response), size === 65536 ? 'public_page_disabled' : '');
    assert.equal(body.locked, false);
  }
});

test('only 403 JSON objects with a string code are inspected', async () => {
  for (const [status, value, expected] of [[403, '{"code":"forbidden"}', 'forbidden'], [403, 'null', ''], [403, '{', ''], [403, '{"code":1}', ''], [500, '{"code":"public_page_disabled"}', '']]) {
    let reads = 0;
    const body = new ReadableStream({pull(controller) { reads += 1; controller.enqueue(encoded(value)); controller.close(); }}, {highWaterMark: 0});
    assert.equal(await responseErrorCode(new Response(body, {status})), expected);
    if (status !== 403) assert.equal(reads, 0);
    assert.equal(body.locked, false);
  }
});

test('abort cancels a pending error-body read and releases the reader', async () => {
  let cancelled = false;
  const body = new ReadableStream({cancel() { cancelled = true; }});
  const controller = new AbortController();
  const reading = responseErrorCode(new Response(body, {status: 403}), controller.signal);
  controller.abort();
  await assert.rejects(reading, (error) => error.name === 'AbortError');
  assert.equal(cancelled, true);
  assert.equal(body.locked, false);
});
