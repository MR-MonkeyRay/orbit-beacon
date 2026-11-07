import test from 'node:test';
import assert from 'node:assert/strict';
import {translateHTML} from '../src/shared/translate-html.js';
import {statusLabels} from '../src/shared/status.js';
import {setLocalePreference} from '../src/console/i18n.js';

const element = (attribute, key) => ({
  textContent: '', attributes: new Map([[attribute, key]]),
  getAttribute(name) { return this.attributes.get(name); },
  setAttribute(name, value) { this.attributes.set(name, value); }
});

test('one HTML translation pass handles text, ARIA labels, and meta content in both languages', () => {
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  let preference = 'en';
  Object.defineProperty(globalThis, 'localStorage', {configurable: true, value: {
    getItem: () => preference, setItem(_key, value) { preference = value; }
  }});
  try {
    for (const [locale, language, refresh, description] of [
      ['en', 'en', 'Refresh', 'Availability and resource usage of public nodes'],
      ['zh-CN', 'zh-CN', '刷新', '已公开节点的可用性与资源使用情况']
    ]) {
      setLocalePreference(locale);
      const text = element('data-i18n', 'common.refresh');
      const aria = element('data-i18n-aria-label', 'common.refresh');
      const meta = element('data-i18n-content', 'public.description');
      const document = {documentElement: {}, querySelectorAll: (selector) => ({
        '[data-i18n]': [text], '[data-i18n-aria-label]': [aria], '[data-i18n-content]': [meta]
      })[selector]};
      translateHTML(document);
      assert.equal(document.documentElement.lang, language);
      assert.equal(text.textContent, refresh);
      assert.equal(aria.attributes.get('aria-label'), refresh);
      assert.equal(meta.attributes.get('content'), description);
    }
    assert.equal(statusLabels().unknown, '未知');
    assert.equal(statusLabels({unknown: 'public.status.unknown'}).unknown, '未知');
  } finally {
    if (previous) Object.defineProperty(globalThis, 'localStorage', previous);
    else delete globalThis.localStorage;
  }
});
