import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

const css = readFileSync(new URL('../src/shared/tokens.css', import.meta.url), 'utf8');

function declarations(selector) {
  const pattern = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replaceAll('\\{', '\\s*\\{\\s*');
  const start = css.search(new RegExp(`${pattern}\\s*\\{`));
  assert.notEqual(start, -1, `missing ${selector}`);
  return css.slice(start, css.indexOf('}', start));
}

function token(block, name) {
  const value = block.match(new RegExp(`--${name}:\\s*(#[0-9a-f]{3,6})`, 'i'))?.[1];
  assert.ok(value, `missing --${name}`);
  return value;
}

function rgb(hex) {
  const value = hex.length === 4 ? hex.replace(/./g, (digit, index) => index ? digit.repeat(2) : digit) : hex;
  return [1, 3, 5].map((index) => Number.parseInt(value.slice(index, index + 2), 16));
}

function luminance(hex) {
  const [red, green, blue] = rgb(hex).map((value) => value / 255)
    .map((value) => value <= .04045 ? value / 12.92 : ((value + .055) / 1.055) ** 2.4);
  return .2126 * red + .7152 * green + .0722 * blue;
}

function contrast(foreground, background) {
  const values = [luminance(foreground), luminance(background)].sort((a, b) => b - a);
  return (values[0] + .05) / (values[1] + .05);
}

function mixWithBlack(hex, amount) {
  return `#${rgb(hex).map((value) => Math.round(value * amount).toString(16).padStart(2, '0')).join('')}`;
}

const light = declarations(':root');
const dark = declarations(':root[data-theme="dark"]');

test('light focus and normal text colors meet WCAG contrast thresholds', () => {
  const bg = token(light, 'bg');
  const panel = token(light, 'panel');
  assert.ok(contrast(token(light, 'focus'), bg) >= 3);
  assert.ok(contrast(token(light, 'focus'), panel) >= 3);
  assert.ok(contrast(token(light, 'ink-3'), bg) >= 4.5);
  assert.ok(contrast(token(light, 'ok'), token(light, 'ok-bg')) >= 4.5);
  assert.ok(contrast(token(light, 'bad'), token(light, 'bad-bg')) >= 4.5);
  assert.ok(contrast(token(light, 'off'), token(light, 'off-bg')) >= 4.5);
});

test('dark danger button text meets normal-text contrast', () => {
  const text = token(dark, 'bad-ink');
  const fill = token(dark, 'bad');
  assert.ok(contrast(text, fill) >= 4.5);
  assert.ok(contrast(text, mixWithBlack(fill, .88)) >= 4.5);
});

test('chart series have graphical contrast against both theme surfaces', () => {
  for (const theme of [light, dark]) {
    for (const color of ['orbit', 'ok']) {
      for (const surface of ['panel', 'panel-2']) assert.ok(contrast(token(theme, color), token(theme, surface)) >= 3);
    }
  }
});

// Safari 16.4 不支持 light-dark()，暗色色板写了两遍：一遍用于
// 系统偏好，一遍用于显式切换。两者须保持一致。
test('system and explicit dark palettes declare the same tokens', () => {
  const system = declarations('@media (prefers-color-scheme:dark){:root:not([data-theme="light"])');
  const normalize = (block) => block.slice(block.lastIndexOf('{') + 1).split(';').map((item) => item.trim()).filter(Boolean).sort();
  assert.deepEqual(normalize(system), normalize(dark));
});
