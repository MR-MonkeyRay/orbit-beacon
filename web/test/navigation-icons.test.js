import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {ICONS} from '../src/shared/icons.js';

const source = (path) => readFile(new URL(path, import.meta.url), 'utf8');

test('sidebar keeps the primary page order and gives notifications its own inbox icon', async () => {
  const html = await source('../src/console/index.html');
  const links = [...html.matchAll(/data-nav="([^"]+)"/g)].map((match) => match[1]);
  assert.deepEqual(links, ['/overview', '/nodes', '/services', '/alerts', '/settings']);
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
  for (const name of ['languages', 'sidebar-collapse', 'sidebar-expand']) assert.ok(ICONS[name]);
  assert.notEqual(ICONS.inbox, ICONS.bell);
});

test('command palette sidebar button is not treated as a notification counter', async () => {
  const html = await source('../src/console/index.html');
  const button = html.match(/<button[^>]+id="command-palette"[^>]*>/)?.[0];
  assert.ok(button);
  assert.doesNotMatch(button, /notification-entry/);
  assert.match(button, /aria-haspopup="dialog"/);
});

test('sidebar rail keeps item geometry, clips labels instead of moving them, and keeps only the expand button', async () => {
  const shell = await source('../src/console/styles/shell.css');
  const responsive = await source('../src/console/styles/responsive.css');
  assert.match(shell, /\.alert-count\s*\{[^}]*color: var\(--warn\)/);
  assert.match(shell, /\.alert-count\.critical\s*\{[^}]*color: var\(--bad\)/);
  assert.match(shell, /dialog\.command-dialog\s*\{[^}]*margin: 15vh auto auto/);
  assert.match(shell, /@media \(pointer: coarse\)\s*\{\s*\.command-shortcut\s*\{ display: none/);
  assert.match(responsive, /html:is\(\.nav-compact, \.nav-collapsed\):not\(\.nav-peek\)/);
  assert.match(responsive, /grid-template-columns: var\(--rail-w\) minmax\(0,1fr\)/);
  assert.match(shell, /--rail-w:72px/);
  assert.match(shell, /\.shell\s*\{[^}]*transition: grid-template-columns/);
  assert.match(shell, /\.sidebar\s*\{[^}]*overflow-x: hidden/);
  assert.doesNotMatch(responsive, /\.nav-label\s*\{[^}]*position: absolute/, 'labels never leave the rail as positioned tooltips');
  assert.match(responsive, /\.account > \.user-menu,\.account > \.locale-toggle\s*\{[^}]*visibility: hidden/);
  assert.doesNotMatch(shell + responsive, /view-transition/);
});
