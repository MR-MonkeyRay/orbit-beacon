import {h, icon} from '../shared/dom.js';
import {statusLabels} from '../shared/status.js';
import {timestampMillis} from '../shared/freshness.js';
import {getLocale, t, tParts} from '../shared/i18n.js';
import {formatLoadNumber} from '../shared/format.js';

const reportFormat = new Intl.DateTimeFormat(getLocale(), {month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false});
const integerFormat = new Intl.NumberFormat(getLocale());
const percentFormat = new Intl.NumberFormat(getLocale(), {style: 'percent', minimumFractionDigits: 1, maximumFractionDigits: 1});

export const STATUS = statusLabels({unknown: 'public.status.unknown'});

function duration(seconds) {
  if (seconds === null) return '—';
  const minutes = Math.floor(seconds / 60);
  const days = Math.floor(minutes / 1440);
  const hours = Math.floor(minutes % 1440 / 60);
  if (days) return t('public.duration.daysHours', {days: integerFormat.format(days), hours: integerFormat.format(hours)});
  if (hours) return t('public.duration.hoursMinutes', {hours: integerFormat.format(hours), minutes: integerFormat.format(minutes % 60)});
  return t('public.duration.minutes', {minutes: integerFormat.format(minutes % 60)});
}

function gauge(label) {
  const value = h('span', {class: 'gauge-value num'});
  const fill = h('span', {class: 'fill'});
  const track = h('span', {class: 'track', role: 'meter', 'aria-label': label, 'aria-valuemin': '0', 'aria-valuemax': '100'}, fill);
  const element = h('div', {class: 'gauge'}, h('div', {class: 'gauge-line'}, h('span', null, label), value), track, h('span', {class: 'na'}, t('public.metric.noData')));
  element.set = (percent) => {
    const missing = percent === null;
    element.classList.toggle('missing', missing);
    const display = missing ? '—' : percentFormat.format(Number(percent.toFixed(1)) / 100);
    value.textContent = display;
    if (missing) {
      track.removeAttribute('aria-valuenow');
      track.removeAttribute('aria-valuetext');
      return;
    }
    track.setAttribute('aria-valuenow', percent.toFixed(1));
    track.setAttribute('aria-valuetext', display);
    element.dataset.level = percent >= 90 ? 'critical' : percent >= 75 ? 'high' : 'normal';
    fill.style.setProperty('--v', String(percent / 100));
  };
  return element;
}

// 卡片只创建一次，后续根据已校验的快照增量更新。
// 连接故障只标记数据可能过期，不改变节点的在线状态或数值。
export function createCard() {
  const name = h('h2');
  const badge = h('span', {class: 'status'});
  const region = h('span', null, icon('pin'), h('span'));
  const provider = h('span', null, icon('cloud'), h('span'));
  const where = h('p', {class: 'where'}, region, provider);
  const cpu = gauge(t('public.metric.cpu'));
  const memory = gauge(t('public.metric.memory'));
  const load = h('dd', {class: 'num'});
  const uptime = h('dd', {class: 'num'});
  const lastReport = h('p', {class: 'last-report', hidden: true});
  let current;
  let connectionStale = false;
  const paintFreshness = () => {
    if (!current) return;
    const states = current.metric_status;
    for (const [key, element] of [['cpu_percent', cpu], ['memory_percent', memory], ['load1', load], ['uptime_seconds', uptime]]) {
      const stale = current[key] !== null && (states[key] === 'stale' || connectionStale);
      element.classList.toggle('metric-stale', stale);
    }
    const hasTime = Number.isFinite(timestampMillis(current.last_seen));
    lastReport.hidden = !hasTime || !(connectionStale || current.status === 'offline' || Object.values(states).includes('stale'));
  };
  const article = h('article', {class: 'card'},
    h('header', {class: 'card-head'}, name, badge), where,
    h('div', {class: 'gauges'}, cpu, memory),
    h('dl', {class: 'stats'}, h('div', null, h('dt', null, t('public.metric.load')), load), h('div', null, h('dt', null, t('public.metric.uptime')), uptime)), lastReport);
  const item = h('li', {class: 'node'}, article);
  article.addEventListener('animationend', (event) => {
    if (event.target === article && event.animationName === 'status-changed') article.classList.remove('changed');
  });
  item.update = (node) => {
    // 已显示数据的卡片在线状态变化时，以新状态色短暂闪烁一圈。
    if (current && current.status !== node.status) {
      article.classList.remove('changed');
      void article.offsetWidth;
      article.classList.add('changed');
    }
    current = node;
    connectionStale = false;
    item.dataset.status = node.status;
    name.textContent = node.name;
    badge.className = `status ${node.status}`;
    badge.textContent = STATUS[node.status];
    region.lastChild.textContent = node.region;
    region.hidden = !node.region;
    provider.lastChild.textContent = node.provider;
    provider.hidden = !node.provider;
    where.hidden = !node.region && !node.provider;
    cpu.set(node.cpu_percent);
    memory.set(node.memory_percent);
    load.textContent = node.load1 === null ? '—' : formatLoadNumber(node.load1);
    uptime.textContent = duration(node.uptime_seconds);
    const received = timestampMillis(node.last_seen);
    lastReport.replaceChildren(...(Number.isFinite(received)
      ? tParts('public.lastReport', {time: h('time', {datetime: node.last_seen, title: node.last_seen}, reportFormat.format(new Date(received)))}) : []));
    paintFreshness();
  };
  item.setStale = (stale = true) => { connectionStale = stale; paintFreshness(); };
  return item;
}

export function skeletonCards(count) {
  return Array.from({length: count}, () => h('li', {class: 'node placeholder', 'aria-hidden': 'true'},
    h('div', {class: 'card'}, h('span', {class: 'sk w60'}), h('span', {class: 'sk w40'}), h('span', {class: 'sk bar'}), h('span', {class: 'sk bar'}), h('span', {class: 'sk w80'}))));
}
