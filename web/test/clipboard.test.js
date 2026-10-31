import assert from 'node:assert/strict';
import test from 'node:test';
import {copyElementText} from '../src/shared/clipboard.js';

const replaceGlobal = (name, value) => Object.defineProperty(globalThis, name, {value, writable: true, configurable: true});

test('copyElementText uses the Clipboard API when available', async () => {
  const previousNavigator = globalThis.navigator;
  let value;
  replaceGlobal('navigator', {clipboard: {writeText: async (text) => { value = text; }}});
  try {
    assert.equal(await copyElementText({textContent: 'orbit'}), 'clipboard');
    assert.equal(value, 'orbit');
  } finally { replaceGlobal('navigator', previousNavigator); }
});

test('copyElementText falls back to selection and execCommand', async () => {
  const previous = {navigator: globalThis.navigator, document: globalThis.document, getSelection: globalThis.getSelection};
  let selected;
  replaceGlobal('navigator', {});
  replaceGlobal('document', {
    createRange: () => ({selectNodeContents: (element) => { selected = element; }}),
    execCommand: (command) => command === 'copy'
  });
  replaceGlobal('getSelection', () => ({removeAllRanges() {}, addRange() {}}));
  const element = {textContent: 'beacon'};
  try {
    assert.equal(await copyElementText(element), 'fallback');
    assert.equal(selected, element);
  } finally {
    replaceGlobal('navigator', previous.navigator);
    replaceGlobal('document', previous.document);
    replaceGlobal('getSelection', previous.getSelection);
  }
});

test('copyElementText reports failed fallback', async () => {
  const previous = {navigator: globalThis.navigator, document: globalThis.document, getSelection: globalThis.getSelection};
  replaceGlobal('navigator', {});
  replaceGlobal('document', {createRange: () => ({selectNodeContents() {}}), execCommand: () => false});
  replaceGlobal('getSelection', () => ({removeAllRanges() {}, addRange() {}}));
  try { assert.equal(await copyElementText({textContent: 'manual'}), false); }
  finally {
    replaceGlobal('navigator', previous.navigator);
    replaceGlobal('document', previous.document);
    replaceGlobal('getSelection', previous.getSelection);
  }
});
