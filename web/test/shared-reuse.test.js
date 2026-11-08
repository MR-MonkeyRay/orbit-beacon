import test from 'node:test';
import assert from 'node:assert/strict';

test('shared UUID validation preserves general and RFC detail-link checks', async () => {
  const {uuid, isDetailUUID} = await import('../src/shared/uuid.js');
  assert.equal(uuid.test('00000000-0000-0000-0000-000000000001'), true);
  assert.equal(uuid.test('00000000-0000-4000-8000-000000000001'), true);
  assert.equal(uuid.test('not-a-uuid'), false);
  assert.equal(isDetailUUID('00000000-0000-4000-8000-000000000001'), true);
  assert.equal(isDetailUUID('00000000-0000-0000-0000-000000000001'), false);
  assert.equal(isDetailUUID('00000000-0000-4000-0000-000000000001'), false);
});

test('shared load-number formatting keeps the public and console precision', async () => {
  const {formatLoadNumber} = await import('../src/shared/format.js');
  assert.equal(formatLoadNumber(0), '0.00');
  assert.equal(formatLoadNumber(.125), '0.13');
  assert.equal(formatLoadNumber(12.345), '12.35');
});
