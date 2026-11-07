import test from 'node:test';
import assert from 'node:assert/strict';
import {createNumberTween} from '../src/shared/tween.js';
import {motionTiming} from '../src/shared/motion.js';
import {reducedMotion} from '../src/shared/dom.js';
import {metricText} from '../src/console/lib/nodes-table.js';
import {installDOM, TestElement} from './console-dom.js';

function setup(t) {
  const dom = installDOM();
  const originalWindow = globalThis.window;
  globalThis.window = new EventTarget();
  const descriptor = Object.getOwnPropertyDescriptor(reducedMotion, 'matches');
  let reduce = false;
  Object.defineProperty(reducedMotion, 'matches', {configurable: true, get: () => reduce});
  const frames = new Map();
  const page = new AbortController();
  let time = 0;
  let id = 0;
  let reads = 0;
  const tween = createNumberTween({signal: page.signal, now: () => time, timing: () => { reads++; return {duration: 250}; },
    requestFrame: (callback) => { frames.set(++id, callback); return id; }, cancelFrame: (id) => frames.delete(id)});
  t.after(() => { page.abort(); Object.defineProperty(reducedMotion, 'matches', descriptor); globalThis.window = originalWindow; dom.restore(); });
  const cell = new TestElement('td'); dom.body.append(cell);
  return {...dom, tween, cell, frames, page, get reads() { return reads; }, reduce: (value) => { reduce = value; },
    step(value) { time = value; const pending = [...frames.values()]; frames.clear(); pending.forEach((callback) => callback(time)); }};
}

test('number tween retargets the displayed value, ignores repeated targets, and ends with exact caller formatting', (t) => {
  const env = setup(t); const {tween, cell} = env;
  tween.set(cell, 0, metricText);
  tween.set(cell, 100, metricText);
  env.step(125);
  assert.equal(cell.textContent, metricText(50));
  tween.set(cell, 23.9, metricText);
  assert.equal(cell.textContent, metricText(50));
  const pending = [...env.frames];
  tween.set(cell, 23.9, metricText);
  assert.deepEqual([...env.frames], pending);
  env.step(250);
  assert.equal(cell.textContent, metricText(36.95));
  env.step(375);
  assert.equal(cell.textContent, metricText(23.9));
  assert.equal(env.frames.size, 0);
  tween.set(cell, 23.9, () => assert.fail('same target must not format again'));
  assert.equal(env.frames.size, 0);
  assert.equal(env.reads, 1);
});

test('a frame timestamp older than the update stays at the displayed value', (t) => {
  const env = setup(t); const {tween, cell} = env;
  tween.set(cell, 0, metricText);
  env.step(100);
  tween.set(cell, 50, metricText);
  env.step(90);
  assert.equal(cell.textContent, metricText(0));
});

test('null transitions are immediate, including cancellation of a running number tween', (t) => {
  const env = setup(t); const {tween, cell} = env;
  for (const value of [null, 20, null, 40]) {
    tween.set(cell, value, metricText);
    assert.equal(cell.textContent, metricText(value));
    assert.equal(env.frames.size, 0);
  }
  tween.set(cell, 100, metricText);
  env.step(100);
  tween.set(cell, null, metricText);
  assert.equal(cell.textContent, metricText(null));
  assert.equal(env.frames.size, 0);
});

for (const boundary of ['hidden', 'reduce', 'abort', 'pagehide', 'removed']) {
  test(`number tween stops at ${boundary} without leaving scheduled frames`, (t) => {
    const env = setup(t); const {tween, cell} = env;
    tween.set(cell, 0, metricText); tween.set(cell, 100, metricText); env.step(100);
    if (boundary === 'hidden') { env.document.hidden = true; env.document.dispatchEvent(new Event('visibilitychange')); }
    if (boundary === 'reduce') { env.reduce(true); env.step(110); }
    if (boundary === 'abort') env.page.abort();
    if (boundary === 'pagehide') window.dispatchEvent(new Event('pagehide'));
    if (boundary === 'removed') { cell.remove(); env.step(110); }
    assert.equal(env.frames.size, 0);
    assert.equal(cell.textContent, metricText(100));
    env.step(1000);
    assert.equal(env.frames.size, 0);
  });
}

for (const boundary of ['hidden', 'reduce', 'abort', 'detached']) {
  test(`number tween writes directly when initially ${boundary}`, (t) => {
    const env = setup(t);
    env.tween.set(env.cell, 10, metricText);
    if (boundary === 'hidden') env.document.hidden = true;
    if (boundary === 'reduce') env.reduce(true);
    if (boundary === 'abort') env.page.abort();
    if (boundary === 'detached') env.cell.remove();
    env.tween.set(env.cell, 23.9, metricText);
    assert.equal(env.cell.textContent, metricText(23.9));
    assert.equal(env.frames.size, 0);
  });
}

test('200 nodes use one scheduled frame and one token read for 400 changing metrics', (t) => {
  const env = setup(t); const cells = Array.from({length: 400}, () => new TestElement('td'));
  env.body.append(...cells);
  for (const cell of cells) env.tween.set(cell, 0, metricText);
  for (let i = 0; i < 10; i++) for (const cell of cells) env.tween.set(cell, 100, metricText);
  assert.equal(env.frames.size, 1); assert.equal(env.reads, 1);
  env.step(125);
  assert.ok(cells.every((cell) => cell.textContent === metricText(50)));
  for (const cell of cells) env.tween.set(cell, 100, metricText);
  env.step(250);
  assert.ok(cells.every((cell) => cell.textContent === metricText(100)));
  assert.equal(env.frames.size, 0);
});

test('motion timing resolves seconds and milliseconds with existing easing', (t) => {
  const env = setup(t);
  assert.deepEqual(motionTiming('--motion-fast', '--ease-default'), {duration: 150, easing: 'ease'});
  globalThis.getComputedStyle = () => ({getPropertyValue: (key) => key === '--motion-fast' ? '150ms' : 'linear'});
  assert.deepEqual(motionTiming('--motion-fast', '--ease-linear'), {duration: 150, easing: 'linear'});
});
