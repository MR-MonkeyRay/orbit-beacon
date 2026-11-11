import {h} from './dom.js';
import {getLocale} from './i18n.js';

export const USAGE_WARN = 80;
export const USAGE_BAD = 90;

export const usageLevel = (percent) => Number.isFinite(percent) ? percent > USAGE_BAD ? 'bad' : percent > USAGE_WARN ? 'warn' : '' : '';

let percentFormat = {locale: null};
const formatPercent = (value) => {
  if (percentFormat.locale !== getLocale()) percentFormat = {locale: getLocale(), format: new Intl.NumberFormat(getLocale(), {useGrouping: false, maximumFractionDigits: 1})};
  return `${percentFormat.format.format(Number(value.toFixed(1)))}%`;
};

// 控制台和主题共用：usageMeter(percent) 返回 DOM；meter.update(percent) 同步条宽、阈值色与文本。
export function usageMeter(percent) {
  const number = h('span', {class: 'usage-value'});
  const fill = h('i');
  const track = h('span', {class: 'usage-track', 'aria-hidden': 'true'}, fill);
  const meter = h('span', {class: 'usage-meter'}, track, number);
  meter.update = (value) => {
    const level = usageLevel(value);
    meter.className = `usage-meter${level ? ` usage-${level}` : ''}`;
    // 无数据时只留“—”，不画空条。
    track.hidden = !Number.isFinite(value);
    fill.style.width = `${Number.isFinite(value) ? Math.max(0, Math.min(100, value)) : 0}%`;
    number.textContent = Number.isFinite(value) ? formatPercent(value) : '—';
  };
  meter.update(percent);
  return meter;
}
