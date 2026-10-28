// A malformed response is a failure, never an invitation to display stale data.
export function validResponse(result, offset = 0) {
  if (!result || !Array.isArray(result.nodes) || result.nodes.length > 100 || typeof result.has_more !== 'boolean' || !Number.isFinite(Date.parse(result.generated_at))) return false;
  if (result.has_more ? !Number.isSafeInteger(result.next_offset) || result.next_offset <= offset : result.next_offset !== null) return false;
  return result.nodes.every((node) => node && ['name', 'region', 'provider'].every((key) => typeof node[key] === 'string') && ['online', 'offline', 'unknown'].includes(node.status) && ['cpu_percent', 'memory_percent', 'load1', 'uptime_seconds'].every((key) => node[key] === null || typeof node[key] === 'number' && Number.isFinite(node[key]) && node[key] >= 0 && (!key.endsWith('_percent') || node[key] <= 100)));
}
