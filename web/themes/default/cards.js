import {usageLevel} from '../../src/shared/usage.js';
import {availabilityBar} from '../../src/shared/availability.js';
import {h, icon} from '../../src/shared/dom.js';
import {statusLabels} from '../../src/shared/status.js';
import {timestampMillis} from '../../src/shared/freshness.js';
import {getLocale, t, tParts} from '../../src/shared/i18n.js';
import {formatDuration, formatLoadNumber, formatTimestamp} from '../../src/shared/format.js';

const percentFormat = new Intl.NumberFormat(getLocale(), {style: 'percent', maximumFractionDigits: 0});

const STATUS = statusLabels({unknown: 'public.status.unknown'});

function gauge(label) {
  const value = h('span', {class: 'gauge-value num'});
  const fill = h('span', {class: 'fill'});
  const track = h('span', {class: 'track', role: 'meter', 'aria-label': label, 'aria-valuemin': '0', 'aria-valuemax': '100'}, fill);
  const element = h('div', {class: 'gauge'}, h('div', {class: 'gauge-line'}, h('span', null, label), value), track, h('span', {class: 'na'}, t('public.metric.noData')));
  let baseText = '';
  let stale = false;
  const publish = () => { track.setAttribute('aria-valuetext', stale ? t('public.metric.staleValue', {value: baseText, note: t('public.metric.staleNote')}) : baseText); };
  element.set = (percent) => {
    const missing = percent === null;
    element.classList.toggle('missing', missing);
    const display = missing ? '—' : percentFormat.format(percent / 100);
    const level = usageLevel(percent);
    element.classList.toggle('usage-warn', level === 'warn');
    element.classList.toggle('usage-bad', level === 'bad');
    value.textContent = display;
    if (missing) {
      baseText = '';
      track.removeAttribute('aria-valuenow');
      track.removeAttribute('aria-valuetext');
      return;
    }
    track.setAttribute('aria-valuenow', percent.toFixed(1));
    baseText = display;
    publish();
    fill.style.setProperty('--v', String(percent / 100));
  };
  element.setStale = (next) => {
    stale = next;
    if (baseText) publish();
  };
  return element;
}

// 卡片只创建一次，后续根据已校验的快照增量更新。
// 连接故障只标记数据可能过期，不改变节点的在线状态或数值。
export function createCard() {
  const availability = availabilityBar();
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
    const states = current.metric_status || {};
    for (const [key, element] of [['cpu_percent', cpu], ['memory_percent', memory]]) {
      const stale = current[key] != null && (states[key] === 'stale' || connectionStale);
      element.classList.toggle('metric-stale', stale);
      element.setStale(stale);
    }
    for (const [key, element] of [['load1', load], ['uptime_seconds', uptime]]) {
      const stale = current[key] != null && (states[key] === 'stale' || connectionStale);
      element.classList.toggle('metric-stale', stale);
      if (stale) element.title = t('public.metric.staleNote'); else element.removeAttribute('title');
    }
    const hasTime = Number.isFinite(timestampMillis(current.last_seen));
    lastReport.hidden = !hasTime || !(connectionStale || current.status === 'offline' || Object.values(states).includes('stale'));
  };
  const gauges = h('div', {class: 'gauges'}, cpu, memory);
  const stats = h('dl', {class: 'stats'}, h('div', null, h('dt', null, t('public.metric.load')), load), h('div', null, h('dt', null, t('public.metric.uptime')), uptime));
  const extras = h('div', {class: 'stats'});
  const article = h('article', {class: 'card'},
    h('header', {class: 'card-head'}, name, badge), where,
    gauges, stats, availability, extras, lastReport);
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
    availability.hidden = !('availability' in node);
    if (node.availability) availability.update(node.availability);
    connectionStale = false;
    item.dataset.status = node.status;
    name.textContent = node.name;
    badge.className = `status ${node.status}`;
    badge.textContent = STATUS[node.status];
    region.lastChild.textContent = [node.region, node.country].filter(Boolean).join(' · ');
    region.hidden = !node.region && !node.country;
    provider.lastChild.textContent = node.provider || '';
    provider.hidden = !node.provider;
    where.hidden = !node.region && !node.country && !node.provider;
    cpu.hidden = !('cpu_percent' in node);
    cpu.set(node.cpu_percent ?? null);
    memory.hidden = !('memory_percent' in node);
    memory.set(node.memory_percent ?? null);
    load.parentElement.hidden = !('load1' in node);
    uptime.parentElement.hidden = !('uptime_seconds' in node);
    gauges.hidden = cpu.hidden && memory.hidden;
    stats.hidden = load.parentElement.hidden && uptime.parentElement.hidden;
    load.textContent = node.load1 == null ? '—' : formatLoadNumber(node.load1);
    uptime.textContent = formatDuration(node.uptime_seconds ?? null);
    const lines = [];
    if ('price' in node || 'billing_cycle' in node) lines.push(h('p', null, [node.price ? `${node.price.currency} ${node.price.amount}`.trim() : '', node.billing_cycle ? t(`public.cycle.${node.billing_cycle}`) : '', node.auto_renew ? t('public.field.auto_renew') : ''].filter(Boolean).join(' · ') || '—'));
    if (node.ip_version) lines.push(h('p', {class: 'cell-tags'}, ['ipv4', 'ipv6'].filter((key) => node.ip_version[key]).map((key) => h('span', {class: 'tag static'}, key.toUpperCase()))));
    for (const key of ['bandwidth', 'network_route', 'plan_extra', 'expires_on']) {
      if (key in node) lines.push(h('p', null, t(`public.field.${key}`), ': ', node[key] || '—'));
    }
    if (node.traffic) lines.push(h('p', null, t('public.field.traffic'), ': ', node.traffic.quota_bytes, ' B'));
    extras.replaceChildren(...lines);
    extras.hidden = lines.length === 0;
    const received = timestampMillis(node.last_seen);
    lastReport.replaceChildren(...(Number.isFinite(received)
      ? tParts('public.lastReport', {time: h('time', {datetime: node.last_seen, title: node.last_seen}, formatTimestamp(new Date(received)))}) : []));
    paintFreshness();
  };
  item.setStale = (stale = true) => { connectionStale = stale; paintFreshness(); };
  return item;
}

export function skeletonCards(count) {
  return Array.from({length: count}, () => h('li', {class: 'node placeholder', 'aria-hidden': 'true'},
    h('div', {class: 'card'}, h('span', {class: 'sk w60'}), h('span', {class: 'sk w40'}), h('span', {class: 'sk bar'}), h('span', {class: 'sk bar'}), h('span', {class: 'sk w80'}))));
}
