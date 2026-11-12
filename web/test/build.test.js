import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp, mkdir, readFile, rm, writeFile} from 'node:fs/promises';
import fs from 'node:fs/promises';
import {syncBuiltinESMExports} from 'node:module';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {buildWeb} from '../build.mjs';

// 用隔离源码实际构建，验证缓存 URL 与内容的对应关系。
async function fixture(t) {
  const root = await mkdtemp(path.join(tmpdir(), 'orbitbeacon-build-test-'));
  t.after(() => rm(root, {recursive: true, force: true}));
  const files = {
    'build.config.json': JSON.stringify({publicTheme: 'default', assetBase: ''}),
    'src/console/main.js': "import './style.css'; console.log('console');",
    'src/console/style.css': 'body { color: black; }',
    'src/console/index.html': '<!--theme--><!--icons--><link rel="stylesheet" href="%STYLE%"><script type="module" src="%SCRIPT%"></script>',
    'themes/default/main.js': "import './style.css'; console.log('default');",
    'themes/default/style.css': 'body { color: blue; background-image: url(./mark.svg); }',
    'themes/default/mark.svg': '<svg xmlns="http://www.w3.org/2000/svg"><circle r="4"/></svg>',
    'themes/default/index.html': '<!--theme--><!--icons--><link rel="stylesheet" href="%STYLE%"><script type="module" src="%SCRIPT%"></script><main>default</main>',
    'themes/example/main.js': "import './style.css'; console.log('example');",
    'themes/example/style.css': 'body { color: green; }',
    'themes/example/index.html': '<!--theme--><!--icons--><link rel="stylesheet" href="%STYLE%"><script type="module" src="%SCRIPT%"></script><main>example</main>'
  };
  for (const [name, content] of Object.entries(files)) {
    await mkdir(path.dirname(path.join(root, name)), {recursive: true});
    await writeFile(path.join(root, name), content);
  }
  const outDir = path.join(root, 'dist');
  const build = async (config) => {
    if (config) await writeFile(path.join(root, 'build.config.json'), JSON.stringify(config));
    await buildWeb(outDir, root);
    return JSON.parse(await readFile(path.join(outDir, 'asset-manifest.json'), 'utf8'));
  };
  return {root, outDir, build};
}

test('content edits change version URLs, not filenames, and theme selection leaves console bytes unchanged', async (t) => {
  const {root, outDir, build} = await fixture(t);
  const initial = await build();
  const consoleJS = await readFile(path.join(outDir, '_app/assets/console.js'));
  const consoleCSS = await readFile(path.join(outDir, '_app/assets/console.css'));
  const initialHTML = await readFile(path.join(outDir, 'public.html'), 'utf8');
  assert.ok(initialHTML.includes(`/_app/assets/public.js?v=${initial.versions['_app/assets/public.js']}`));
  await writeFile(path.join(root, 'themes/default/main.js'), "import './style.css'; console.log('changed');");
  const changed = await build();
  assert.deepEqual(Object.keys(changed.versions).sort(), Object.keys(initial.versions).sort());
  assert.notEqual(changed.versions['_app/assets/public.js'], initial.versions['_app/assets/public.js']);
  const selected = await build({publicTheme: 'example', assetBase: 'https://static.example.test/release-1'});
  assert.equal(selected.asset_base, 'https://static.example.test/release-1');
  assert.deepEqual(await readFile(path.join(outDir, '_app/assets/console.js')), consoleJS);
  assert.deepEqual(await readFile(path.join(outDir, '_app/assets/console.css')), consoleCSS);
  const html = await readFile(path.join(outDir, 'public.html'), 'utf8');
  assert.ok(html.includes(`https://static.example.test/release-1/_app/assets/public.js?v=${selected.versions['_app/assets/public.js']}`));
  assert.match(html, /<main>example<\/main>/);
});

