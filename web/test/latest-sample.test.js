import test from 'node:test';
import assert from 'node:assert/strict';
import {highestDisk, networkRate, usedPercent} from '../src/console/lib/latest-sample-helpers.js';

test('highest disk selects usage, not capacity, with stable ties', () => {
  const disks = [{path: '/', used_bytes: 50, total_bytes: 100}, {path: '/data', used_bytes: 9, total_bytes: 10}, {path: '/tie', used_bytes: 90, total_bytes: 100}];
  assert.equal(highestDisk(disks), disks[1]);
  assert.equal(highestDisk([{used_bytes: null, total_bytes: 100}]), null);
  assert.equal(highestDisk([]), null);
  assert.equal(usedPercent(0, 0), 0);
  assert.equal(usedPercent(101, 100), null);
});
test('network rates total each direction and keep missing fields unavailable', () => {
  const networks = [{rx_bytes_per_sec: 1024, tx_bytes_per_sec: 512}, {rx_bytes_per_sec: 2048, tx_bytes_per_sec: 256}];
  assert.equal(networkRate(networks, 'rx_bytes_per_sec'), 3072);
  assert.equal(networkRate(networks, 'tx_bytes_per_sec'), 768);
  assert.equal(networkRate([], 'rx_bytes_per_sec'), null);
  networks[1].rx_bytes_per_sec = null;
  assert.equal(networkRate(networks, 'rx_bytes_per_sec'), null);
  assert.equal(networkRate(networks, 'tx_bytes_per_sec'), 768);
});
