import test from 'node:test';
import assert from 'node:assert/strict';
import {elements, installDOM} from './console-dom.js';

test('facts preserves repeated labels and h child semantics for arrays, Nodes and false values', async () => {
  const dom = installDOM();
  try {
    const {facts} = await import('../src/console/ui/components.js');
    const node = document.createElement('strong');
    node.textContent = 'node';
    const root = facts([['Same', 'first'], ['Same', ['array ', [false, null, ['nested ', node]]]],
      ['False', false], ['Empty array', []], ['Null', null], ['Empty', '']]);
    const rows = root.children;
    assert.equal(rows.length, 6);
    assert.deepEqual(rows.map((row) => row.children[0].textContent), ['Same', 'Same', 'False', 'Empty array', 'Null', 'Empty']);
    assert.equal(rows[0].children[1].textContent, 'first');
    assert.equal(rows[1].children[1].textContent, 'array nested node');
    assert.equal(rows[1].children[1].children[2], node);
    assert.equal(rows[2].children[1].textContent, '');
    assert.equal(rows[2].children[1].className, '');
    assert.equal(rows[3].children[1].textContent, '');
    assert.equal(rows[3].children[1].className, '');
    assert.equal(rows[4].children[1].className, 'none');
    assert.equal(rows[5].children[1].className, 'none');
  } finally { dom.restore(); }
});

test('readonly facts updates repeated labels by occurrence without replacing stable Nodes', async () => {
  const dom = installDOM();
  try {
    const {readonlyFacts} = await import('../src/console/ui/components.js');
    const node = document.createElement('strong');
    node.textContent = 'stable';
    const values = readonlyFacts([['Same', 'one'], ['Same', node]]);
    values.update([['Same', 'two'], ['Same', node]]);
    assert.equal(values.element.children.length, 2);
    assert.equal(values.element.children[0].children[1].textContent, 'two');
    assert.equal(values.element.children[1].children[1].firstElementChild, node);
  } finally { dom.restore(); }
});

test('status badge creation and patching use the same labels', async () => {
  const dom = installDOM();
  try {
    const {patchStatusBadge, statusBadge} = await import('../src/console/ui/components.js');
    const badge = statusBadge('online');
    const label = badge.textContent;
    badge.textContent = 'stale label';
    patchStatusBadge(badge, 'online');
    assert.equal(badge.className, 'badge online');
    assert.equal(badge.textContent, label);
    patchStatusBadge(badge, 'unrecognized');
    assert.equal(badge.className, 'badge unrecognized');
    assert.equal(badge.textContent, 'unrecognized');
  } finally { dom.restore(); }
});

test('live status distinguishes an initial connection from reconnection after live data', async () => {
  const dom = installDOM();
  try {
    const {liveStatus} = await import('../src/console/ui/components.js');
    const status = liveStatus();
    assert.equal(status.element.textContent, 'Connecting…');
    status.update({state: 'stale'});
    status.update({state: 'connecting'});
    assert.equal(status.element.textContent, 'Connecting…');
    status.update({state: 'live'});
    assert.equal(status.element.textContent, 'Live');
    status.update({state: 'stale'});
    assert.equal(status.element.textContent, 'Stale');
    status.update({state: 'connecting'});
    assert.equal(status.element.textContent, 'Reconnecting…');
  } finally { dom.restore(); }
});

test('data table preserves wrapper properties, class, colgroup and table structure', async () => {
  const dom = installDOM();
  try {
    const {dataTable} = await import('../src/console/ui/components.js');
    const colgroup = document.createElement('colgroup');
    const body = document.createElement('tbody');
    const wrapper = dataTable({caption: 'Items', columns: ['Name', 'State'], body,
      className: 'items-table', colgroup, hidden: true, 'aria-busy': 'true'});
    const table = wrapper.children[0];
    assert.equal(wrapper.className, 'table-wrap');
    assert.equal(wrapper.hidden, true);
    assert.equal(wrapper.getAttribute('aria-busy'), 'true');
    assert.equal(table.className, 'table items-table');
    assert.deepEqual(table.children.map((child) => child.tagName), ['caption', 'colgroup', 'thead', 'tbody']);
    assert.equal(table.children[0].className, 'sr');
    assert.equal(table.children[0].textContent, 'Items');
    assert.deepEqual(elements(table.children[2]).filter((element) => element.tagName === 'th').map((header) => [header.getAttribute('scope'), header.textContent]), [['col', 'Name'], ['col', 'State']]);
    assert.equal(table.children[3], body);
  } finally { dom.restore(); }
});
