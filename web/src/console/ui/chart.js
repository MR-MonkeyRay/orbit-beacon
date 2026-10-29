import {getLocale, t} from '../../shared/i18n.js';
import {h, svg} from '../../shared/dom.js';
import {formatLoad, formatPercent, formatRate} from '../format.js';

const WIDTH = 640;
const HEIGHT = 240;
const LEFT = 100;
const RIGHT = 16;
const TOP = 16;
const BOTTOM = 38;
const timestamp = (point) => Date.parse(point.t);
const coordinate = (value) => Number(value.toFixed(2));

// SVG text scales with the view box, so --chart-k (view box units per CSS
// pixel) keeps axis labels at a fixed CSS size however wide a chart renders.
// A chart removed after it was laid out reports a zero size once, which
// releases its observation; one built before insertion keeps waiting.
let textFitter;
const viewBoxWidths = new WeakMap();
const laidOut = new WeakSet();
export function fitChartText(drawing, viewBoxWidth) {
  if (typeof ResizeObserver !== 'function') return;
  textFitter ||= new ResizeObserver((entries) => {
    for (const {target, contentRect} of entries) {
      if (!target.isConnected) { if (laidOut.has(target)) textFitter.unobserve(target); } else if (contentRect.width > 0) {
        laidOut.add(target);
        target.style.setProperty('--chart-k', (viewBoxWidths.get(target) / contentRect.width).toFixed(3));
      }
    }
  });
  viewBoxWidths.set(drawing, viewBoxWidth);
  textFitter.observe(drawing);
}

// Three equal intervals give four labelled gridlines, even for very small loads.
export function niceMax(value) {
  if (!Number.isFinite(value) || value <= 0) return 1;
  const roughStep = value / 3;
  const magnitude = 10 ** Math.floor(Math.log10(roughStep));
  const fraction = roughStep / magnitude;
  const step = [1, 2, 5, 10].find((candidate) => candidate >= fraction);
  return Number((3 * step * magnitude).toPrecision(12));
}

export function chartScales(points, unit) {
  let start = Infinity;
  let end = -Infinity;
  let peak = 0;
  for (const point of points) {
    const time = timestamp(point);
    start = Math.min(start, time);
    end = Math.max(end, time);
    peak = Math.max(peak, point.avg, point.max);
  }
  if (!points.length) { start = 0; end = 1; }
  const max = unit === 'percent' ? 100 : niceMax(peak);
  const x = (time) => start === end ? (LEFT + WIDTH - RIGHT) / 2
    : LEFT + (time - start) / (end - start) * (WIDTH - LEFT - RIGHT);
  const y = (value) => HEIGHT - BOTTOM - Math.max(0, Math.min(max, value)) / max * (HEIGHT - TOP - BOTTOM);
  return {start, end, max, x, y};
}

// Each disconnected segment closes its own area down to zero. A lone sample
// has a zero-length line with round caps so it remains visible as a dot.
export function chartPaths(points, scales, stepSeconds, valueKey = 'avg') {
  const segments = [];
  for (let index = 0; index < points.length; index += 1) {
    if (index === 0 || timestamp(points[index]) - timestamp(points[index - 1]) > 1.5 * stepSeconds * 1000) segments.push([]);
    segments.at(-1).push(points[index]);
  }
  const lines = [];
  const areas = [];
  for (const segment of segments) {
    const coordinates = segment.map((point) => `${coordinate(scales.x(timestamp(point)))},${coordinate(scales.y(point[valueKey]))}`);
    const line = `M${coordinates.join('L')}${segment.length === 1 ? `L${coordinates[0]}` : ''}`;
    const firstX = coordinate(scales.x(timestamp(segment[0])));
    const lastX = coordinate(scales.x(timestamp(segment.at(-1))));
    lines.push(line);
    areas.push(`${line}L${lastX},${coordinate(scales.y(0))}L${firstX},${coordinate(scales.y(0))}Z`);
  }
  return {line: lines.join(''), area: areas.join('')};
}

