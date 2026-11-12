import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {copy, elements, installDOM} from './console-dom.js';

const setup = async () => {
  const dom = installDOM();
  const window = globalThis.window;
  globalThis.window ||= new EventTarget();
  const [{overviewPlaceholder}, {nodesPlaceholder}, {nodePlaceholder}, {servicesPlaceholder}, components] = await Promise.all([
    import('../src/console/pages/overview.js'), import('../src/console/pages/nodes.js'), import('../src/console/pages/node.js'),
    import('../src/console/pages/services.js'), import('../src/console/ui/components.js')]);
  return {overviewPlaceholder, nodesPlaceholder, nodePlaceholder, servicesPlaceholder, ...components,
    restore() { dom.restore(); if (window === undefined) delete globalThis.window; else globalThis.window = window; }};
};

const focusable = (root) => elements(root).filter((element) => ['a', 'button', 'input', 'select', 'textarea'].includes(element.tagName)
  || element.getAttribute('tabindex') !== null);

test('metric page placeholders mirror the page title, stay busy, hold no controls and leave stable IDs untouched', async () => {
  const env = await setup();
  try {
    const cases = [[env.overviewPlaceholder, copy('nodes.overview')], [env.nodesPlaceholder, copy('nodes.listTitle')],
      [env.servicesPlaceholder, copy('services.title')], [env.nodePlaceholder, '']];
    for (const [build, title] of cases) {
      env.resetIDs();
      const placeholder = build();
      assert.equal(placeholder.getAttribute('aria-busy'), 'true');
      assert.equal(placeholder.getAttribute('aria-label'), copy('controls.loading'));
      assert.equal(placeholder.querySelector('h1').textContent, title, 'known titles render as text; the node name waits for data');
      assert.deepEqual(focusable(placeholder), [], 'placeholders never take focus or clicks');
      assert.equal(env.uid('probe'), 'probe-1', 'placeholders do not consume the page ID sequence');
    }
  } finally { env.restore(); }
});

test('the node placeholder marks the hash-selected tab and only draws charts for the monitoring tab', async () => {
  const env = await setup();
  try {
    location.hash = '#traffic';
    let placeholder = env.nodePlaceholder();
    const selected = placeholder.querySelectorAll('.tab-placeholder').filter((tab) => tab.dataset.selected === 'true');
    assert.deepEqual(selected.map((tab) => tab.textContent), [copy('nodes.traffic')]);
    assert.equal(placeholder.querySelectorAll('.sk-chart').length, 0);
    location.hash = '';
    placeholder = env.nodePlaceholder();
    assert.equal(placeholder.querySelectorAll('.tab-placeholder').find((tab) => tab.dataset.selected === 'true').textContent, copy('common.monitoring'));
    assert.equal(placeholder.querySelectorAll('.sk-chart').length, 2);
  } finally { env.restore(); }
});

test('metric routes register their placeholders', async () => {
  const main = await readFile(new URL('../src/console/main.js', import.meta.url), 'utf8');
  assert.match(main, /const placeholders = \{services: servicesPlaceholder, overview: overviewPlaceholder, nodes: nodesPlaceholder, node: nodePlaceholder\}/);
  assert.match(main, /placeholder: placeholders\[route\.page\]/);
});
