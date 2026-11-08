import test from 'node:test';
import assert from 'node:assert/strict';
import {build} from 'esbuild';
import {fileURLToPath} from 'node:url';
import en from '../src/shared/locales/en.js';
import zhCN from '../src/shared/locales/zh-CN.js';
import publicEn from '../src/shared/locales/public-catalog.en.js';
import publicZhCN from '../src/shared/locales/public-catalog.zh-CN.js';

const keys = (catalog) => Object.keys(catalog).sort();

test('full and public catalog sets have matching English and Chinese keys', () => {
  assert.deepEqual(keys(en), keys(zhCN));
  assert.deepEqual(keys(publicEn), keys(publicZhCN));
  for (const [small, full] of [[publicEn, en], [publicZhCN, zhCN]]) {
    for (const [key, value] of Object.entries(small)) {
      assert.deepEqual(value, full[key], key);
      assert.match(key, /^(common|controls|public|errors)\.|^alerts\.severity\.(info|warning|critical)$/);
    }
    assert.ok(keys(small).length < keys(full).length / 4);
  }
});

test('catalog placeholders match across English and Chinese', () => {
  const forms = (value) => typeof value === 'string' ? [value] : Object.values(value);
  const placeholders = (value) => [...new Set(forms(value).flatMap((text) =>
    [...text.matchAll(/\{([A-Za-z][A-Za-z0-9_]*)\}/g)].map((match) => match[1])))].sort();
  for (const [key, value] of Object.entries(en)) {
    assert.deepEqual(placeholders(value), placeholders(zhCN[key]), key);
  }
});

for (const theme of ['default', 'example']) test(`${theme} public entry bundles only its synchronous catalogs`, async () => {
  const result = await build({
    absWorkingDir: fileURLToPath(new URL('..', import.meta.url)),
    entryPoints: [`themes/${theme}/main.js`], bundle: true, write: false,
    outdir: '/tmp/orbitbeacon-public-catalog-test', metafile: true, logLevel: 'silent'
  });
  const inputs = Object.keys(result.metafile.inputs);
  assert.ok(inputs.includes('src/shared/locales/public-catalog.en.js'));
  assert.ok(inputs.includes('src/shared/locales/public-catalog.zh-CN.js'));
  assert.ok(!inputs.some((name) => name.startsWith('src/console/')));
  assert.ok(!inputs.some((name) => /locales\/(?:en|zh-CN|shell\..*|nodes\..*|settings\..*|account\..*|audit\..*|notifications\..*|data\..*|alerts\..*)\.js$/.test(name)));
});