test('CDN paths remain literal URLs when embedded in HTML attributes', async (t) => {
  const {outDir, build} = await fixture(t);
  const base = 'https://cdn.example.test/&copy/$&/$$';
  const manifest = await build({publicTheme: 'default', assetBase: base});
  assert.equal(manifest.asset_base, base);
  const html = await readFile(path.join(outDir, 'public.html'), 'utf8');
  const escapedBase = 'https://cdn.example.test/&amp;copy/$&amp;/$$';
  assert.ok(html.includes(`href="${escapedBase}/_app/assets/public.css?v=${manifest.versions['_app/assets/public.css']}"`));
  assert.ok(html.includes(`src="${escapedBase}/_app/assets/public.js?v=${manifest.versions['_app/assets/public.js']}"`));
});

test('imported theme media keeps its path and invalidates the referencing CSS when content changes', async (t) => {
  const {root, outDir, build} = await fixture(t);
  const initial = await build();
  const media = Object.keys(initial.versions).find((name) => name.endsWith('/mark.svg'));
  assert.ok(media);
  const css = await readFile(path.join(outDir, '_app/assets/public.css'), 'utf8');
  assert.ok(css.includes(`/${media}?v=${initial.versions[media]}`));
  await writeFile(path.join(root, 'themes/default/mark.svg'), '<svg xmlns="http://www.w3.org/2000/svg"><circle r="8"/></svg>');
  const changed = await build();
  assert.notEqual(changed.versions[media], initial.versions[media]);
  assert.notEqual(changed.versions['_app/assets/public.css'], initial.versions['_app/assets/public.css']);
});

test('media fragments and query parameters retain their meaning and follow content versions', async (t) => {
  const {root, outDir, build} = await fixture(t);
  await writeFile(path.join(root, 'themes/default/style.css'), 'body { background-image: url(./mark.svg#icon); }');
  await writeFile(path.join(root, 'themes/default/main.js'), "import './style.css'; import icon from './mark.svg?download=1#icon'; console.log(icon);");
  const initial = await build();
  const media = Object.keys(initial.versions).find((name) => name.endsWith('/mark.svg'));
  const css = await readFile(path.join(outDir, '_app/assets/public.css'), 'utf8');
  const js = await readFile(path.join(outDir, '_app/assets/public.js'), 'utf8');
  assert.ok(css.includes(`/${media}?v=${initial.versions[media]}#icon`));
  assert.ok(js.includes(`/${media}?download=1&v=${initial.versions[media]}#icon`));
  await writeFile(path.join(root, 'themes/default/mark.svg'), '<svg xmlns="http://www.w3.org/2000/svg"><circle r="8"/></svg>');
  const changed = await build();
  assert.notEqual(changed.versions['_app/assets/public.css'], initial.versions['_app/assets/public.css']);
  assert.notEqual(changed.versions['_app/assets/public.js'], initial.versions['_app/assets/public.js']);
});

test('failed output writes preserve the last complete distribution', async (t) => {
  const {outDir, build} = await fixture(t);
  await build();
  const original = await readFile(path.join(outDir, 'public.html'));
  const originalScript = await readFile(path.join(outDir, '_app/assets/public.js'));
  const write = fs.writeFile;
  const stub = t.mock.method(fs, 'writeFile', async (name, ...args) => {
    if (String(name).endsWith('/public.js')) throw Object.assign(new Error('disk full'), {code: 'ENOSPC'});
    return write(name, ...args);
  });
  syncBuiltinESMExports();
  try {
    await assert.rejects(build(), {code: 'ENOSPC'});
    assert.deepEqual(await readFile(path.join(outDir, 'public.html')), original);
    assert.deepEqual(await readFile(path.join(outDir, '_app/assets/public.js')), originalScript);
  } finally {
    stub.mock.restore();
    syncBuiltinESMExports();
  }
});

test('failed publication rolls back and an interrupted swap recovers before rebuilding', async (t) => {
  const {outDir, build} = await fixture(t);
  await build();
  const original = await readFile(path.join(outDir, 'public.html'));
  const rename = fs.rename;
  const stub = t.mock.method(fs, 'rename', async (from, to) => {
    if (String(from).startsWith(`${outDir}.next-`)) throw Object.assign(new Error('publication denied'), {code: 'EACCES'});
    return rename(from, to);
  });
  syncBuiltinESMExports();
  try {
    await assert.rejects(build(), {code: 'EACCES'});
    assert.deepEqual(await readFile(path.join(outDir, 'public.html')), original);
  } finally {
    stub.mock.restore();
    syncBuiltinESMExports();
  }
  await rename(outDir, `${outDir}.previous`);
  await assert.rejects(build({publicTheme: 'missing', assetBase: ''}));
  assert.deepEqual(await readFile(path.join(outDir, 'public.html')), original);
});

