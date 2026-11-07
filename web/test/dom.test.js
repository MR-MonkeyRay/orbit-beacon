import test from 'node:test';
import assert from 'node:assert/strict';
import {copy, copyPattern} from './console-dom.js';
import en from '../src/shared/locales/en.js';

test('reducedMotion is lazy and preserves live .matches access', async () => {
  let calls = 0;
  const query = {matches: false};
  globalThis.matchMedia = (value) => {
    assert.equal(value, '(prefers-reduced-motion: reduce)');
    calls += 1;
    return query;
  };
  try {
    const {reducedMotion} = await import('../src/shared/dom.js');
    assert.equal(calls, 0);
    assert.equal(reducedMotion.matches, false);
    assert.equal(calls, 1);
    query.matches = true;
    assert.equal(reducedMotion.matches, true);
    assert.equal(calls, 1);
  } finally {
    delete globalThis.matchMedia;
  }
});

test('test copy resolves catalog keys, parameters, plurals and literal regex characters', () => {
  assert.equal(copy('common.unavailable'), en['common.unavailable']);
  assert.equal(copy('nodes.probe.checks', {percent: '100%', count: 1}), en['nodes.probe.checks'].one.replace('{percent}', '100%').replace('{count}', '1'));
  assert.equal(copy('nodes.probe.checks', {percent: '0%', count: 2}), en['nodes.probe.checks'].other.replace('{percent}', '0%').replace('{count}', '2'));
  const params = {name: 'node [a+b].*? ($1)'};
  assert.equal(copy('nodes.selectNode', params), en['nodes.selectNode'].replace('{name}', params.name));
  assert.match(copy('nodes.selectNode', params), copyPattern('nodes.selectNode', params));
  assert.doesNotMatch(copy('nodes.selectNode', {name: 'node aab ($1)'}), copyPattern('nodes.selectNode', params));
  assert.throws(() => copy('missing.key'));
  assert.throws(() => copy('nodes.selectNode'));
});
