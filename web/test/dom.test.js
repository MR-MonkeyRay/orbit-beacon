import test from 'node:test';
import assert from 'node:assert/strict';

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
