import assert from 'node:assert/strict';
import test from 'node:test';
import {USAGE_WARN, USAGE_BAD, usageLevel, usageMeter} from '../src/shared/usage.js';
import {installDOM} from './console-dom.js';

test('usage levels use strict 80 and 90 boundaries and ignore missing metrics', () => {
  assert.equal(USAGE_WARN, 80);
  assert.equal(USAGE_BAD, 90);
  for (const value of [null, undefined, NaN, Infinity, '95', 0, 80]) assert.equal(usageLevel(value), '');
  assert.equal(usageLevel(80.01), 'warn');
  assert.equal(usageLevel(90), 'warn');
  assert.equal(usageLevel(90.01), 'bad');
});

test('usage meter retains a text value and decorative, bounded bar through updates', () => {
  const dom = installDOM();
  try {
    const meter = usageMeter(0);
    const track = meter.firstElementChild;
    const value = meter.lastElementChild;
    assert.equal(track.getAttribute('aria-hidden'), 'true');
    for (const [percent, width, level, text] of [[0, '0%', '', '0%'], [85, '85%', 'warn', '85%'], [95, '95%', 'bad', '95%'], [null, '0%', '', '—'], [120, '100%', 'bad', '120%']]) {
      meter.update(percent);
      assert.equal(track.firstElementChild.style.width, width);
      assert.equal(value.textContent, text);
      assert.equal(meter.className, `usage-meter${level ? ` usage-${level}` : ''}`);
      assert.equal(meter.lastElementChild, value);
      assert.equal(value.getAttribute('aria-hidden'), null);
    }
  } finally { dom.restore(); }
});
