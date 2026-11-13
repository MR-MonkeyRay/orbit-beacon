import assert from 'node:assert/strict';
import test from 'node:test';
import {nodeViewMenu, patchNodeColumns, readNodeColumns} from '../src/console/lib/nodes-columns.js';
import {copy, installDOM} from './console-dom.js';

test('view menu persists optional columns, keeps the URL alone, and dismisses on Escape and outside pointer', () => {
  const dom = installDOM();
  const signal = new AbortController();
  try {
    const stored = new Map();
    globalThis.localStorage = {getItem: (key) => stored.get(key) ?? null, setItem: (key, value) => stored.set(key, value)};
    const columns = readNodeColumns();
    assert.deepEqual([...columns], ['last_seen', 'group', 'location', 'tags']);
    const table = document.createElement('table');
    for (const key of ['agent_version', 'created', 'last_seen']) {
      const cell = document.createElement('td'); cell.dataset.column = key; table.append(cell);
    }
    const picker = nodeViewMenu({columns, signal: signal.signal, onChange: () => patchNodeColumns(table, columns)});
    document.body.append(picker, table);
    patchNodeColumns(table, columns);
    assert.equal(table.children[0].classList.contains('column-hidden'), true);
    assert.equal(table.children[2].classList.contains('column-hidden'), false);
    const trigger = picker.querySelector('button');
    assert.equal(trigger.textContent, copy('nodes.view'));
    assert.equal(picker.querySelector('.view-columns').firstElementChild.textContent, copy('nodes.columns'));
    trigger.click();
    assert.equal(trigger.getAttribute('aria-expanded'), 'true');
    const choices = picker.querySelectorAll('input');
    assert.equal(choices.length, 6);
    choices[0].checked = true; choices[0].dispatchEvent(new Event('change'));
    assert.equal(table.children[0].classList.contains('column-hidden'), false);
    assert.equal(readNodeColumns().has('agent_version'), true);
    for (const checkbox of choices) { checkbox.checked = false; checkbox.dispatchEvent(new Event('change')); }
    assert.deepEqual([...readNodeColumns()], []);
    assert.equal(location.search, '');
    const escape = Object.assign(new Event('keydown', {cancelable: true}), {key: 'Escape'});
    picker.dispatchEvent(escape);
    assert.equal(trigger.getAttribute('aria-expanded'), 'false');
    assert.equal(document.activeElement, trigger);
    trigger.click();
    document.dispatchEvent(new Event('pointerdown'));
    assert.equal(trigger.getAttribute('aria-expanded'), 'false');
    trigger.click(); signal.abort();
    assert.equal(trigger.getAttribute('aria-expanded'), 'false');
  } finally { signal.abort(); dom.restore(); }
});

test('column preferences tolerate malformed, unknown and blocked storage', () => {
  const dom = installDOM();
  const signal = new AbortController();
  try {
    for (const value of ['invalid', '{}', 'null']) {
      localStorage.getItem = () => value;
      assert.equal(readNodeColumns().has('last_seen'), true);
    }
    localStorage.getItem = () => '["created","unknown"]';
    assert.deepEqual([...readNodeColumns()], ['created']);
    localStorage.getItem = () => { throw Error('blocked'); };
    localStorage.setItem = () => { throw Error('blocked'); };
    const columns = readNodeColumns();
    const picker = nodeViewMenu({columns, signal: signal.signal, onChange() {}});
    const checkbox = picker.querySelector('input');
    checkbox.checked = true;
    assert.doesNotThrow(() => checkbox.dispatchEvent(new Event('change')));
    assert.ok(columns.has('agent_version'));
  } finally { signal.abort(); dom.restore(); }
});

test('view menu sort control writes the key with its default direction and toggles direction', () => {
  const dom = installDOM();
  const signal = new AbortController();
  try {
    const state = {sort: 'status', dir: 'asc'};
    const writes = [];
    const menu = nodeViewMenu({columns: new Set(), signal: signal.signal, onChange() {}, sort: {
      keys: [['status', 'Status'], ['name', 'Name'], ['created', 'Added']],
      read: () => state,
      write: (sort, dir) => { writes.push([sort, dir]); state.sort = sort; state.dir = dir || (sort === 'created' ? 'desc' : 'asc'); menu.syncSort(); }
    }});
    const key = menu.querySelector('select');
    assert.equal(key.value, 'status');
    const directions = menu.querySelector('.view-direction').querySelectorAll('button');
    assert.deepEqual(directions.map((control) => control.getAttribute('aria-pressed')), ['true', 'false']);
    key.value = 'created'; key.dispatchEvent(new Event('change'));
    assert.deepEqual(writes.at(-1), ['created', '']);
    assert.deepEqual(directions.map((control) => control.getAttribute('aria-pressed')), ['false', 'true']);
    directions[0].click();
    assert.deepEqual(writes.at(-1), ['created', 'asc']);
    assert.equal(directions[0].getAttribute('aria-pressed'), 'true');
  } finally { signal.abort(); dom.restore(); }
});
