import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {activeAlertsSummary} from '../src/console/pages/alert-events.js';
import {copy, installDOM} from './console-dom.js';

let attentionPanel, groupsPanel, resourcesPanel;
const setup = async () => {
  const dom = installDOM();
  const window = globalThis.window;
  globalThis.window ||= new EventTarget();
  ({attentionPanel, groupsPanel, resourcesPanel} = await import('../src/console/pages/overview.js'));
  return {restore() { dom.restore(); if (window === undefined) delete globalThis.window; else globalThis.window = window; }};
};

const node = (id, extra = {}) => ({id: String(id), name: `Node ${id}`, presence: 'online', stopped: false, group_id: null, ...extra});

test('attention uses UTC inclusive expiry boundaries, existing presence reasons, stable links, and a single healthy empty line', async () => {
  const dom = await setup();
  try {
    const now = Date.parse('2026-11-10T00:00:00Z');
    const nodes = [node(1, {presence: 'offline'}), node(2, {presence: 'never'}), node(3, {expires_on: '2026-11-16'}),
      node(4, {expires_on: '2026-11-10'}), node(5, {expires_on: '2026-11-09'}), node(6, {expires_on: '2026-11-17'}), node(7), node(8, {presence: 'unknown'})];
    const panel = attentionPanel([]); panel.snapshot(nodes, now);
    const rows = panel.querySelectorAll('li');
    assert.equal(rows.length, 6);
    assert.match(rows[0].textContent, /Offline/);
    assert.ok(rows[1].textContent.includes(copy('common.status.never')));
    assert.match(rows[2].textContent, /Expires in 7 days/);
    assert.match(rows[3].textContent, /Expires in 1 day/);
    assert.match(rows[4].textContent, /Expired/);
    rows.forEach((row, i) => assert.equal(row.querySelector('a').getAttribute('href'), `/nodes/${[1, 2, 3, 4, 5, 8][i]}`));
    assert.ok(rows[5].textContent.includes(copy('common.status.unknown')));
    panel.snapshot(nodes, now + 1000);
    assert.deepEqual(panel.querySelectorAll('li'), rows);
    panel.snapshot([node(1), node(7)], now);
    assert.equal(panel.querySelectorAll('li').length, 0);
    // 无需关注时面板隐藏，状态行显示在节点总览里。
    assert.equal(panel.hidden, true);
    assert.equal(panel.healthy.hidden, false);
    assert.equal(panel.healthy.textContent, copy('nodes.allHealthy'));
    assert.equal(panel.healthy.querySelector('a').getAttribute('href'), '/nodes');
    panel.snapshot([node(1, {presence: 'offline', expires_on: '2026-11-12'})], now);
    assert.match(panel.querySelector('li').textContent, /Offline.*Expires in 3 days/);
    assert.equal(panel.hidden, false); assert.equal(panel.healthy.hidden, true);
    panel.snapshot(Array.from({length: 12}, (_, i) => node(i, {presence: 'offline'})), now);
    assert.equal(panel.querySelectorAll('li').length, 8);
  } finally { dom.restore(); }
});

test('resource lists independently exclude offline/missing values, cap at five, and show only a dash when empty', async () => {
  const dom = await setup();
  try {
    const panel = resourcesPanel([]);
    const columns = panel.querySelector('.resource-columns').children;
    for (const column of columns) { assert.equal(column.querySelector('.text-note').textContent, '—'); assert.equal(column.querySelector('ul').hidden, true); }
    const nodes = Array.from({length: 8}, (_, i) => node(i, {cpu_percent: i * 10, memory_used_percent: 99 - i}));
    nodes[7].presence = 'offline'; nodes[6].cpu_percent = null;
    panel.snapshot(nodes);
    const [cpu, memory] = panel.querySelectorAll('ul');
    assert.deepEqual(cpu.querySelectorAll('a').map((a) => a.textContent), ['Node 5', 'Node 4', 'Node 3', 'Node 2', 'Node 1']);
    assert.equal(memory.querySelectorAll('li').length, 5);
    assert.ok(memory.querySelector('.usage-bad'));
    assert.equal(memory.querySelector('a').textContent, 'Node 0');
    panel.snapshot([node(0, {cpu_percent: 0, memory_used_percent: null})]);
    assert.equal(cpu.querySelector('.usage-value').textContent, '0%');
    assert.equal(memory.hidden, true);
  } finally { dom.restore(); }
});

