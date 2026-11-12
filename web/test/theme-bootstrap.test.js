import test from 'node:test';
import assert from 'node:assert/strict';
import {runInNewContext} from 'node:vm';
import {themeBootstrap} from '../theme-bootstrap.mjs';

test('inline theme bootstrap preserves stored themes and console auth routes', () => {
  for (const consolePage of [false, true]) {
    for (const pathname of ['/', '/login', '/change-password', '/overview']) {
      for (const stored of ['light', 'dark', 'system', null, new Error('Storage unavailable')]) {
        const classes = [];
        const root = {dataset: {}, classList: {add: (value) => classes.push(value)}};
        const script = themeBootstrap(consolePage);
        assert.ok(script.startsWith('<script>') && script.endsWith('</script>'));
        runInNewContext(script.slice(8, -9), {
          document: {documentElement: root}, location: {pathname}, matchMedia: () => ({matches: false}),
          localStorage: {getItem(key) {
            if (key === 'orbitbeacon.nav') return null;
            assert.equal(key, 'orbitbeacon-theme');
            if (stored instanceof Error) throw stored;
            return stored;
          }}
        });
        assert.equal(root.dataset.theme, ['light', 'dark'].includes(stored) ? stored : undefined);
        assert.deepEqual(classes, consolePage && ['/login', '/change-password'].includes(pathname) ? ['route-login'] : []);
      }
    }
  }
});

test('console bootstrap applies saved collapse and tablet rail before CSS, never on public pages', () => {
  for (const consolePage of [false, true]) for (const stored of ['collapsed', 'expanded', null, new Error('Storage unavailable')]) for (const tablet of [false, true]) {
    const classes = [];
    const root = {dataset: {}, classList: {add: (value) => classes.push(value)}};
    runInNewContext(themeBootstrap(consolePage).slice(8, -9), {
      document: {documentElement: root}, location: {pathname: '/overview'}, matchMedia: () => ({matches: tablet}),
      localStorage: {getItem(key) {
        if (key !== 'orbitbeacon.nav') return null;
        if (stored instanceof Error) throw stored;
        return stored;
      }}
    });
    assert.equal(classes.includes('nav-collapsed'), consolePage && stored === 'collapsed');
    assert.equal(classes.includes('nav-compact'), consolePage && tablet);
  }
});
