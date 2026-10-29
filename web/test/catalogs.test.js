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
      assert.match(key, /^(common|controls|public|errors)\./);
    }
    assert.ok(keys(small).length < keys(full).length / 4);
  }
  for (const key of ['shell.currentIncorrect', 'shell.passwordUnchanged', 'shell.passwordRateLimited',
    'shell.notificationsOfflineLoad', 'errors.locale_save_failed']) assert.ok(!Object.hasOwn(en, key), key);
  assert.equal(en['nodes.details'], 'Details');
  assert.equal(zhCN['nodes.details'], '详情');
});

test('public entry bundles only its synchronous catalogs', async () => {
  const result = await build({
    absWorkingDir: fileURLToPath(new URL('..', import.meta.url)),
    entryPoints: ['src/public/main.js'], bundle: true, write: false,
    outdir: '/tmp/orbitbeacon-public-catalog-test', metafile: true, logLevel: 'silent'
  });
  const inputs = Object.keys(result.metafile.inputs);
  assert.ok(inputs.includes('src/shared/locales/public-catalog.en.js'));
  assert.ok(inputs.includes('src/shared/locales/public-catalog.zh-CN.js'));
  assert.ok(!inputs.some((name) => name.startsWith('src/console/')));
  assert.ok(!inputs.some((name) => /locales\/(?:en|zh-CN|shell\..*|nodes\..*|settings\..*|account\..*|audit\..*|notifications\..*|data\..*|alerts\..*)\.js$/.test(name)));
});
