import test from 'node:test';
import assert from 'node:assert/strict';
import * as i18n from '../src/shared/i18n.js';
import en from '../src/shared/locales/en.js';
import zhCN from '../src/shared/locales/zh-CN.js';
import {installDOM} from './console-dom.js';
import {h} from '../src/shared/dom.js';
import {activeAlertsSummary} from '../src/console/pages/alert-events.js';

test('translated parts preserve live elements, translation order, and literal user content', () => {
  const dom = installDOM();
  const source = h('a', {href: '/nodes/one'}, '<script>{time}</script>');
  const time = h('time', {datetime: '2026-10-29T12:00:00Z'}, 'today');
  try {
    i18n.setCatalogs({en: {
      summary: '{source} · {time}',
      reordered: '{time}：{source}',
      count: {one: '{count} item: {source}', other: '{count} items: {source}'}
    }});
    i18n.setLocalePreference('en');
    const output = h('p', null, i18n.tParts('summary', {source, time}));
    assert.equal(output.textContent, '<script>{time}</script> · today');
    assert.equal(output.children[0], source);
    assert.equal(output.children[2], time);
    const reordered = h('p', null, i18n.tParts('reordered', {source, time}));
    assert.equal(reordered.children[0], time);
    assert.equal(reordered.children[2], source);
    assert.equal(reordered.textContent, 'today：<script>{time}</script>');
    assert.equal(h('p', null, i18n.tParts('count', {count: 1, source})).textContent, '1 item: <script>{time}</script>');
    assert.equal(h('p', null, i18n.tParts('count', {count: 2, source})).textContent, '2 items: <script>{time}</script>');
    assert.throws(() => i18n.tParts('summary', {source}), /Missing translation parameter time/);
    assert.throws(() => i18n.tParts('missing'), /Unknown translation key/);
  } finally {
    i18n.setCatalogs({en, 'zh-CN': zhCN});
    dom.restore();
  }
});

test('alert summaries keep their node link and timestamp in both languages', () => {
  const dom = installDOM();
  try {
    for (const locale of ['en', 'zh-CN']) {
      i18n.setLocalePreference(locale);
      const summary = activeAlertsSummary({firing: {total: 1, critical: 1}, newest: [{
        id: 'event', node_id: 'node', node_name: '<node>', rule_name: 'CPU', metric: 'cpu_percent',
        severity: 'critical', value: 90, started_at: '2026-10-29T12:00:00Z'
      }]});
      const details = summary.querySelector('.sub');
      assert.equal(details.querySelector('a').getAttribute('href'), '/nodes/node');
      assert.equal(details.querySelector('a').textContent, '<node>');
      assert.equal(details.querySelector('time').getAttribute('datetime'), '2026-10-29T12:00:00Z');
      assert.match(details.textContent, /<node> · 90% · /);
      assert.doesNotMatch(details.textContent, /\[object|\{(?:node|value|time)\}/);
    }
  } finally { dom.restore(); }
});