// Axis ticks add decimals for small scales so near-idle charts keep distinct labels.
export function formatAxisValue(value, unit, max) {
  if (unit === 'percent' || !(max > 0) || !Number.isFinite(value)) return formatChartValue(value, unit);
  const step = max / 3;
  if (unit === 'bytes_per_second' && step >= 1) return formatRate(value);
  const decimals = Math.min(6, Math.max(unit === 'bytes_per_second' ? 0 : 2, Math.ceil(-Math.log10(step)) + 1));
  const formatted = Number(value.toFixed(decimals)).toLocaleString(getLocale(), {useGrouping: false, minimumFractionDigits: decimals, maximumFractionDigits: decimals});
  return unit === 'bytes_per_second' ? t('nodes.bytesRate', {value: formatted}) : formatted;
}

export function formatChartValue(value, unit) {
  if (unit === 'percent') return formatPercent(value);
  if (unit === 'bytes_per_second') return value > 0 && value < 1 ? t('nodes.chart.belowByte') : formatRate(value);
  return value > 0 && value < 0.01 ? t('nodes.chart.belowLoad') : formatLoad(value);
}

// Sorted points and a time-based x scale keep omitted buckets as real gaps.
// Ties go to the earlier sample; coordinates outside the plot clamp to an end.
export function pointIndexAtX(points, x, scales) {
  if (!points.length || !Number.isFinite(x)) return -1;
  let low = 0;
  let high = points.length - 1;
  while (low < high) {
    const middle = Math.floor((low + high) / 2);
    if (scales.x(timestamp(points[middle])) < x) low = middle + 1;
    else high = middle;
  }
  if (low === 0) return 0;
  return Math.abs(scales.x(timestamp(points[low - 1])) - x) <= Math.abs(scales.x(timestamp(points[low])) - x) ? low - 1 : low;
}

