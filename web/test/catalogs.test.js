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

test('catalog placeholders match and UI copy follows the glossary', () => {
  const forms = (value) => typeof value === 'string' ? [value] : Object.values(value);
  const placeholders = (value) => [...new Set(forms(value).flatMap((text) =>
    [...text.matchAll(/\{([A-Za-z][A-Za-z0-9_]*)\}/g)].map((match) => match[1])))].sort();
  for (const [key, value] of Object.entries(en)) {
    assert.deepEqual(placeholders(value), placeholders(zhCN[key]), key);
    for (const text of forms(value)) {
      assert.doesNotMatch(text, /public (?:status )?page/i, key);
      // 安装说明中的文件名保留原始拼写。
      assert.doesNotMatch(text, /(?<![-/])\bagents?\b(?![-.])/, key);
    }
    for (const text of forms(zhCN[key])) {
      assert.doesNotMatch(text, /服务端|控制平面|管理控制台|公开页|公开状态页|公开节点页|接入票据|通知渠道|还原/, key);
    }
  }
  assert.equal(en['public.heading'], 'Node status');
  assert.equal(zhCN['public.heading'], '节点状态');
  assert.equal(en['settings.publicPage'], 'Status page');
  assert.equal(zhCN['settings.publicPage'], '状态页');
  assert.equal(en['nodes.publicPage'], 'Status page listing');
  assert.equal(zhCN['nodes.publicPage'], '状态页展示');
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
