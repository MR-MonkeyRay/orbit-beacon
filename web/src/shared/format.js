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
