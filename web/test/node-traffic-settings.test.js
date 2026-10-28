import test from 'node:test';
import assert from 'node:assert/strict';

import {installDOM} from './console-dom.js';

test('quota conversion uses exact decimal BigInt arithmetic and uint64 bounds', async (t) => {
  const dom = installDOM();
  t.after(() => dom.restore());
  const {quotaToBytes} = await import('../src/console/pages/node-traffic-settings.js');
  assert.deepEqual(quotaToBytes('9', 'GB'), {ok: true, value: '9000000000'});
  assert.deepEqual(quotaToBytes('0', 'TB'), {ok: true, value: '0'});
  assert.equal(quotaToBytes('1', 'KB').value, '1000');
  assert.equal(quotaToBytes('1', 'MB').value, '1000000');
  assert.equal(quotaToBytes('1', 'TB').value, '1000000000000');
  assert.equal(quotaToBytes('18446744073709551', 'KB').ok, true);
  assert.equal(quotaToBytes('18446744073709552', 'KB').ok, false);
  assert.equal(quotaToBytes('9007199254740993', 'bytes').value, '9007199254740993');
  assert.equal(quotaToBytes(Number.MAX_SAFE_INTEGER + 1, 'bytes').ok, false);
  assert.equal(quotaToBytes('01', 'bytes').ok, true);
  assert.equal(quotaToBytes('1', 'constructor').ok, false);
  for (const value of ['', '-1', '1.5', '1e3', null, undefined]) assert.equal(quotaToBytes(value, 'bytes').ok, false);
});

