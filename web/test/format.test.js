import test from 'node:test';
import assert from 'node:assert/strict';
import {copy} from './console-dom.js';

import {formatBytes, formatDuration, formatLoad, formatPercent, formatRate, relative, shortID} from '../src/console/format.js';

test('formatBytes formats decimal byte strings with binary units', () => {
  assert.equal(formatBytes('0'), '0 B');
  assert.equal(formatBytes('1'), '1 B');
  assert.equal(formatBytes('1023'), '1023 B');
  assert.equal(formatBytes('1024'), '1 KiB');
  assert.equal(formatBytes('1536'), '1.5 KiB');
  assert.equal(formatBytes('1048576'), '1 MiB');
  assert.equal(formatBytes('18446744073709551615'), '15.9 EiB');
  assert.equal(formatBytes('0001024'), '1 KiB');
  assert.equal(formatBytes('1099511627776'), '1 TiB');
  assert.equal(formatBytes('1125899906842624'), '1 PiB');
  assert.equal(formatBytes('1152921504606846976'), '1 EiB');
  assert.equal(formatBytes('1180591620717411303424'), '1024 EiB');
  assert.equal(formatBytes('1126'), '1 KiB');
  assert.equal(formatBytes('1127'), '1.1 KiB');
  assert.equal(formatBytes('-1'), '-1 B');
});

test('formatBytes returns invalid input unchanged', () => {
  assert.equal(formatBytes('1.5'), '1.5');
  assert.equal(formatBytes('not bytes'), 'not bytes');
  assert.equal(formatBytes(null), null);
  assert.equal(formatBytes(undefined), undefined);
});

test('latest sample values have compact human-readable formats', () => {
  assert.equal(formatPercent(7.25), '7.3%');
  assert.equal(formatPercent(null), copy('common.unavailable'));
  assert.equal(formatDuration(183845), copy('nodes.duration.days', {days: 2, hours: 3}));
  assert.equal(formatDuration(65), copy('nodes.duration.minutes', {minutes: 1}));
  assert.equal(formatRate(1536), '1.5 KiB/s');
  assert.equal(formatRate(null), copy('common.unavailable'));
});

test('load, percent and byte rates format history values and unavailable samples', () => {
  assert.equal(formatLoad(.125), '0.13');
  assert.equal(formatLoad(0), '0.00');
  assert.equal(formatLoad(12.345), '12.35');
  for (const value of [null, undefined, NaN, Infinity, -1]) {
    assert.equal(formatLoad(value), copy('common.unavailable'));
    assert.equal(formatRate(value), copy('common.unavailable'));
  }
  assert.equal(formatRate(0), '0 B/s');
  assert.equal(formatRate(1024 * 1024), '1 MiB/s');
  assert.equal(formatRate(.6), '1 B/s');
  assert.equal(formatPercent(0), '0%');
  assert.equal(formatPercent(100), '100%');
  assert.equal(formatPercent(Infinity), copy('common.unavailable'));
});



test('shortID truncates identifiers safely', () => {
  assert.equal(shortID('123456789'), '12345678');
  assert.equal(shortID('short'), 'short');
  assert.equal(shortID(''), '');
  assert.equal(shortID(null), '');
  assert.equal(shortID(undefined), '');
});

test('relative chooses seconds, minutes, hours, days, and absolute dates', () => {
  const originalNow = Date.now;
  Date.now = () => Date.parse('2026-10-27T12:00:00Z');
  try {
    assert.equal(relative(new Date('2026-10-27T11:59:30Z')), copy('nodes.justNow'));
    assert.equal(relative(new Date('2026-10-27T11:59:00Z')), '1 minute ago');
    assert.equal(relative(new Date('2026-10-27T10:00:00Z')), '2 hours ago');
    assert.equal(relative(new Date('2026-10-25T12:00:00Z')), '2 days ago');
    assert.equal(relative(new Date('2026-10-27T12:05:00Z')), 'in 5 minutes');
    assert.equal(relative(new Date('2026-10-28T12:00:00Z')), 'tomorrow');
    assert.equal(relative(new Date(Date.now() - 44_000)), copy('nodes.justNow'));
    assert.equal(relative(new Date(Date.now() - 45_000)), '1 minute ago');
    assert.equal(relative(new Date(Date.now() - 3600_000)), '1 hour ago');
    assert.equal(relative(new Date(Date.now() - 86400_000)), 'yesterday');
    assert.match(relative(new Date('2026-08-01T12:00:00Z')), /Aug 1, 2026/);
  } finally {
    Date.now = originalNow;
  }
});
