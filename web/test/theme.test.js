import test from 'node:test';
import assert from 'node:assert/strict';

test('storage events synchronize themes and button labels without writing back', async () => {
  const writes = [];
  const attributes = new Map();
  const element = new EventTarget();
  element.replaceChildren = () => {};
  element.setAttribute = (key, value) => attributes.set(key, value);
  element.getAttribute = (key) => attributes.get(key);
  globalThis.window = new EventTarget();
  globalThis.document = {
    documentElement: {dataset: {}},
    createElementNS: () => ({setAttribute() {}, append() {}})
  };
  globalThis.localStorage = {
    setItem: (key, value) => writes.push([key, value]),
    removeItem: (key) => writes.push([key, null])
  };
  try {
    const {currentTheme, setTheme, themeButton} = await import('../src/shared/theme.js');
    themeButton(element);
    const systemLabel = attributes.get('aria-label');
    const storage = (key, newValue) => window.dispatchEvent(Object.assign(new Event('storage'), {key, newValue}));
    storage('orbitbeacon-theme', 'dark');
    assert.equal(currentTheme(), 'dark');
    const darkLabel = attributes.get('aria-label');
    assert.notEqual(darkLabel, systemLabel);
    assert.equal(element.title, darkLabel);
    storage('unrelated', 'light');
    assert.equal(currentTheme(), 'dark');
    storage('orbitbeacon-theme', 'light');
    assert.equal(currentTheme(), 'light');
    storage('orbitbeacon-theme', null);
    assert.equal(currentTheme(), 'system');
    storage('orbitbeacon-theme', 'invalid');
    assert.equal(currentTheme(), 'system');
    assert.deepEqual(writes, []);
    setTheme('dark');
    assert.equal(attributes.get('aria-label'), darkLabel);
    setTheme('system');
    assert.deepEqual(writes, [['orbitbeacon-theme', 'dark'], ['orbitbeacon-theme', null]]);
    localStorage.setItem = () => { throw new Error('Storage unavailable'); };
    setTheme('light');
    assert.equal(currentTheme(), 'light');
  } finally {
    delete globalThis.window;
    delete globalThis.document;
    delete globalThis.localStorage;
  }
});
