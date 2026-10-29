import test from 'node:test';
import assert from 'node:assert/strict';

const i18n = await import('../src/console/i18n.js');

test('locale resolution respects language priority and Simplified Chinese fallback', () => {
  for (const [preference, languages, expected] of [
    ['system', ['zh-CN', 'en-US'], 'zh-CN'], ['system', ['en-GB', 'zh-CN'], 'en'],
    ['system', ['fr-FR', 'zh-TW'], 'zh-CN'], ['system', ['fr-FR'], 'en'],
    ['zh-CN', ['en-US'], 'zh-CN'], ['en', ['zh-CN'], 'en'], ['invalid', ['zh-HK'], 'zh-CN'],
    ['system', [], 'en'], ['system', undefined, 'en']
  ]) assert.equal(i18n.resolveLocale(preference, languages), expected);
});

test('locale preference is explicit, strict, and storage failures do not change active preference', () => {
  const originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  const originalNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  let stored = 'invalid';
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {languages: ['zh-TW', 'en'], language: 'en-US'}});
  Object.defineProperty(globalThis, 'localStorage', {configurable: true, value: {getItem: () => stored, setItem(_key, value) { stored = value; }}});
  try {
    assert.equal(i18n.getLocalePreference(), 'system');
    assert.equal(i18n.getLocale(), 'zh-CN');
    i18n.setLocalePreference('en');
    assert.equal(stored, 'en');
    assert.equal(i18n.getLocale(), 'en');
    assert.throws(() => i18n.setLocalePreference('invalid'));
    globalThis.localStorage.setItem = () => { throw new Error('blocked'); };
    assert.throws(() => i18n.setLocalePreference('zh-CN'));
    assert.equal(i18n.getLocalePreference(), 'en');
    globalThis.localStorage.getItem = () => { throw new Error('blocked'); };
    assert.equal(i18n.getLocalePreference(), 'system');
  } finally {
    if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage); else delete globalThis.localStorage;
    if (originalNavigator) Object.defineProperty(globalThis, 'navigator', originalNavigator); else delete globalThis.navigator;
  }
});

test('translation interpolates named values as plain text and selects English plurals', () => {
  const originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  let preference = 'en';
  Object.defineProperty(globalThis, 'localStorage', {configurable: true, value: {getItem: () => preference, setItem(_key, value) { preference = value; }}});
  try {
    assert.equal(i18n.t('alerts.value.failingProbes', {count: 0}), '0 failing probes');
    assert.equal(i18n.t('alerts.value.failingProbes', {count: 1}), '1 failing probe');
    assert.equal(i18n.t('alerts.value.failingProbes', {count: 2}), '2 failing probes');
    assert.equal(i18n.t('nodes.selectNode', {name: '<script>"user"</script>'}), 'Select <script>"user"</script>');
    assert.throws(() => i18n.t('missing.key'));
    i18n.setLocalePreference('zh-CN');
    assert.equal(i18n.t('alerts.value.failingProbes', {count: 2}), '2 个探测失败');
  } finally { if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage); else delete globalThis.localStorage; }
});

test('an active document retains one locale until its explicit change and reload', () => {
  const originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  const originalNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  const originalWindow = Object.getOwnPropertyDescriptor(globalThis, 'window');
  let stored = 'en';
  const target = new EventTarget();
  Object.defineProperty(globalThis, 'window', {configurable: true, value: target});
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {languages: ['en-US']}});
  Object.defineProperty(globalThis, 'localStorage', {configurable: true, value: {getItem: () => stored, setItem(_key, value) { stored = value; }}});
  try {
    assert.equal(i18n.getLocale(), 'en');
    stored = 'zh-CN';
    target.dispatchEvent(Object.assign(new Event('storage'), {key: 'orbitbeacon-locale'}));
    assert.equal(i18n.getLocalePreference(), 'zh-CN');
    assert.equal(i18n.getLocale(), 'en');
    assert.equal(i18n.t('common.none'), 'None');
    i18n.setLocalePreference('zh-CN');
    assert.equal(i18n.getLocale(), 'zh-CN');
  } finally {
    for (const [key, original] of [['localStorage', originalStorage], ['navigator', originalNavigator], ['window', originalWindow]]) {
      if (original) Object.defineProperty(globalThis, key, original); else delete globalThis[key];
    }
  }
});
