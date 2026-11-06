import {timestampMillis} from '../shared/freshness.js';

export function validAnnouncements(items) {
  return Array.isArray(items) && items.length <= 5 && Array.from(items).every((item) =>
    item && typeof item === 'object' && !Array.isArray(item) && Object.keys(item).length === 5 &&
    ['level', 'title', 'body', 'starts_at', 'ends_at'].every((key) => Object.hasOwn(item, key)) &&
    ['info', 'warning', 'critical'].includes(item.level) &&
    ['title', 'body'].every((key) => typeof item[key] === 'string' && item[key].trim().length > 0) &&
    Number.isFinite(timestampMillis(item.starts_at)) &&
    (item.ends_at === null || Number.isFinite(timestampMillis(item.ends_at))));
}

const METRICS = ['cpu_percent', 'memory_percent', 'load1', 'uptime_seconds'];

const validPercent = (value) => value === null || typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 100;
export function validAvailability(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).length !== 3) return false;
  if (typeof value.start_day !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(value.start_day)) return false;
  const date = new Date(`${value.start_day}T00:00:00Z`);
  return Number.isFinite(date.getTime()) && date.toISOString().slice(0, 10) === value.start_day &&
    validPercent(value.percent) && Array.isArray(value.days) && value.days.length === 30 && Array.from(value.days).every(validPercent);
}

// 无效数据绝不替换最后一次校验通过的公开集合。
export function validResponse(result, offset = 0) {
  if (!result || !validAnnouncements(result.announcements) || !Array.isArray(result.nodes) || result.nodes.length > 100 || typeof result.has_more !== 'boolean' || !Number.isFinite(timestampMillis(result.generated_at))) return false;
  if (result.has_more ? !Number.isSafeInteger(result.next_offset) || result.next_offset <= offset : result.next_offset !== null) return false;
  return result.nodes.every((node) => {
    if (!node || !['name', 'region', 'provider'].every((key) => typeof node[key] === 'string') || !['online', 'offline', 'unknown'].includes(node.status)) return false;
    if (!validAvailability(node.availability)) return false;
    if (node.last_seen !== null && !Number.isFinite(timestampMillis(node.last_seen))) return false;
    if (!node.metric_status || typeof node.metric_status !== 'object' || Array.isArray(node.metric_status) || Object.keys(node.metric_status).length !== METRICS.length) return false;
    return METRICS.every((key) => {
      const value = node[key];
      const status = node.metric_status[key];
      if (status === 'unavailable') return value === null;
      return (status === 'ok' || status === 'stale') && typeof value === 'number' && Number.isFinite(value) && value >= 0 && (!key.endsWith('_percent') || value <= 100);
    });
  });
}
