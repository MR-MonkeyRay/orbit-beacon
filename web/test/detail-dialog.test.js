import test from 'node:test';
import assert from 'node:assert/strict';
import {flush, installDOM} from './console-dom.js';

test('detail dialog owns loading, replacement, disposal, and navigation abort', async () => {
  const dom = installDOM();
  const originalMatchMedia = globalThis.matchMedia;
  globalThis.matchMedia = () => ({matches: true});
  try {
    const {detailDialog} = await import('../src/console/ui/detail-dialog.js');
    let resolve;
    let disposed = 0;
    const navigation = new AbortController();
    const dialog = detailDialog({
      title: 'Details', loading: 'Loading', signal: navigation.signal,
      load: () => new Promise((done) => { resolve = done; }),
      render: (value) => ({nodes: document.createElement(value), dispose: () => { disposed += 1; }}),
      failure: () => document.createElement('error')
    });
    assert.equal(dom.body.querySelector('p')?.textContent, 'Loading');
    resolve('section');
    await flush();
    assert.ok(dom.body.querySelector('section'));
    navigation.abort();
    assert.equal(await dialog.closed, 'navigation');
    assert.equal(disposed, 1);
    assert.equal(dom.body.querySelector('dialog'), null);
  } finally {
    dom.restore();
    if (originalMatchMedia === undefined) delete globalThis.matchMedia; else globalThis.matchMedia = originalMatchMedia;
  }
});

test('detail dialog rejects invalid IDs without loading', async () => {
  const dom = installDOM();
  try {
    const {detailDialog} = await import('../src/console/ui/detail-dialog.js');
    let loads = 0;
    detailDialog({title: 'Details', loading: 'Loading', valid: false, invalid: 'Invalid', load: () => { loads += 1; }, render: () => null, failure: () => null});
    assert.equal(loads, 0);
    assert.equal(dom.body.querySelector('p')?.textContent, 'Invalid');
  } finally { dom.restore(); }
});
