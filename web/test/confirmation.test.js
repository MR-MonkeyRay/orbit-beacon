import test from 'node:test';
import assert from 'node:assert/strict';
import {matchesConfirmation} from '../src/console/ui/confirmation-match.js';

test('destructive confirmation requires the exact name or phrase', () => {
  assert.equal(matchesConfirmation('edge-1', 'edge-1'), true);
  assert.equal(matchesConfirmation('Edge-1', 'edge-1'), false);
  assert.equal(matchesConfirmation('edge-1 ', 'edge-1'), false);
  assert.equal(matchesConfirmation('', 'edge-1'), false);
  assert.equal(matchesConfirmation('delete 2 nodes', 'delete 2 nodes'), true);
  assert.equal(matchesConfirmation('delete 3 nodes', 'delete 2 nodes'), false);
});
