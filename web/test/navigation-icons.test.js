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

// 展开动画期间侧栏很窄：可折行的条目会临时变高，把底部图标顶起再落回；
// 按百分比定宽的收起按钮会在第一帧跳动、随后宽度先增后减。
test('bottom sidebar entries keep their height and the toggle keeps its size while the rail animates', async () => {
  const shell = await source('../src/console/styles/shell.css');
  const responsive = await source('../src/console/styles/responsive.css');
  const notifications = await source('../src/console/styles/notifications.css');
  assert.match(notifications, /\.nav \.notification-entry\s*\{[^}]*display: grid;[^}]*grid-template-columns: 18px minmax\(0,1fr\) auto/);
  assert.doesNotMatch(notifications + responsive, /notification-entry\s*\{[^}]*flex-wrap: wrap/);
  assert.doesNotMatch(responsive, /\.notification-freshness\s*\{[^}]*display: none/, 'the freshness row only fades, so the entry height never changes');
  assert.match(shell, /\.account > \.nav-toggle\s*\{[^}]*position: absolute;[^}]*right: 0/);
  assert.doesNotMatch(shell + responsive, /\.nav-toggle\s*\{[^}]*width: 100%/);
  assert.doesNotMatch(shell + responsive, /\.account > \.(user-menu|locale-toggle)[^{]*\{[^}]*max-width/, 'the hidden buttons keep their size and are clipped by the rail');
  assert.match(responsive, /\.account > \.nav-toggle\s*\{\s*transform: translateX\(calc\(var\(--control-h\) \/ 2 - 23px\)\)/);
});

// 页头固定在顶部（手机在顶栏之下），铺满的背景条由 .main 横向裁剪，不产生横向滚动；
// 手机节点页头不固定，但仍需定位，否则背景条的 100vmax 会以视口为包含块越出裁剪。
test('page heads stay pinned with a full-bleed divider that never widens the page', async () => {
  const components = await source('../src/console/styles/components.css');
  const shell = await source('../src/console/styles/shell.css');
  const responsive = await source('../src/console/styles/responsive.css');
  assert.match(components, /\.page-head\s*\{[^}]*position: sticky;[^}]*top: var\(--page-head-top\)/);
  assert.match(components, /\.page-head::before\s*\{[^}]*inset: 0 -100vmax;[^}]*border-bottom: 1px solid var\(--line\)/);
  assert.match(components, /html\s*\{\s*scroll-padding-top: calc\(var\(--page-head-top\) \+ var\(--page-head-h\)/);
  assert.match(shell, /\.main\s*\{[^}]*overflow-x: clip/);
  assert.doesNotMatch(shell, /\.main\s*\{[^}]*overflow-x: hidden/, 'a scroll container would stop the sticky head');
  assert.match(responsive, /--page-head-top: calc\(64px \+ env\(safe-area-inset-top\)\)/);
  assert.match(responsive, /\.page-head\.node-head\s*\{\s*position: relative;/);
});
