import {getLocale, t, tParts} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {h, svg} from '../../shared/dom.js';
import {api} from '../api.js';
import {formatBytes, formatPercent, when} from '../format.js';
import {fitChartText, niceMax} from '../ui/chart.js';
import {facts, panel} from '../ui/components.js';
import {historyError} from '../ui/history-error.js';

const DAY = 86400000;
const dateFormat = new Intl.DateTimeFormat(getLocale(), {dateStyle: 'medium', timeZone: 'UTC'});
const shortDateFormat = new Intl.DateTimeFormat(getLocale(), {month: 'short', day: 'numeric', timeZone: 'UTC'});

// 不将浮点数、科学计数法或格式错误的 API 值强制转为字节。
export function parseTrafficBytes(value) {
  return typeof value === 'string' && value.length > 0 && !/[^0-9]/.test(value) ? BigInt(value) : 0n;
}

const bytes = (value) => typeof value === 'bigint' ? (value > 0n ? value : 0n) : parseTrafficBytes(value);

export function fillTrafficDays(daily, now = new Date()) {
  const today = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate());
  const byDay = new Map((Array.isArray(daily) ? daily : []).filter((item) => item && typeof item.day === 'string').map((item) => [item.day, item]));
  return Array.from({length: 30}, (_, index) => {
    const day = new Date(today - (29 - index) * DAY).toISOString().slice(0, 10);
    const item = byDay.get(day);
    return {day, rx_bytes: parseTrafficBytes(item?.rx_bytes), tx_bytes: parseTrafficBytes(item?.tx_bytes)};
  });
}

export function cycleDateLabel(cycle) {
  const start = Date.parse(cycle?.start);
  const end = Date.parse(cycle?.end);
  if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) return t('nodes.cycleUnavailable');
  return `${dateFormat.format(start)} – ${dateFormat.format(end - DAY)} (UTC)`;
}

export function trafficQuota(usedValue, quotaValue) {
  const used = bytes(usedValue);
  const quota = bytes(quotaValue);
  const state = used > 0n && used >= quota ? 'over' : quota > 0n && used * 100n >= quota * 80n ? 'warning' : 'normal';
  // 比较保持精确，只在最终有界的显示比例处转换。
  const tenths = quota > 0n ? used * 1000n / quota : used > 0n ? 1000n : 0n;
  const percent = Number(tenths > BigInt(Number.MAX_SAFE_INTEGER) ? BigInt(Number.MAX_SAFE_INTEGER) : tenths) / 10;
  return {percent, state};
}

function quotaMeter(used, quota) {
  const {percent, state} = trafficQuota(used, quota);
  const label = t('nodes.trafficQuotaDescription', {used: formatBytes(used), quota: formatBytes(quota), percent: formatPercent(percent)});
  return h('section', {class: `traffic-quota ${state}`, 'aria-label': t('nodes.trafficQuota')},
    h('h3', null, t('nodes.quota')),
    h('p', {class: 'num'}, t('nodes.traffic.usage', {used: formatBytes(used), quota: formatBytes(quota), percent: formatPercent(percent)})),
    h('meter', {min: 0, max: 100, low: 0, high: 80, optimum: 0, value: Math.min(100, percent),
      'aria-label': label, 'aria-valuetext': label}, label),
    state === 'normal' ? null : h('p', null, state === 'over' ? t('nodes.quotaExceeded') : t('nodes.quotaWarning')));
}

function dailyChart(days) {
  const width = 960;
  const height = 280;
  const left = 100;
  const right = 16;
  const top = 16;
  const baseline = height - 40;
  const plotHeight = baseline - top;
  const peak = days.reduce((max, day) => {
    const total = day.rx_bytes + day.tx_bytes;
    return total > max ? total : max;
  }, 0n);
  // 按坐标轴显示的二进制单位取整，刻度读作 50/100/150 GiB。
  let unit = 1n;
  while (peak >= unit * 1024n) unit *= 1024n;
  // 只有有界的比例进入 Number；字节总量和刻度保持精确。
  const precision = 1000000000000n;
  const normalized = Number((peak * precision + unit - 1n) / unit) / 1e12;
  const rounded = BigInt(Math.ceil(niceMax(normalized) * 1e12));
  const proposed = (rounded * unit + precision - 1n) / precision;
  const upper = proposed > peak ? proposed : peak;
  const max = upper > 3n ? upper : 3n;
  const scaledHeight = (value) => Number(value * 1000000n / max) / 1000000 * plotHeight;
  // h() 创建 HTML 元素；与历史图表一样使用带命名空间的 svg()。
  const drawing = svg('svg', {class: 'chart-svg traffic-svg', viewBox: `0 0 ${width} ${height}`,
    role: 'group', 'aria-label': t('nodes.dailyTrafficAria')});
  const text = (value, attributes) => {
    const element = svg('text', attributes);
    element.textContent = value;
    return element;
  };
  for (let index = 0; index <= 3; index += 1) {
    const y = baseline - index / 3 * plotHeight;
    // history 的 formatAxisValue 处理速率和负载；这里是字节总量。
    drawing.append(svg('line', {class: 'chart-gridline', x1: left, x2: width - right, y1: y, y2: y}),
      text(formatBytes(max * BigInt(index) / 3n), {class: 'chart-axis', x: left - 10, y: y + 4, 'text-anchor': 'end'}));
  }
  const slot = (width - left - right) / days.length;
  days.forEach((day, index) => {
    const x = left + index * slot + 2;
    const rxHeight = scaledHeight(day.rx_bytes);
    const txHeight = scaledHeight(day.tx_bytes);
    const label = t('nodes.dailyTrafficDescription', {date: dateFormat.format(Date.parse(day.day)), rx: formatBytes(day.rx_bytes), tx: formatBytes(day.tx_bytes), total: formatBytes(day.rx_bytes + day.tx_bytes)});
    const group = svg('g', {class: 'traffic-day', tabindex: 0, role: 'img', 'aria-label': label});
    const title = svg('title');
    title.textContent = label;
    group.append(title,
      svg('rect', {class: 'traffic-hit', x: x - 2, y: top, width: slot, height: plotHeight}),
      svg('rect', {class: 'traffic-rx', x, y: baseline - rxHeight, width: slot - 4, height: rxHeight}),
      svg('rect', {class: 'traffic-tx', x, y: baseline - rxHeight - txHeight, width: slot - 4, height: txHeight}));
    drawing.append(group);
  });
  // 30 天图在移动端缩小时，稀疏的日期刻度仍保持可读。
  for (const index of [0, 14, 29]) {
    drawing.append(text(shortDateFormat.format(Date.parse(days[index].day)), {class: 'chart-axis',
      x: left + (index + 0.5) * slot, y: height - 10, 'text-anchor': index === 0 ? 'start' : index === 29 ? 'end' : 'middle'}));
  }
  fitChartText(drawing, width);
  const key = (label, className) => h('span', {class: 'chart-key'}, h('i', {class: className, 'aria-hidden': 'true'}), label);
  return h('section', {class: 'traffic-chart', 'aria-label': t('nodes.dailyTraffic')},
    h('h3', null, t('nodes.dailyTraffic')),
    h('p', {class: 'muted'}, t('nodes.dailyTrafficIntro')),
    h('div', {class: 'chart-legend', 'aria-label': t('nodes.chartLegend')}, key(t('nodes.receivedLower'), 'traffic-rx-key'), key(t('nodes.sentUpper'), 'traffic-tx-key')),
    h('div', {class: 'traffic-plot'}, drawing),
    h('p', {class: 'note'}, t('nodes.dailyTrafficHint')));
}

