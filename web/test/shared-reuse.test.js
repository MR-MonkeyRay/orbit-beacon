import test from 'node:test';
import assert from 'node:assert/strict';

test('shared UUID validation preserves general and RFC detail-link checks', async () => {
  const {uuid, isDetailUUID} = await import('../src/shared/uuid.js');
  assert.equal(uuid.test('00000000-0000-0000-0000-000000000001'), true);
  assert.equal(uuid.test('00000000-0000-4000-8000-000000000001'), true);
  assert.equal(uuid.test('not-a-uuid'), false);
  assert.equal(isDetailUUID('00000000-0000-4000-8000-000000000001'), true);
  assert.equal(isDetailUUID('00000000-0000-0000-0000-000000000001'), false);
  assert.equal(isDetailUUID('00000000-0000-4000-0000-000000000001'), false);
});

test('shared load-number formatting keeps the public and console precision', async () => {
  const {formatLoadNumber} = await import('../src/shared/format.js');
  assert.equal(formatLoadNumber(0), '0.00');
  assert.equal(formatLoadNumber(.125), '0.13');
  assert.equal(formatLoadNumber(12.345), '12.35');
});

test('shared CSS uses four semantic interaction speeds and resolves token references in both entry points', async () => {
  const {readFileSync, readdirSync} = await import('node:fs');
  const {fileURLToPath} = await import('node:url');
  const {join} = await import('node:path');
  const root = fileURLToPath(new URL('../src/', import.meta.url));
  const styles = ['shared/tokens.css', 'shared/availability.css', 'public/public.css',
    ...readdirSync(join(root, 'console/styles')).map((name) => `console/styles/${name}`)];
  const css = styles.map((name) => readFileSync(join(root, name), 'utf8')).join('\n');
  const tokens = readFileSync(join(root, 'shared/tokens.css'), 'utf8');
  assert.deepEqual([...tokens.matchAll(/(--motion-[\w-]+):([^;]+);/g)].map((match) => match.slice(1)), [
    ['--motion-instant', '.1s'], ['--motion-fast', '.15s'], ['--motion-normal', '.25s'], ['--motion-slow', '.35s']
  ]);
  assert.doesNotMatch(css, /--duration-\d+\b/);
  const defined = new Set([...css.matchAll(/(--[\w-]+)\s*:/g)].map((match) => match[1]));
  // --refresh-duration is set by the public page's dynamic stylesheet.
  defined.add('--refresh-duration');
  for (const [, token, fallback] of css.matchAll(/var\((--[\w-]+)(\s*,)?/g)) {
    assert.ok(defined.has(token) || fallback, `undefined ${token}`);
  }
  assert.match(tokens, /--duration-reduced-motion:\.01ms;/);
  assert.match(tokens, /animation: shimmer 1\.4s/);
  assert.match(css, /animation: live-pulse 4s/);
});

test('consolidated CSS keeps native checkbox focus and stack spacing without obsolete variants', async () => {
  const {readFileSync} = await import('node:fs');
  const read = (path) => readFileSync(new URL(`../src/console/styles/${path}.css`, import.meta.url), 'utf8');
  const forms = read('forms');
  const components = read('components');
  const css = ['forms', 'components', 'pages', 'data-operations', 'notifications'].map(read).join('\n');
  assert.match(forms, /\.check-row:has\(input:focus-visible\)\s*\{\s*outline: 2px solid var\(--focus\);/);
  assert.match(forms, /\.check-row input\s*\{[^}]*width: 18px;[^}]*height: 18px;/);
  assert.doesNotMatch(forms, /\.check-row input\s*\{[^}]*(?:appearance|opacity|outline):/);
  assert.match(components, /\.panel\+\.panel,\.stack>\*\+\*\s*\{\s*margin-top: 18px;/);
  assert.doesNotMatch(css, /\.(?:hint|lede|note|data-check|data-load-note|data-secondary|data-empty|notification-empty|notification-status|notification-read-status|notification-source-status|notification-error|notification-filters|administrator-protection)(?![\w-])/);
});
