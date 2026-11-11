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
  assert.match(html, /<h2[^>]*id="nav-monitoring"/);
  assert.match(html, /role="group" aria-labelledby="nav-monitoring"/);
  assert.match(html, /<h2[^>]*id="nav-management"/);
  assert.ok(html.indexOf('nav-monitoring') < html.indexOf('data-nav="/overview"'));
  assert.ok(html.indexOf('data-nav="/alerts"') < html.indexOf('nav-management'));
  assert.ok(html.indexOf('data-nav="/settings"') < html.indexOf('id="command-palette"'));
  assert.ok(html.indexOf('id="command-palette"') < html.indexOf('id="notifications"'));
  assert.ok(html.indexOf('id="notifications"') < html.indexOf('class="nav-public"'));
  assert.match(html, /<kbd class="command-shortcut">Ctrl K<\/kbd>/);
  assert.ok(ICONS.inbox);
  assert.notEqual(ICONS.inbox, ICONS.bell);
});

test('command palette sidebar button is not treated as a notification counter', async () => {
  const html = await source('../src/console/index.html');
  const button = html.match(/<button[^>]+id="command-palette"[^>]*>/)?.[0];
  assert.ok(button);
  assert.doesNotMatch(button, /notification-entry/);
  assert.match(button, /aria-haspopup="dialog"/);
});

test('sidebar responsive rules hide headings and shortcuts in the rail, touch hides shortcuts, and search anchors at 15vh', async () => {
  const shell = await source('../src/console/styles/shell.css');
  const responsive = await source('../src/console/styles/responsive.css');
  assert.match(shell, /\.alert-count\s*\{[^}]*color: var\(--warn\)/);
  assert.match(shell, /\.alert-count\.critical\s*\{[^}]*color: var\(--bad\)/);
  assert.match(shell, /dialog\.command-dialog\s*\{[^}]*margin: 15vh auto auto/);
  assert.match(shell, /@media \(pointer: coarse\)\s*\{\s*\.command-shortcut\s*\{ display: none/);
  assert.match(responsive, /@media \(min-width:768px\) and \(max-width:1199px\)\s*\{\s*\.nav-heading,\.command-shortcut,\.user-menu \.who-name\s*\{ display: none/);
});
