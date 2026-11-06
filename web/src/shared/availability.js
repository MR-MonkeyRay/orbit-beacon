import {h} from './dom.js';
import {formatDuration} from './format.js';
import {getLocale, t} from './i18n.js';

const DAY = 86400000;
// 不足一分钟也要可见：离线 30 秒不能显示成 0 分钟。
const span = (seconds) => formatDuration(Math.ceil(seconds / 60) * 60);
const percent = (value) => value === null ? t('public.availabilityNoData') : `${new Intl.NumberFormat(getLocale(), {maximumFractionDigits: 2}).format(value)}%`;
export const availabilityLevel = (value) => value === null ? 'neutral' : value >= 99.9 ? 'normal' : value >= 95 ? 'warning' : 'critical';

// Keep the same thirty elements when a public snapshot updates.
export function availabilityBar() {
  const bars = Array.from({length: 30}, () => h('span', {class: 'availability-day', 'aria-hidden': 'true'}));
  const total = h('span', {class: 'availability-total num', 'aria-hidden': 'true'});
  const root = h('div', {class: 'availability', role: 'img'}, h('div', {class: 'availability-days'}, bars), total);
  root.update = (data) => {
    const start = Date.parse(`${data.start_day}T00:00:00Z`);
    const end = new Date(start + 29 * DAY).toISOString().slice(0, 10);
    const summary = percent(data.percent);
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
