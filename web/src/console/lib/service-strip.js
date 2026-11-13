import {h} from '../../shared/dom.js';
import {availabilityLevel} from '../../shared/availability.js';
import {getLocale} from '../i18n.js';
import {formatPercent} from '../format.js';

const HOUR = 3600000;

// recent_hours 共 24 项：末项是当前 UTC 小时（尚未结束），首项早 23 小时。
// 条形只作视觉概览，旁边的百分比和列标题承担可读信息，悬停可看每小时数值。
export function recentStrip(hours, now = Date.now()) {
  if (!Array.isArray(hours) || hours.length !== 24) return null;
  const current = Math.floor(now / HOUR) * HOUR;
  const clock = new Intl.DateTimeFormat(getLocale(), {hour: '2-digit', minute: '2-digit'});
  return h('span', {class: 'service-strip', 'aria-hidden': 'true'}, hours.map((value, index) =>
    h('span', {class: `availability-day availability-${availabilityLevel(value)}`,
      title: `${clock.format(current - (23 - index) * HOUR)} ${formatPercent(value)}`})));
}