test('invalid theme paths and unsafe CDN bases fail before replacing existing output', async (t) => {
  const {outDir, build} = await fixture(t);
  await build();
  const original = await readFile(path.join(outDir, 'public.html'));
  for (const config of [
    {publicTheme: '../console', assetBase: ''},
    {publicTheme: 'missing', assetBase: ''},
    ...['http://cdn.test', '//cdn.test', 'https://u:p@cdn.test', 'https://cdn.test/?x=y', 'https://cdn.test/#x', 'https://cdn.test/";script-src *', 'https://cdn.test/\u0001', 'https://cdn.test/\u0085', 'https://bad_host.test', 'https://cdn.test/a/../b', 'https://cdn.test/releases/{42}', 'https://cdn.test/releases/^42'].map((assetBase) => ({publicTheme: 'default', assetBase}))
  ]) {
    await assert.rejects(build(config));
    assert.deepEqual(await readFile(path.join(outDir, 'public.html')), original);
  }
});


for (const theme of ['default', 'example']) test(`${theme} actual theme builds compact banner, shared threshold colors and regional search styles`, async (t) => {
  const root = path.resolve(new URL('..', import.meta.url).pathname);
  const out = await mkdtemp(path.join(tmpdir(), 'orbitbeacon-theme-ux-'));
  t.after(() => rm(out, {recursive: true, force: true}));
  // Override only the config read; source files and all renderer imports remain real.
  const original = fs.readFile;
  fs.readFile = async (name, ...args) => String(name) === path.join(root, 'build.config.json')
    ? JSON.stringify({publicTheme: theme, assetBase: ''}) : original(name, ...args);
  syncBuiltinESMExports();
  try { await buildWeb(out, root); }
  finally { fs.readFile = original; syncBuiltinESMExports(); }
  const css = await readFile(path.join(out, '_app/assets/public.css'), 'utf8');
  const js = await readFile(path.join(out, '_app/assets/public.js'), 'utf8');
  assert.match(css, /min-height:160px/);
  assert.match(css, /summary\[data-tone=ok\]/);
  assert.match(css, /\.gauge\.usage-warn \.fill/);
  assert.match(css, /\.gauge\.usage-bad \.fill/);
  assert.match(css, /\.region-heading/);
  assert.match(css, /\.node-search/);
  assert.match(css, /safe-area-inset-top/);
  assert.match(css, /safe-area-inset-bottom/);
  assert.match(css, /min-height:100vh;min-height:100dvh/);
  assert.match(css, /#empty\{flex:1;align-content:center;border:0\}/);
  const html = await readFile(path.join(out, 'public.html'), 'utf8');
  // 默认主题：标题、摘要、刷新信息成一列；example 保留旧的 .hero-row 结构，默认样式两者都支持。
  if (theme === 'default') assert.match(html, /class="hero hero-split"[\s\S]*?<h1 id="headline"[\s\S]*?id="summary"[\s\S]*?<div class="live">/);
  else assert.match(html, /class="hero-row">\s*<h1 id="headline"[\s\S]*?<div class="live">/);
  assert.match(css, /\.hero-row\{/);
  assert.match(html, /<a id="sign-in" class="login" href="\/login"/);
  assert.match(html, /<div id="services" hidden><\/div>/);
  assert.match(css, /\.account-chip\{/);
  assert.match(html, /id="refresh"[^>]*aria-label="Refresh"[^>]*title="Refresh"/);
  assert.doesNotMatch(html, /<span data-i18n="common.refresh">/);
  assert.doesNotMatch(js, /Automatic updates|Language: \{preference\}/);
  assert.match(js, /Search this page/);
  assert.doesNotMatch(js, /Top resource usage/);
});
