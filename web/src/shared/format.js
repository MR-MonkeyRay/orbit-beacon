import {getLocale, t} from './i18n.js';

const loadFormat = new Intl.NumberFormat(getLocale(), {useGrouping: false, minimumFractionDigits: 2, maximumFractionDigits: 2});

export const formatLoadNumber = (value) => loadFormat.format(Number(value.toFixed(2)));

export function formatDuration(seconds) {
  if (seconds === null) return '—';
  const integer = new Intl.NumberFormat(getLocale());
  const minutes = Math.floor(seconds / 60);
  const days = Math.floor(minutes / 1440);
  const hours = Math.floor(minutes % 1440 / 60);
  if (days) return t('public.duration.daysHours', {days: integer.format(days), hours: integer.format(hours)});
  if (hours) return t('public.duration.hoursMinutes', {hours: integer.format(hours), minutes: integer.format(minutes % 60)});
  return t('public.duration.minutes', {minutes: integer.format(minutes % 60)});
}

// 使用本地日历判断日期范围，始终采用应用语言与 24 小时制。格式器按语言缓存，避免实时刷新时反复创建。
const clock = {hour: '2-digit', minute: '2-digit', hourCycle: 'h23'};
let formats = {locale: null};
function timestampFormats() {
  const locale = getLocale();
  if (formats.locale !== locale) formats = {locale,
    day: new Intl.DateTimeFormat(locale, {...clock, second: '2-digit'}),
    year: new Intl.DateTimeFormat(locale, {...clock, month: 'short', day: 'numeric'}),
    other: new Intl.DateTimeFormat(locale, {...clock, month: 'short', day: 'numeric', year: 'numeric'}),
    full: new Intl.DateTimeFormat(locale, {...clock, second: '2-digit', month: 'short', day: 'numeric', year: 'numeric'})};
  return formats;
}

export function formatTimestamp(date, now = new Date()) {
  const sameYear = date.getFullYear() === now.getFullYear();
  const sameDay = sameYear && date.getMonth() === now.getMonth() && date.getDate() === now.getDate();
  return timestampFormats()[sameDay ? 'day' : sameYear ? 'year' : 'other'].format(date);
}

export const formatFullTimestamp = (date) => timestampFormats().full.format(date);
