import {h} from './dom.js';
import {formatDuration} from './format.js';
import {getLocale, t} from './i18n.js';

const DAY = 86400000;
// 不足一分钟也要可见：离线 30 秒不能显示成 0 分钟。
const span = (seconds) => formatDuration(Math.ceil(seconds / 60) * 60);
const percent = (value) => value === null ? t('public.availabilityNoData') : `${new Intl.NumberFormat(getLocale(), {maximumFractionDigits: 2}).format(value)}%`;
export const availabilityLevel = (value) => value === null ? 'neutral' : value >= 99.9 ? 'normal' : value >= 95 ? 'warning' : 'critical';

// 同一范围的快照更新复用每日条形；范围取自实际天数。
export function availabilityBar() {
  // 触屏没有 hover：点按某天时把它的 title 显示在下方说明行。
  const detail = h('span', {class: 'availability-detail', hidden: true});
  const bars = [];
  const createDay = () => {
    const bar = h('span', {class: 'availability-day', 'aria-hidden': 'true'});
    bar.addEventListener('click', () => { detail.textContent = bar.getAttribute('title'); detail.hidden = false; });
    return bar;
  };
  const total = h('span', {class: 'availability-total num', 'aria-hidden': 'true'});
  const from = h('span');
  const range = h('div', {class: 'availability-range', 'aria-hidden': 'true'}, from, h('span', null, t('public.today')));
  const days = h('div', {class: 'availability-days'}, bars);
  const root = h('div', {class: 'availability', role: 'img'}, h('div', {class: 'availability-window'}, days, range), total, detail);
  root.update = (data) => {
    while (bars.length > data.days.length) bars.pop().remove();
    while (bars.length < data.days.length) { const bar = createDay(); bars.push(bar); days.append(bar); }
    days.style.gridTemplateColumns = `repeat(${data.days.length},minmax(0,1fr))`;
    const start = Date.parse(`${data.start_day}T00:00:00Z`);
    const end = new Date(start + (data.days.length - 1) * DAY).toISOString().slice(0, 10);
    const summary = percent(data.percent);
    from.textContent = t('public.availabilityRange', {count: data.days.length});
    root.setAttribute('aria-label', `${t('public.availability')}: ${data.start_day} – ${end}, ${summary}`);
    total.textContent = summary;
    bars.forEach((bar, i) => {
      const value = data.days[i];
      const day = new Date(start + i * DAY).toISOString().slice(0, 10);
      bar.className = `availability-day availability-${availabilityLevel(value)}`;
      const detail = data.details?.[i];
      bar.setAttribute('title', `${day}: ${percent(value)}${detail ? `; ${t('public.availabilityDuration', {offline: span(detail.offline_seconds), unknown: span(detail.unknown_seconds)})}` : ''}`);
    });
  };
  return root;
}
