import test from 'node:test';
import assert from 'node:assert/strict';

import {validResponse} from '../src/public/validate.js';

function node(overrides = {}) {
  return {
    name: 'edge-1',
    region: 'us-west',
    provider: 'Orbit',
    status: 'online',
    cpu_percent: 12.5,
    memory_percent: 48,
    load1: 0.75,
    uptime_seconds: 3600,
    last_seen: '2026-10-27T12:34:56Z',
    metric_status: {cpu_percent: 'ok', memory_percent: 'ok', load1: 'ok', uptime_seconds: 'ok'},
    ...overrides,
  };
}

function response(overrides = {}) {
  return {
    nodes: [node()],
    has_more: false,
    next_offset: null,
    generated_at: '2026-10-27T12:34:56Z',
    ...overrides,
  };
}

test('accepts valid payloads, nullable metrics, empty arrays, and extra fields', () => {
  assert.equal(validResponse(response(), 0), true);
  assert.equal(validResponse(response({nodes: [node({cpu_percent: null, memory_percent: null, load1: null, uptime_seconds: null,
    last_seen: null, metric_status: {cpu_percent: 'unavailable', memory_percent: 'unavailable', load1: 'unavailable', uptime_seconds: 'unavailable'}, extra: true})], extra: true}), 0), true);
  assert.equal(validResponse(response({nodes: []}), 0), true);
  assert.equal(validResponse(response({has_more: true, next_offset: 50}), 0), true);
  assert.equal(validResponse(response({nodes: Array.from({length: 100}, () => node())}), 0), true);
});

test('rejects missing, malformed, and oversized node arrays', () => {
  assert.equal(validResponse(null, 0), false);
  assert.equal(validResponse(response({nodes: undefined}), 0), false);
  assert.equal(validResponse(response({nodes: {}}), 0), false);
  assert.equal(validResponse(response({nodes: Array.from({length: 101}, () => node())}), 0), false);
});

test('rejects invalid pagination fields', () => {
  for (const hasMore of [null, 0, 'false']) assert.equal(validResponse(response({has_more: hasMore}), 0), false);
  for (const nextOffset of [null, 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, '50']) {
    assert.equal(validResponse(response({has_more: true, next_offset: nextOffset}), 0), false);
  }
  assert.equal(validResponse(response({has_more: true, next_offset: 50}), 50), false);
  assert.equal(validResponse(response({has_more: true, next_offset: 50}), 100), false);
  for (const nextOffset of [0, 50, undefined, '']) {
    assert.equal(validResponse(response({next_offset: nextOffset}), 0), false);
  }
});

test('rejects missing or invalid generated timestamps', () => {
  for (const generatedAt of [undefined, null, '', 'not-a-date', {}]) {
    assert.equal(validResponse(response({generated_at: generatedAt}), 0), false);
  }
});

test('rejects null nodes and wrong identity field types', () => {
  assert.equal(validResponse(response({nodes: [null]}), 0), false);
  for (const key of ['name', 'region', 'provider']) {
    for (const value of [undefined, null, 1, true, {}]) {
      assert.equal(validResponse(response({nodes: [node({[key]: value})]}), 0), false);
    }
  }
});

test('rejects unknown and wrongly typed statuses', () => {
  for (const status of [undefined, null, '', 'degraded', 1]) {
    assert.equal(validResponse(response({nodes: [node({status})]}), 0), false);
  }
  for (const status of ['online', 'offline', 'unknown']) {
    assert.equal(validResponse(response({nodes: [node({status})]}), 0), true);
  }
});

test('rejects missing, wrongly typed, non-finite, and negative metrics', () => {
  for (const key of ['cpu_percent', 'memory_percent', 'load1', 'uptime_seconds']) {
    for (const value of [undefined, '1', true, {}, NaN, Infinity, -1]) {
      assert.equal(validResponse(response({nodes: [node({[key]: value})]}), 0), false, `${key} accepted ${String(value)}`);
    }
  }
});

test('rejects percentages above 100 while allowing larger load and uptime values', () => {
  assert.equal(validResponse(response({nodes: [node({cpu_percent: 100.01})]}), 0), false);
  assert.equal(validResponse(response({nodes: [node({memory_percent: 101})]}), 0), false);
  assert.equal(validResponse(response({nodes: [node({load1: 101, uptime_seconds: 1_000_000})]}), 0), true);
});

test('requires nullable last_seen and exactly the four public metric states without contradictory values', () => {
  for (const last_seen of [undefined, '', 'not-a-date', 123, {}]) {
    assert.equal(validResponse(response({nodes: [node({last_seen})]})), false);
  }
  assert.equal(validResponse(response({nodes: [node({status: 'offline', metric_status: {cpu_percent: 'stale', memory_percent: 'stale', load1: 'stale', uptime_seconds: 'stale'}})]})), true);
  for (const key of ['cpu_percent', 'memory_percent', 'load1', 'uptime_seconds']) {
    for (const status of [undefined, null, 'error', 'unknown', 0]) {
      assert.equal(validResponse(response({nodes: [node({metric_status: {...node().metric_status, [key]: status}})]})), false);
    }
    assert.equal(validResponse(response({nodes: [node({[key]: null})]})), false);
    assert.equal(validResponse(response({nodes: [node({metric_status: {...node().metric_status, [key]: 'unavailable'}})]})), false);
  }
  assert.equal(validResponse(response({nodes: [node({metric_status: {...node().metric_status, runtime: 'ok'}})]})), false);
});
