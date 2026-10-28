import test from 'node:test';
import assert from 'node:assert/strict';

import {diskSummary, memorySummary, networkSummary} from '../src/console/pages/latest-sample-helpers.js';

test('latest sample summaries format complete metric families', () => {
  assert.equal(memorySummary({memory_total_bytes: 4096, memory_available_bytes: 1024}), '3 KiB / 4 KiB (75%)');
  assert.equal(diskSummary([{path: '/', total_bytes: 4096, used_bytes: 1024}]), '/: 1 KiB / 4 KiB (25%)');
  assert.equal(networkSummary([{name: 'eth0', rx_bytes_per_sec: 1536, tx_bytes_per_sec: 1024}]), 'eth0: receive 1.5 KiB/s, send 1 KiB/s');
});

test('latest sample summaries handle missing metrics gracefully', () => {
  assert.equal(memorySummary({memory_total_bytes: null, memory_available_bytes: null}), 'Unavailable');
  assert.equal(diskSummary([]), 'Unavailable');
  assert.equal(networkSummary(null), 'Unavailable');
});