test('group tiles hide zero groups, color each status like the fleet bar, and keep text counts for AT', async () => {
  const dom = await setup();
  try {
    const groups = [{id: 'a', name: 'A'}, {id: 'b', name: 'B'}];
    const nodes = [node(1, {group_id: 'a'}), node(2, {group_id: 'a', presence: 'offline'}), node(3, {group_id: 'a', presence: 'disabled'}), node(4, {group_id: 'a', stopped: true})];
    const panel = groupsPanel(nodes, groups);
    assert.equal(panel.querySelectorAll('.group-tile').length, 1);
    const tile = panel.querySelector('.group-tile');
    assert.equal(tile.getAttribute('href'), '/nodes?group=a');
    assert.equal(tile.querySelector('.group-name').textContent, 'A');
    assert.equal(tile.querySelector('.num').textContent, `4 · Online: 1, Offline: 1, Disabled: 1, and Stopped: 1`);
    const bar = tile.querySelector('.group-bar');
    assert.equal(bar.getAttribute('aria-hidden'), 'true');
    assert.equal(tile.querySelector('.group-meta').getAttribute('aria-hidden'), 'true');
    assert.deepEqual(bar.children.map((segment) => segment.className), ['c-online', 'c-offline', 'c-disabled', 'c-disabled']);
    assert.deepEqual(bar.children.map((segment) => segment.style.flex), ['1', '1', '1', '1']);
    assert.deepEqual(tile.querySelector('.group-meta').children.map((item) => item.querySelector('b').textContent), ['1', '1', '1', '1']);
    // 实时帧只修补数值：卡片元素保持不变，焦点不会丢失。
    panel.snapshot([node(1, {group_id: 'a'}), node(2, {group_id: 'a'})]);
    assert.equal(panel.querySelector('.group-tile'), tile);
    assert.equal(tile.querySelector('.num').textContent, `2 · Online: 2`);
    assert.deepEqual(bar.children.map((segment) => segment.style.flex), ['2']);
    panel.snapshot([node(1)]);
    assert.equal(panel.querySelectorAll('.group-tile').length, 1);
    assert.match(panel.querySelector('.group-tile').className, /ungrouped/);
    assert.match(panel.querySelector('.group-tile').textContent, /Ungrouped/);
    panel.snapshot([]); assert.equal(panel.hidden, true);
  } finally { dom.restore(); }
});

test('alert severity count rows skip zero slots and CSS keeps counts left aligned at compact widths', async () => {
  const dom = installDOM();
  try {
    const panel = activeAlertsSummary({firing: {total: 3, warning: 2, critical: 0, info: 1, unknown: 0}, newest: []});
    const counts = panel.querySelector('.alert-counts');
    assert.equal(counts.children.length, 2);
    assert.equal(counts.querySelector('.alert-critical'), null);
    assert.equal(counts.querySelector('.alert-unknown'), null);
    const css = await readFile(new URL('../src/console/styles/alerts.css', import.meta.url), 'utf8');
    assert.match(css, /\.alert-counts\s*\{[^}]*justify-content: flex-start/);
    for (const file of ['shell', 'responsive']) {
      const css = await readFile(new URL(`../src/console/styles/${file}.css`, import.meta.url), 'utf8');
      assert.doesNotMatch(css, /(?:^|\n)\s*\.alert-count\s*\{/);
    }
    const pages = await readFile(new URL('../src/console/styles/pages.css', import.meta.url), 'utf8');
    assert.match(pages, /\.overview-bottom\s*\{align-items:stretch;/);
  } finally { dom.restore(); }
});
