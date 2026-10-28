import {timestampMillis} from '../shared/freshness.js';

const METRICS = ['cpu_percent', 'memory_percent', 'load1', 'uptime_seconds'];

// Invalid data never replaces the last validated public collection.
export function validResponse(result, offset = 0) {
  if (!result || !Array.isArray(result.nodes) || result.nodes.length > 100 || typeof result.has_more !== 'boolean' || !Number.isFinite(timestampMillis(result.generated_at))) return false;
  if (result.has_more ? !Number.isSafeInteger(result.next_offset) || result.next_offset <= offset : result.next_offset !== null) return false;
  return result.nodes.every((node) => {
    if (!node || !['name', 'region', 'provider'].every((key) => typeof node[key] === 'string') || !['online', 'offline', 'unknown'].includes(node.status)) return false;
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