const timeFormat = new Intl.DateTimeFormat(getLocale(), {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'});
const fullTimeFormat = new Intl.DateTimeFormat(getLocale(), {dateStyle: 'medium', timeStyle: 'long'});

function seriesSummary(series, unit) {
  if (!series.points.length) return t('nodes.chart.noSamples', {label: series.label});
  let min = Infinity;
  let max = -Infinity;
  for (const point of series.points) {
    min = Math.min(min, point.avg);
    max = Math.max(max, point.max);
  }
  const latest = series.points.at(-1);
  const format = (value) => formatChartValue(value, unit);
  return t('nodes.chart.summary', {label: series.label, avg: format(latest.avg), max: format(latest.max), time: fullTimeFormat.format(new Date(latest.t)), min: format(min), peak: format(max), count: series.points.length});
}

function legendKey(label, classes) {
  return h('span', {class: 'chart-key'}, h('i', {class: classes, 'aria-hidden': 'true'}), label);
}

// series: [{label, points, stepSeconds}], ordered by t as returned by history.
// All DOM is built without HTML strings.
export function chart({title, unit, series, id, selectedTime}) {
  const timeline = [...new Map(series.flatMap((item) => item.points).map((point) => [timestamp(point), point])).values()]
    .sort((a, b) => timestamp(a) - timestamp(b));
  if (!timeline.length) return h('p', {class: 'muted'}, t('nodes.chart.empty'));
  // Include both network series' peaks even where their timestamps overlap.
  const scales = chartScales(series.flatMap((item) => item.points), unit);
  const descriptionID = `${id}-description`;
  const tooltipID = `${id}-tooltip`;
  const description = h('p', {class: 'sr', id: descriptionID},
    t('nodes.chart.guidance'));
  const drawing = svg('svg', {class: 'chart-svg', viewBox: `0 0 ${WIDTH} ${HEIGHT}`, role: 'img',
    'aria-label': `${title}. ${series.map((item) => seriesSummary(item, unit)).join(' ')}`});
  const text = (value, attributes) => {
    const element = svg('text', attributes);
    element.textContent = value;
    return element;
  };
  const ticks = unit === 'percent' ? [0, 50, 100] : [0, scales.max / 3, scales.max * 2 / 3, scales.max];
  for (const value of ticks) {
    const y = scales.y(value);
    drawing.append(svg('line', {class: 'chart-gridline', x1: LEFT, x2: WIDTH - RIGHT, y1: y, y2: y}),
      text(formatAxisValue(value, unit, scales.max), {class: 'chart-axis', x: LEFT - 10, y: y + 4, 'text-anchor': 'end'}));
  }
  drawing.append(text(timeFormat.format(scales.start), {class: 'chart-axis', x: LEFT, y: HEIGHT - 10}),
    text(timeFormat.format(scales.end), {class: 'chart-axis', x: WIDTH - RIGHT, y: HEIGHT - 10, 'text-anchor': 'end'}));
  series.forEach((item, index) => {
    const average = chartPaths(item.points, scales, item.stepSeconds);
    const maximum = chartPaths(item.points, scales, item.stepSeconds, 'max');
    const group = svg('g', {class: `chart-series chart-series-${index}`, 'aria-hidden': 'true'});
    group.append(svg('path', {class: 'chart-area', d: average.area}),
      svg('path', {class: 'chart-max', d: maximum.line}),
      svg('path', {class: 'chart-average', d: average.line}));
    drawing.append(group);
  });
  const crosshair = svg('line', {class: 'chart-crosshair', y1: TOP, y2: HEIGHT - BOTTOM, visibility: 'hidden', 'aria-hidden': 'true'});
  drawing.append(crosshair);
  const tooltip = h('div', {class: 'chart-tooltip', id: tooltipID, role: 'status', 'aria-live': 'polite', 'aria-atomic': 'true', hidden: true});
  const plot = h('div', {class: 'chart-plot', id, tabindex: 0, role: 'group', 'aria-label': t('nodes.chart.explore', {title}),
    'aria-describedby': `${descriptionID} ${tooltipID}`}, drawing, tooltip);
  // A rebuilt chart keeps the reader's selected sample when it still exists.
  const kept = timeline.findIndex((point) => timestamp(point) === selectedTime);
  let selected = kept >= 0 ? kept : timeline.length - 1;
  const byTime = series.map((item) => new Map(item.points.map((point) => [timestamp(point), point])));
  const show = (index) => {
    selected = index;
    plot.dataset.selectedTime = String(timestamp(timeline[index]));
    const time = timestamp(timeline[index]);
    const x = scales.x(time);
    crosshair.setAttribute('x1', x);
    crosshair.setAttribute('x2', x);
    crosshair.setAttribute('visibility', 'visible');
    tooltip.hidden = false;
    tooltip.replaceChildren(h('strong', null, fullTimeFormat.format(time)), ...series.map((item, position) => {
      const point = byTime[position].get(time);
      return h('div', null, t('nodes.chart.tooltip', {label: item.label, avg: formatChartValue(point?.avg, unit), max: formatChartValue(point?.max, unit)}));
    }));
    // Place beside the crosshair, flipping and clamping with measured sizes so
    // the tooltip always stays inside the plot.
    const plotWidth = plot.clientWidth;
    const anchor = x / WIDTH * plotWidth;
    const width = tooltip.offsetWidth;
    const preferred = x > WIDTH / 2 ? anchor - width - 8 : anchor + 8;
    tooltip.style.left = `${Math.max(4, Math.min(plotWidth - width - 4, preferred))}px`;
  };
  const hide = () => {
    tooltip.hidden = true;
    crosshair.setAttribute('visibility', 'hidden');
  };
  plot.addEventListener('pointermove', (event) => {
    const bounds = drawing.getBoundingClientRect();
    if (!bounds.width) return;
    show(pointIndexAtX(timeline, (event.clientX - bounds.left) / bounds.width * WIDTH, scales));
  });
  plot.addEventListener('pointerleave', () => { if (document.activeElement !== plot) hide(); });
  plot.addEventListener('focus', () => show(selected));
  plot.addEventListener('blur', hide);
  plot.addEventListener('keydown', (event) => {
    const index = {ArrowLeft: selected - 1, ArrowRight: selected + 1, Home: 0, End: timeline.length - 1}[event.key];
    if (index === undefined) { if (event.key === 'Escape') hide(); return; }
    event.preventDefault();
    show(Math.max(0, Math.min(timeline.length - 1, index)));
  });
  fitChartText(drawing, WIDTH);
  return h('div', {class: 'history-chart'},
    h('div', {class: 'chart-legend', 'aria-label': t('nodes.chartLegend')},
      series.map((item, index) => legendKey(item.label, `chart-series chart-series-${index} chart-average`)),
      legendKey(t('nodes.chart.maximum'), 'chart-series chart-maximum-key')),
    plot, description);
}
