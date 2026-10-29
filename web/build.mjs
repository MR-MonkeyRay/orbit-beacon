// Builds the embedded web UI into internal/server/webui/dist.
//
//   node build.mjs           production build (minified, content-hashed)
//   node build.mjs --watch   rebuild on change
//   node build.mjs --check   build into a temporary directory and fail if dist differs
//
// HTML shells are served with no-store; scripts and styles land in
// _app/immutable/ under content-hashed names, which the server caches for a year.
import * as esbuild from 'esbuild';
import {mkdtemp, mkdir, readdir, readFile, rm, writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {ICONS} from './src/shared/icons.js';
import {themeBootstrap} from './theme-bootstrap.mjs';

const root = path.dirname(fileURLToPath(import.meta.url));
const dist = path.resolve(root, '../internal/server/webui/dist');
const pages = [
  {entry: 'console', template: 'src/console/index.html', output: '200.html'},
  {entry: 'public', template: 'src/public/index.html', output: 'public.html'}
];
const mode = process.argv[2] || '';

function iconSprite() {
  const symbols = Object.entries(ICONS).map(([name, body]) => `<symbol id="i-${name}" viewBox="0 0 24 24">${body}</symbol>`).join('');
  return `<svg width="0" height="0" class="icon-sprite" aria-hidden="true">${symbols}</svg>`;
}

async function build(outDir) {
  const immutable = path.join(outDir, '_app/immutable');
  await rm(immutable, {recursive: true, force: true});
  await mkdir(immutable, {recursive: true});
  const result = await esbuild.build({
    absWorkingDir: root,
    entryPoints: {console: 'src/console/main.js', public: 'src/public/main.js'},
    bundle: true,
    minify: true,
    format: 'esm',
    target: ['chrome111', 'firefox115', 'safari16.4'],
    entryNames: '[name]-[hash]',
    outdir: immutable,
    metafile: true,
    legalComments: 'none',
    charset: 'utf8',
    logLevel: 'warning'
  });
  const sprite = iconSprite();
  for (const page of pages) {
    const [file, output] = Object.entries(result.metafile.outputs).find(([, item]) => item.entryPoint === `src/${page.entry}/main.js`);
    const asset = (name) => `/${path.relative(outDir, path.resolve(root, name)).split(path.sep).join('/')}`;
    const template = await readFile(path.join(root, page.template), 'utf8');
    const html = template
      .replace('<!--theme-->', themeBootstrap(page.entry === 'console'))
      .replace('<!--icons-->', sprite)
      .replace('%STYLE%', asset(output.cssBundle))
      .replace('%SCRIPT%', asset(file));
    await writeFile(path.join(outDir, page.output), html);
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

if (mode === '--check') {
  const temporary = await mkdtemp(path.join(tmpdir(), 'orbitbeacon-web-'));
  try {
    await build(temporary);
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
  const rebuild = () => build(dist).then(() => console.log(`rebuilt ${new Date().toLocaleTimeString()}`), (error) => console.error(error.message));
  await rebuild();
  for (const input of ['src', 'theme-bootstrap.mjs']) {
    watch(path.join(root, input), {recursive: input === 'src'}, () => { clearTimeout(timer); timer = setTimeout(rebuild, 80); });
  }
} else {
  await build(dist);
  console.log(`Built web UI into ${path.relative(process.cwd(), dist) || dist}`);
}
