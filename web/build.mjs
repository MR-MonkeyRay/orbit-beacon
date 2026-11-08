// 构建内嵌 UI：固定文件名，内容版本由构建器写入 URL 与清单。
import * as esbuild from 'esbuild';
import {createHash} from 'node:crypto';
import {mkdtemp, mkdir, readdir, readFile, rename, rm, writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {consoleRoutes} from './src/console/routes.js';
import {ICONS} from './src/shared/icons.js';
import {themeBootstrap} from './theme-bootstrap.mjs';

const root = path.dirname(fileURLToPath(import.meta.url));
const dist = path.resolve(root, '../internal/server/webui/dist');
const version = (data) => createHash('sha256').update(data).digest('hex').slice(0, 32);
const mediaExtensions = ['.svg', '.png', '.jpg', '.jpeg', '.gif', '.webp', '.avif', '.ico', '.woff', '.woff2', '.ttf', '.otf'];

function assetBase(value) {
  if (typeof value !== 'string') throw new Error('assetBase must be a string');
  if (value === '') return '';
  if (/[^\x21-\x7e]|[\\%<>"'`;?#@]/.test(value) || !value.startsWith('https://')) throw new Error('assetBase must be a clean ASCII HTTPS URL');
  const url = new URL(value);
  const hostname = url.hostname;
  if (hostname.startsWith('[') ? hostname.startsWith('[::ffff:') :
      hostname.replace(/\.$/, '').length > 253 || hostname.replace(/\.$/, '').split('.').some((label) => !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label))) {
    throw new Error('assetBase must contain a valid hostname or IP address');
  }
  const prefix = value.slice(value.indexOf('/', 8));
  if (url.protocol !== 'https:' || url.username || url.password || url.search || url.hash || url.pathname.includes('%') ||
      (value.indexOf('/', 8) >= 0 && (prefix.includes('//') || prefix.split('/').some((part) => part === '.' || part === '..')))) {
    throw new Error('assetBase must contain only an HTTPS origin and optional path prefix');
  }
  return url.origin + url.pathname.replace(/\/$/, '');
}

function iconSprite() {
  const symbols = Object.entries(ICONS).map(([name, body]) => `<symbol id="i-${name}" viewBox="0 0 24 24">${body}</symbol>`).join('');
  return `<svg width="0" height="0" class="icon-sprite" aria-hidden="true">${symbols}</svg>`;
}

export async function buildWeb(outDir, sourceRoot = root) {
  const previous = `${outDir}.previous`;
  try {
    await rename(previous, outDir);
  } catch (error) {
    if (!['ENOENT', 'ENOTEMPTY', 'EEXIST'].includes(error.code)) throw error;
  }
  const config = JSON.parse(await readFile(path.join(sourceRoot, 'build.config.json'), 'utf8'));
  if (!config || typeof config !== 'object' || Array.isArray(config) ||
      Object.keys(config).some((key) => !['publicTheme', 'assetBase'].includes(key)) ||
      typeof config.publicTheme !== 'string' || !/^[a-z0-9][a-z0-9_-]*$/.test(config.publicTheme)) {
    throw new Error('build.config.json requires publicTheme (a theme directory name) and assetBase');
  }
  const base = assetBase(config.assetBase);
  const pages = [
    {entry: 'console', directory: 'src/console', output: '200.html'},
    {entry: 'public', directory: `themes/${config.publicTheme}`, output: 'public.html'}
  ];
  const templates = await Promise.all(pages.map((page) => readFile(path.join(sourceRoot, page.directory, 'index.html'), 'utf8')));
  for (const template of templates) {
    for (const token of ['<!--theme-->', '<!--icons-->', '%STYLE%', '%SCRIPT%']) {
      if (!template.includes(token)) throw new Error(`Theme template is missing ${token}`);
    }
  }
  const assets = path.join(outDir, '_app/assets');
  const result = await esbuild.build({
    absWorkingDir: sourceRoot,
    entryPoints: Object.fromEntries(pages.map((page) => [page.entry, `${page.directory}/main.js`])),
    bundle: true,
    minify: true,
    format: 'esm',
    target: ['chrome111', 'firefox115', 'safari16.4'],
    entryNames: '[name]',
    assetNames: 'media/[dir]/[name]',
    outbase: sourceRoot,
    outdir: assets,
    publicPath: `${base}/_app/assets`,
    loader: Object.fromEntries(mediaExtensions.map((extension) => [extension, 'file'])),
    plugins: [{
      name: 'versioned-media',
      setup(build) {
        build.onResolve({filter: /\.(svg|png|jpe?g|gif|webp|avif|ico|woff2?|ttf|otf)(?:[?#].*)?$/}, async (args) => {
          if (args.pluginData?.versioned || /^(?:https?:|data:|\/\/)/.test(args.path)) return;
          const resolved = await build.resolve(args.path, {
            importer: args.importer, resolveDir: args.resolveDir, kind: args.kind,
            pluginData: {versioned: true}
          });
          if (resolved.errors.length || resolved.external) return resolved;
          const data = await readFile(resolved.path);
          const suffix = new URL(resolved.suffix || '', 'https://asset.invalid/');
          suffix.searchParams.set('v', version(data));
          return {path: resolved.path, suffix: suffix.search + suffix.hash, pluginData: {contents: data}};
        });
        build.onLoad({filter: /\.(svg|png|jpe?g|gif|webp|avif|ico|woff2?|ttf|otf)$/}, (args) => {
          if (args.pluginData?.contents) return {contents: args.pluginData.contents, loader: 'file'};
        });
      }
    }],
    write: false,
    metafile: true,
    legalComments: 'none',
    charset: 'utf8',
    logLevel: 'warning'
  });
  const files = new Map(result.outputFiles.map((file) => [path.relative(outDir, file.path).split(path.sep).join('/'), file.contents]));
  const versions = Object.fromEntries([...files].sort(([a], [b]) => a.localeCompare(b, 'en')).map(([name, data]) => [name, version(data)]));
  const asset = (name) => {
    const relative = path.relative(outDir, path.resolve(sourceRoot, name)).split(path.sep).join('/');
    return `${base}/${relative}?v=${versions[relative]}`.replaceAll('&', '&amp;');
  };
  const sprite = iconSprite();
  for (const [index, page] of pages.entries()) {
    const [file, output] = Object.entries(result.metafile.outputs).find(([, item]) => item.entryPoint === `${page.directory}/main.js`);
    if (!output.cssBundle) throw new Error(`${page.directory}/main.js must import a stylesheet`);
    files.set(page.output, templates[index]
      .replace('<!--theme-->', themeBootstrap(page.entry === 'console'))
      .replace('<!--icons-->', sprite)
      .replace('%STYLE%', () => asset(output.cssBundle))
      .replace('%SCRIPT%', () => asset(file)));
  }
  files.set('console-routes.json', JSON.stringify(consoleRoutes.map(({path}) => path)) + '\n');
  files.set('asset-manifest.json', JSON.stringify({asset_base: base, versions}, null, 2) + '\n');
  // 先写完整暂存目录；切换失败回滚，切换中断后下次构建恢复 previous。
  await mkdir(path.dirname(outDir), {recursive: true});
  const staging = await mkdtemp(`${outDir}.next-`);
  let saved = false;
  try {
    for (const [name, data] of files) {
      const destination = path.join(staging, name);
      await mkdir(path.dirname(destination), {recursive: true});
      await writeFile(destination, data);
    }
    await rm(previous, {recursive: true, force: true});
    try {
      await rename(outDir, previous);
      saved = true;
    } catch (error) {
      if (error.code !== 'ENOENT') throw error;
    }
    try {
      await rename(staging, outDir);
    } catch (error) {
      if (saved) await rename(previous, outDir);
      throw error;
    }
    await rm(previous, {recursive: true, force: true});
  } finally {
    await rm(staging, {recursive: true, force: true});
  }
}

async function snapshot(directory) {
  const files = new Map();
  const walk = async (current) => {
    for (const entry of await readdir(current, {withFileTypes: true})) {
      const full = path.join(current, entry.name);
      if (entry.isDirectory()) await walk(full);
      else files.set(path.relative(directory, full), await readFile(full));
    }
  };
  await walk(directory);
  return files;
}

async function main() {
  const mode = process.argv[2] || '';
  if (mode === '--check') {
    const temporary = await mkdtemp(path.join(tmpdir(), 'orbitbeacon-web-'));
    try {
      await buildWeb(temporary);
      const [expected, actual] = await Promise.all([snapshot(temporary), snapshot(dist)]);
      const differences = [...new Set([...expected.keys(), ...actual.keys()])]
        .filter((name) => !expected.has(name) || !actual.has(name) || !expected.get(name).equals(actual.get(name)));
      if (differences.length) {
        console.error(`Embedded UI is out of date; run "npm run build" in web/:\n  ${differences.join('\n  ')}`);
        process.exitCode = 1;
      } else console.log('Embedded UI matches the source.');
    } finally {
      await rm(temporary, {recursive: true, force: true});
    }
  } else if (mode === '--watch') {
    const {watch} = await import('node:fs');
    let timer;
    let pending = Promise.resolve();
    const rebuild = () => {
      pending = pending.then(() => buildWeb(dist)).then(() => console.log(`rebuilt ${new Date().toLocaleTimeString()}`), (error) => console.error(error.message));
      return pending;
    };
    await rebuild();
    for (const input of ['src', 'themes', 'theme-bootstrap.mjs', 'build.config.json']) {
      watch(path.join(root, input), {recursive: ['src', 'themes'].includes(input)}, () => { clearTimeout(timer); timer = setTimeout(rebuild, 80); });
    }
  } else if (!mode) {
    await buildWeb(dist);
    console.log(`Built web UI into ${path.relative(process.cwd(), dist) || dist}`);
  } else {
    throw new Error(`Unknown build option: ${mode}`);
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