function trafficContent(data) {
  const cycle = data.cycle || {};
  const rx = parseTrafficBytes(cycle.rx_bytes);
  const tx = parseTrafficBytes(cycle.tx_bytes);
  const total = parseTrafficBytes(cycle.total_bytes);
  const daily = Array.isArray(data.daily) ? data.daily : [];
  const empty = !data.updated_at && !daily.length && rx === 0n && tx === 0n && total === 0n;
  return h('div', {class: 'traffic-content'},
    h('section', {class: 'traffic-cycle', 'aria-label': t('nodes.currentCycle')}, h('h3', null, t('nodes.currentCycle')),
      h('p', {class: 'muted'}, cycleDateLabel(cycle)),
      facts([[t('nodes.received'), formatBytes(rx)], [t('nodes.sent'), formatBytes(tx)], [t('nodes.total'), formatBytes(total)]])),
    // 配额为 0 表示无限制，与流量计费表单一致。
    parseTrafficBytes(data.quota_bytes) > 0n ? quotaMeter(total, parseTrafficBytes(data.quota_bytes)) : null,
    empty ? h('p', {class: 'muted', role: 'status'}, t('nodes.trafficEmpty')) : dailyChart(fillTrafficDays(daily)),
    data.updated_at ? h('p', {class: 'note'}, tParts('nodes.updated', {time: when(data.updated_at)})) : null);
}

// 切换详情标签页时保留面板；流量计费设置变更只刷新此面板。
export function trafficPanel(id, signal) {
  const content = h('div', {class: 'traffic-body'});
  const root = panel(t('nodes.traffic'), {}, content);
  let loadVersion = 0;
  let currentPlot;
  let followLatest = true;
  const alignLatest = () => {
    const plot = content.querySelector('.traffic-plot');
    if (!plot || signal?.aborted) return;
    if (plot !== currentPlot) {
      currentPlot = plot;
      followLatest = true;
      const stopFollowing = () => { if (currentPlot === plot) followLatest = false; };
      plot.addEventListener('pointerdown', stopFollowing, {once: true});
      plot.addEventListener('wheel', (event) => {
        if (event.deltaX || event.shiftKey) stopFollowing();
      }, {passive: true});
      plot.addEventListener('keydown', (event) => {
        if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) stopFollowing();
      });
    }
    if (followLatest && plot.clientWidth > 0) {
      plot.scrollLeft = plot.scrollWidth;
    }
  };
  const resize = typeof ResizeObserver === 'function' ? new ResizeObserver(alignLatest) : null;
  resize?.observe(content);
  signal?.addEventListener('abort', () => resize?.disconnect(), {once: true});
  async function load() {
    if (signal?.aborted) return;
    const version = ++loadVersion;
    content.setAttribute('aria-busy', 'true');
    content.replaceChildren(h('p', {class: 'muted', role: 'status'}, t('nodes.trafficLoading')));
    try {
      const data = await api(`/nodes/${encodeURIComponent(id)}/traffic?days=30`, {signal});
      if (!signal?.aborted && version === loadVersion) {
        content.replaceChildren(trafficContent(data));
        alignLatest();
      }
    } catch (error) {
      if (!signal?.aborted && version === loadVersion && error.name !== 'AbortError') content.replaceChildren(
        historyError(t('nodes.trafficLoadError', {error: errorText(error)}),
          {retryID: 'traffic-retry', onRetry: load, status: true}));
    } finally {
      if (!signal?.aborted && version === loadVersion) content.setAttribute('aria-busy', 'false');
    }
  }
  root.reload = load;
  load();
  return root;
}
