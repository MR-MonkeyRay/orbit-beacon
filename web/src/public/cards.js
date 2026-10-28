import {h, icon} from '../shared/dom.js';
import {timestampMillis} from '../shared/freshness.js';

const reportFormat = new Intl.DateTimeFormat('zh-CN', {month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false});

export const STATUS = {online: '在线', offline: '离线', unknown: '状态未知'};

export function duration(seconds) {
  if (seconds === null) return '—';
  const minutes = Math.floor(seconds / 60);
  const days = Math.floor(minutes / 1440);
  const hours = Math.floor(minutes % 1440 / 60);
  if (days) return `${days}天 ${hours}小时`;
  if (hours) return `${hours}小时 ${minutes % 60}分钟`;
  return `${minutes % 60}分钟`;
}

function gauge(label) {
  const value = h('span', {class: 'gauge-value num'});
  const fill = h('span', {class: 'fill'});
  const track = h('span', {class: 'track', role: 'meter', 'aria-label': label, 'aria-valuemin': '0', 'aria-valuemax': '100'}, fill);
  const element = h('div', {class: 'gauge'}, h('div', {class: 'gauge-line'}, h('span', null, label), value), track, h('span', {class: 'na'}, '暂无有效数据'));
  element.set = (percent) => {
    const missing = percent === null;
    element.classList.toggle('missing', missing);
    value.textContent = missing ? '—' : `${percent.toFixed(1)}%`;
    if (missing) {
      track.removeAttribute('aria-valuenow');
      return;
    }
    track.setAttribute('aria-valuenow', percent.toFixed(1));
    track.setAttribute('aria-valuetext', `${percent.toFixed(1)}%`);
    element.dataset.level = percent >= 90 ? 'critical' : percent >= 75 ? 'high' : 'normal';
    fill.style.setProperty('--v', String(percent / 100));
  };
  return element;
}

// A card is created once and patched for each validated snapshot. A connection
// fault only changes freshness styling, never the node's presence or values.
export function createCard() {
  const name = h('h2');
  const badge = h('span', {class: 'status'});
  const region = h('span', null, icon('pin'), h('span'));
  const provider = h('span', null, icon('cloud'), h('span'));
  const where = h('p', {class: 'where'}, region, provider);
  const cpu = gauge('CPU');
  const memory = gauge('内存');
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
    h('dl', {class: 'stats'}, h('div', null, h('dt', null, '1分钟负载'), load), h('div', null, h('dt', null, '运行时长'), uptime)), lastReport);
  const item = h('li', {class: 'node'}, article);
  item.update = (node) => {
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
    load.textContent = node.load1 === null ? '—' : node.load1.toFixed(2);
    uptime.textContent = duration(node.uptime_seconds);
    const received = timestampMillis(node.last_seen);
    lastReport.replaceChildren(...(Number.isFinite(received)
      ? ['最后数据 · ', h('time', {datetime: node.last_seen, title: node.last_seen}, reportFormat.format(new Date(received)))] : []));
    paintFreshness();
  };
  item.setStale = (stale = true) => { connectionStale = stale; paintFreshness(); };
  return item;
}

export function skeletonCards(count) {
  return Array.from({length: count}, () => h('li', {class: 'node placeholder', 'aria-hidden': 'true'},
    h('div', {class: 'card'}, h('span', {class: 'sk w60'}), h('span', {class: 'sk w40'}), h('span', {class: 'sk bar'}), h('span', {class: 'sk bar'}), h('span', {class: 'sk w80'}))));
}
