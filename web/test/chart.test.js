import test from 'node:test';
import assert from 'node:assert/strict';
import {chart, chartPaths, chartScales, formatAxisValue, formatChartValue, niceMax, pointIndexAtX} from '../src/console/ui/chart.js';

const point = (minute, avg = 10, max = 20) => ({t: new Date(Date.UTC(2026, 9, 27, 12, minute)).toISOString(), avg, max});

test('nice maxima cover data with three equally spaced, readable intervals', () => {
  for (const [value, expected] of [[0, 1], [-3, 1], [NaN, 1], [Infinity, 1], [.09, .15], [.3, .3], [1, 1.5], [7, 15], [101, 150], [600, 600], [601, 1500]]) {
    assert.equal(niceMax(value), expected, String(value));
    if (value > 0 && Number.isFinite(value)) assert.ok(niceMax(value) >= value);
  }
});

test('chart scales fix percent bounds and include peaks for other units', () => {
  const points = [point(0, 20, 70), point(1, 80, 120)];
  const percent = chartScales(points, 'percent');
  assert.equal(percent.max, 100);
  assert.equal(percent.y(0), 202);
  assert.equal(percent.y(100), 16);
  assert.equal(percent.y(120), 16);
  assert.equal(percent.y(-10), 202);
  assert.equal(percent.x(Date.parse(points[0].t)), 100);
  assert.equal(percent.x(Date.parse(points[1].t)), 624);
  assert.equal(chartScales(points, 'load').max, 150);
  assert.equal(chartScales([], 'load').max, 1);
  // Small peaks keep their own scale so near-idle variation stays visible.
  assert.equal(chartScales([point(0, 0.001, 0.003)], 'load').max, 0.003);
  const single = chartScales([point(0, 0, 0)], 'load');
  assert.equal(single.x(Date.parse(point(0).t)), 362);
  assert.equal(single.max, 1);
  const loadTicks = [0, 1, 2, 3].map((n) => formatAxisValue(0.003 * n / 3, 'load', 0.003));
  assert.equal(new Set(loadTicks).size, 4, loadTicks.join());
  const rateTicks = [0, 1, 2, 3].map((n) => formatAxisValue(1.5 * n / 3, 'bytes_per_second', 1.5));
  assert.equal(new Set(rateTicks).size, 4, rateTicks.join());
  assert.equal(formatAxisValue(1536, 'bytes_per_second', 3072), '1.5 KiB/s');
});

test('paths break only above 1.5 steps and close every area independently', () => {
  const points = [point(0), point(1.5), point(4)];
  // Date.UTC truncates fractional minutes; use exact seconds for the boundary.
  points[1].t = new Date(Date.parse(points[0].t) + 90000).toISOString();
  const scales = chartScales(points, 'percent');
  const paths = chartPaths(points, scales, 60);
  assert.equal((paths.line.match(/M/g) || []).length, 2);
  assert.equal((paths.area.match(/Z/g) || []).length, 2);
  assert.match(paths.line, /^M100,183.4L296.5,183.4M624,183.4L624,183.4$/);
  const maxima = chartPaths(points, scales, 60, 'max');
  assert.match(maxima.line, /,164.8/);
  assert.doesNotMatch(paths.line, /NaN|Infinity/);
  assert.deepEqual(chartPaths([], scales, 60), {line: '', area: ''});
});

test('isolated and zero samples remain visible and do not connect across gaps', () => {
  const points = [point(0, 0, 0), point(10, 0, 0)];
  const paths = chartPaths(points, chartScales(points, 'load'), 60);
  assert.equal(paths.line, 'M100,202L100,202M624,202L624,202');
});

test('tooltip values match the backend unit names and handle missing values', () => {
  assert.equal(formatChartValue(12.25, 'percent'), '12.3%');
  assert.equal(formatChartValue(1536, 'bytes_per_second'), '1.5 KiB/s');
  assert.equal(formatChartValue(0.3, 'bytes_per_second'), '< 1 B/s');
  assert.equal(formatChartValue(0.003, 'load'), '< 0.01');
  assert.equal(formatChartValue(0, 'load'), '0.00');
  assert.equal(formatChartValue(.125, 'load'), '0.13');
  assert.equal(formatChartValue(0, 'load'), '0.00');
  assert.equal(formatChartValue(null, 'percent'), 'Unavailable');
  assert.equal(formatChartValue(undefined, 'bytes_per_second'), 'Unavailable');
  assert.equal(formatChartValue(NaN, 'load'), 'Unavailable');
});

