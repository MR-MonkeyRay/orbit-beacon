import test from 'node:test';
import assert from 'node:assert/strict';
import {availabilityBar, availabilityLevel} from '../src/shared/availability.js';
import {validAvailability, validResponse} from '../src/public/validate.js';
import {availabilityPanel} from '../src/console/pages/node-availability.js';
import {setNavigationSignal} from '../src/console/api.js';
import {copy, elements, flush, installDOM} from './console-dom.js';

const data = () => ({start_day: '2026-10-07', percent: 99.99, days: [null, 100, 99.9, 99.89, 95, 94.99, ...Array(24).fill(null)]});
test('availability boundaries, accessible titles, stable bars and CSP', () => {
  const dom = installDOM();
  try {
    assert.deepEqual([null, 100, 99.9, 99.89, 95, 94.99, 0].map(availabilityLevel), ['neutral', 'normal', 'normal', 'warning', 'warning', 'critical', 'critical']);
    const bar = availabilityBar(); bar.update(data());
    const bars = bar.querySelectorAll('.availability-day');
    assert.equal(bars.length, 30);
    assert.equal(bar.querySelector('.availability-range').textContent, `${copy('public.availabilityRange', {count: 30})}${copy('public.today')}`);
    assert.equal(bar.getAttribute('role'), 'img');
    assert.equal(bar.getAttribute('aria-label'), `${copy('public.availability')}: 2026-10-07 – 2026-11-05, 99.99%`);
    assert.equal(bars[0].getAttribute('title'), `2026-10-07: ${copy('public.availabilityNoData')}`);
    assert.equal(bars[2].getAttribute('title'), '2026-10-09: 99.9%');
    for (const element of elements(bar)) {
      assert.equal(element.hasAttribute('style'), false);
      assert.equal(element.hasAttribute('tabindex'), false);
    }
    bar.update({...data(), percent: null, details: Array.from({length: 30}, () => ({offline_seconds: 42, unknown_seconds: 84}))});
    assert.equal(bar.querySelectorAll('.availability-day')[0], bars[0]);
    assert.ok(bar.getAttribute('aria-label').endsWith(copy('public.availabilityNoData')));
    assert.ok(bars[0].getAttribute('title').endsWith(copy('public.availabilityDuration', {offline: copy('public.duration.minutes', {minutes: 1}), unknown: copy('public.duration.minutes', {minutes: 2})})));
    // 触屏没有 hover：点按某天把当天明细显示在说明行。
    const detail = bar.querySelector('.availability-detail');
    assert.equal(detail.hidden, true);
    bars[0].click();
    assert.equal(detail.hidden, false);
    assert.equal(detail.textContent, bars[0].getAttribute('title'));
  } finally { dom.restore(); }
});
test('strict public availability validation invalidates the complete response', () => {
  assert.equal(validAvailability(data()), true);
  const base = data();
  for (const bad of [undefined, null, [], {}, {...base, start_day: null}, {...base, start_day: '2026-02-30'}, {...base, start_day: '2026-1-01'}, {...base, days: []}, {...base, days: Array(29).fill(null)}, {...base, days: Array(31).fill(null)}, {...base, days: Array(30)}, {...base, extra: 1}]) assert.equal(validAvailability(bad), false);
  for (const value of [undefined, -1, 100.01, NaN, Infinity, '99', true, {}]) {
    assert.equal(validAvailability({...base, percent: value}), false);
    assert.equal(validAvailability({...base, days: [value, ...Array(29).fill(null)]}), false);
  }
  for (const value of [null, 0, 95, 99.9, 100]) assert.equal(validAvailability({...base, percent: value, days: Array(30).fill(value)}), true);
  const node = {name: '', region: '', provider: '', status: 'unknown', last_seen: null, cpu_percent: null, memory_percent: null, load1: null, uptime_seconds: null, metric_status: {cpu_percent: 'unavailable', memory_percent: 'unavailable', load1: 'unavailable', uptime_seconds: 'unavailable'}};
  const response = {services: [], nodes: [node], has_more: false, next_offset: null, generated_at: '2026-11-05T00:00:00Z'};
  assert.equal(validResponse(response), true); // Unselected availability is absent.
  node.availability = base; assert.equal(validResponse(response), true);
  node.availability.days[0] = -1; assert.equal(validResponse(response), false);
});
test('console availability requests once and isolates failure in the existing error style', async () => {
  const dom = installDOM(); const originalFetch = globalThis.fetch;
  const navigation = new AbortController(); setNavigationSignal(navigation.signal);
  let requests = 0;
  globalThis.fetch = async (path) => {
    requests++; assert.ok(String(path).endsWith('/nodes/node%2Fid/availability?days=30'));
    return new Response(JSON.stringify({error: {code: 'internal_error'}}), {status: 500});
  };
  try {
    const panel = availabilityPanel('node/id', navigation.signal); dom.body.append(panel);
    assert.ok(panel.textContent.includes(copy('public.availability')));
    await flush();
    assert.equal(requests, 1);
    assert.ok(panel.querySelector('.history-error'));
    assert.ok(panel.textContent.includes(copy('errors.internal_error')));
  } finally { navigation.abort(); setNavigationSignal(null); globalThis.fetch = originalFetch; dom.restore(); }
});
test('console availability renders thirty detailed days on success', async () => {
  const dom = installDOM(); const originalFetch = globalThis.fetch;
  const navigation = new AbortController(); setNavigationSignal(navigation.signal);
  let requests = 0;
  const days = Array.from({length: 30}, (_, i) => ({day: new Date(Date.UTC(2026, 9, 7 + i)).toISOString().slice(0, 10), percent: 99.99, online_seconds: 100, offline_seconds: 1, unknown_seconds: 99}));
  globalThis.fetch = async () => { requests++; return new Response(JSON.stringify({days, percent: 99.99})); };
  try {
    const panel = availabilityPanel('node', navigation.signal); dom.body.append(panel); await flush();
    assert.equal(requests, 1);
    const bars = panel.querySelectorAll('.availability-day'); assert.equal(bars.length, 30);
    assert.equal(bars[0].getAttribute('title'), `2026-10-07: 99.99%; ${copy('public.availabilityDuration', {offline: copy('public.duration.minutes', {minutes: 1}), unknown: copy('public.duration.minutes', {minutes: 2})})}`);
    assert.equal(panel.querySelector('.history-error'), null);
  } finally { navigation.abort(); setNavigationSignal(null); globalThis.fetch = originalFetch; dom.restore(); }
});


test('availability range labels derive from the supplied day count', () => {
  const dom = installDOM();
  try {
    const bar = availabilityBar(); const data = {start_day: '2026-10-28', days: Array(7).fill(100), percent: 100};
    bar.update(data);
    assert.equal(bar.querySelector('.availability-range').textContent, `${copy('public.availabilityRange', {count: 7})}${copy('public.today')}`);
    assert.match(bar.getAttribute('aria-label'), /2026-10-28 – 2026-11-03/);
  } finally { dom.restore(); }
});
