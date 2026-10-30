import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {ICONS} from '../src/shared/icons.js';

const source = (path) => readFile(new URL(path, import.meta.url), 'utf8');

test('sidebar keeps the primary page order and gives notifications its own inbox icon', async () => {
  const html = await source('../src/console/index.html');
  const links = [...html.matchAll(/data-nav="([^"]+)"/g)].map((match) => match[1]);
  assert.deepEqual(links, ['/overview', '/nodes', '/alerts', '/settings']);
  assert.match(html, /data-nav="\/alerts"[^>]*>[^<]*(?:<(?!\/a>)[^>]*>[^<]*)*<use href="#i-bell"\/>/);
  assert.match(html, /id="notifications"[^>]*>[^<]*(?:<(?!\/button>)[^>]*>[^<]*)*<use href="#i-inbox"\/>/);
  assert.ok(ICONS.inbox);
  assert.notEqual(ICONS.inbox, ICONS.bell);
});

test('node details and enrollment tabs use distinct purpose-specific icons', async () => {
  const javascript = await source('../src/console/pages/node.js');
  assert.match(javascript, /id: 'details',[^\n]+icon: 'info'/);
  assert.match(javascript, /id: 'enrollment',[^\n]+icon: 'key'/);
});