test('point lookup uses time coordinates, clamps ends, and picks earlier ties', () => {
  const points = [point(0), point(1), point(10)];
  const scales = chartScales(points, 'percent');
  const x = (minute) => scales.x(Date.parse(point(minute).t));
  assert.equal(pointIndexAtX(points, -50, scales), 0);
  assert.equal(pointIndexAtX(points, 1000, scales), 2);
  assert.equal(pointIndexAtX(points, x(1), scales), 1);
  assert.equal(pointIndexAtX(points, x(5), scales), 1);
  assert.equal(pointIndexAtX(points, x(5) + (x(6) - x(5)) / 2, scales), 1);
  assert.equal(pointIndexAtX(points, x(6), scales), 2);
  assert.equal(pointIndexAtX([], 50, scales), -1);
  assert.equal(pointIndexAtX(points, NaN, scales), -1);
  assert.equal(pointIndexAtX([point(0)], 50, scales), 0);
});

test('keyboard tooltip renders actual values and keeps missing network buckets unavailable', () => {
  // Native replaceChildren stringifies an array rather than flattening it.
  // Model that behavior so this catches accidental array/HTML string output.
  class Element extends EventTarget {
    children = [];
    attributes = new Map();
    style = {};
    dataset = {};
    classList = {toggle() {}};
    append(...children) { this.children.push(...children.map((child) => child instanceof Element ? child : String(child))); }
    replaceChildren(...children) { this.children = []; this.append(...children); }
    setAttribute(key, value) { this.attributes.set(key, String(value)); }
    getAttribute(key) { return this.attributes.get(key); }
    set textContent(value) { this.replaceChildren(String(value)); }
    get textContent() { return this.children.map((child) => child instanceof Element ? child.textContent : child).join(''); }
  }
  const originalNode = globalThis.Node;
  const originalDocument = globalThis.document;
  globalThis.Node = Element;
  globalThis.document = {createElement: () => new Element(), createElementNS: () => new Element()};
  const elements = (node) => [node, ...node.children.filter((child) => child instanceof Element).flatMap(elements)];
  try {
    const root = chart({title: 'Network', unit: 'bytes_per_second', id: 'network-test', series: [
      {label: 'Receive (rx)', points: [point(0, 10, 20)], stepSeconds: 60},
      {label: 'Send (tx)', points: [point(1, 100, 200)], stepSeconds: 60}
    ]});
    const nodes = elements(root);
    const plot = nodes.find((element) => element.getAttribute('id') === 'network-test');
    const tooltip = nodes.find((element) => element.getAttribute('id') === 'network-test-tooltip');
    const image = nodes.find((element) => element.getAttribute('role') === 'img');
    assert.match(image.getAttribute('aria-label'), /latest average 100 B\/s/);
    assert.match(image.getAttribute('aria-label'), /minimum average 10 B\/s/);
    assert.match(image.getAttribute('aria-label'), /peak maximum 200 B\/s/);
    assert.match(root.textContent, /300 B\/s/); // Both series contribute to the y scale.
    const key = (value) => plot.dispatchEvent(Object.assign(new Event('keydown', {cancelable: true}), {key: value}));
    plot.dispatchEvent(new Event('focus'));
    key('Home');
    assert.equal(tooltip.hidden, false);
    assert.match(tooltip.textContent, /Receive \(rx\): avg 10 B\/s · max 20 B\/s/);
    assert.match(tooltip.textContent, /Send \(tx\): avg Unavailable · max Unavailable/);
    key('ArrowRight');
    assert.match(tooltip.textContent, /Send \(tx\): avg 100 B\/s · max 200 B\/s/);
    assert.match(tooltip.textContent, /Receive \(rx\): avg Unavailable · max Unavailable/);
    key('Escape');
    assert.equal(tooltip.hidden, true);
    key('ArrowLeft');
    assert.equal(tooltip.hidden, false);
    plot.dispatchEvent(new Event('blur'));
    assert.equal(tooltip.hidden, true);
    // A refresh rebuild keeps the selected sample instead of jumping to the latest.
    assert.equal(plot.dataset.selectedTime, String(Date.parse(point(0).t)));
    const rebuilt = elements(chart({title: 'Network', unit: 'bytes_per_second', id: 'network-test', selectedTime: Number(plot.dataset.selectedTime), series: [
      {label: 'Receive (rx)', points: [point(0, 10, 20)], stepSeconds: 60},
      {label: 'Send (tx)', points: [point(1, 100, 200)], stepSeconds: 60}
    ]}));
    const rebuiltPlot = rebuilt.find((element) => element.getAttribute('id') === 'network-test');
    rebuiltPlot.dispatchEvent(new Event('focus'));
    assert.match(rebuilt.find((element) => element.getAttribute('id') === 'network-test-tooltip').textContent, /Receive \(rx\): avg 10 B\/s/);
  } finally {
    if (originalNode === undefined) delete globalThis.Node;
    else globalThis.Node = originalNode;
    if (originalDocument === undefined) delete globalThis.document;
    else globalThis.document = originalDocument;
  }
});
