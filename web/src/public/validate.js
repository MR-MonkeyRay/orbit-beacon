import {timestampMillis} from '../shared/freshness.js';
import {byteLength} from '../shared/dom.js';

const METRICS = ['cpu_percent', 'memory_percent', 'load1', 'uptime_seconds'];

const validPercent = (value) => value === null || typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 100;
export function validAvailability(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).length !== 3) return false;
  if (typeof value.start_day !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(value.start_day)) return false;
  const date = new Date(`${value.start_day}T00:00:00Z`);
  return Number.isFinite(date.getTime()) && date.toISOString().slice(0, 10) === value.start_day &&
    validPercent(value.percent) && Array.isArray(value.days) && value.days.length === 30 && Array.from(value.days).every(validPercent);
}

const validName = (name) => typeof name === 'string' && [...name].length >= 1 && [...name].length <= 100 && !/[\u0000-\u001f\u007f-\u009f]/.test(name);
// 状态页开启分组时，响应带 node_groups / service_groups（只有名称），成员用 group 下标引用，未分组为 null。
// 分组名与后端规则一致：非空白、最多 255 字节；节点分组没有数量上限，监测分组最多 100 个。
const validGroupName = (name) => typeof name === 'string' && name.trim() !== '' && byteLength(name) <= 255;
const validGroups = (groups, max = Infinity) => Array.isArray(groups) && groups.length <= max && groups.every(validGroupName);
const validGroupIndex = (value, groups) => value === null || Number.isSafeInteger(value) && value >= 0 && value < groups.length;

// 无效数据绝不替换最后一次校验通过的公开集合。groups 为 null 时不允许 group 字段。
export function validService(service, groups = null) {
 const keys = ['name','type','status','latency_ms','availability', ...(groups ? ['group'] : [])];
 if (!service || typeof service !== 'object' || Array.isArray(service) || Object.keys(service).length !== keys.length || Object.keys(service).some((key) => !keys.includes(key))) return false;
 if (groups && !validGroupIndex(service.group, groups)) return false;
 return validName(service.name) && ['http','tcp','icmp'].includes(service.type) && ['up','down','unknown'].includes(service.status) && (service.latency_ms === null || typeof service.latency_ms === 'number' && Number.isFinite(service.latency_ms) && service.latency_ms >= 0) && Array.isArray(service.availability) && service.availability.length === 30 && Array.from(service.availability).every(validPercent);
}
export function validResponse(result, offset = 0) {
  const grouped = result && ('node_groups' in result || 'service_groups' in result);
  if (grouped && (!validGroups(result.node_groups) || !validGroups(result.service_groups, 100))) return false;
  const serviceGroups = grouped ? result.service_groups : null, nodeGroups = grouped ? result.node_groups : null;
 if (!Array.isArray(result?.services) || result.services.length > 100 || !result.services.every((service) => validService(service, serviceGroups))) return false;
  if (!result || !Array.isArray(result.nodes) || result.nodes.length > 100 || typeof result.has_more !== 'boolean' || !Number.isFinite(timestampMillis(result.generated_at))) return false;
  if (result.has_more ? !Number.isSafeInteger(result.next_offset) || result.next_offset <= offset : result.next_offset !== null) return false;
  return result.nodes.every((node) => {
    if (!node || typeof node.name !== 'string' || !['online', 'offline', 'unknown'].includes(node.status)) return false;
    if (node.last_seen !== null && !Number.isFinite(timestampMillis(node.last_seen))) return false;
    const allowed = ['name', 'status', 'last_seen', 'region', 'country', 'provider', ...METRICS, 'metric_status', 'availability', 'traffic', 'expires_on', 'price', 'billing_cycle', 'auto_renew', 'bandwidth', 'ip_version', 'network_route', 'plan_extra', ...(nodeGroups ? ['group'] : [])];
    if (Object.keys(node).some((key) => !allowed.includes(key))) return false;
    if (nodeGroups && !validGroupIndex(node.group, nodeGroups)) return false;
    for (const key of ['region', 'country', 'provider', 'billing_cycle', 'bandwidth', 'network_route', 'plan_extra']) {
      if (key in node && typeof node[key] !== 'string') return false;
    }
    if ('country' in node && !/^(?:[A-Z]{2})?$/.test(node.country)) return false;
    if ('availability' in node && !validAvailability(node.availability)) return false;
    if ('expires_on' in node && node.expires_on !== null && (typeof node.expires_on !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(node.expires_on))) return false;
    if ('auto_renew' in node && typeof node.auto_renew !== 'boolean') return false;
    if ('billing_cycle' in node && !['', 'month', 'quarter', 'half_year', 'year', 'once'].includes(node.billing_cycle)) return false;
    if ('price' in node && node.price !== null && (!node.price || typeof node.price.amount !== 'string' || !/^[0-9]+(?:\.[0-9]{1,2})?$/.test(node.price.amount) || node.price.amount.length > 16 || typeof node.price.currency !== 'string')) return false;
    if ('ip_version' in node && (!node.ip_version || typeof node.ip_version.ipv4 !== 'boolean' || typeof node.ip_version.ipv6 !== 'boolean')) return false;
    if ('traffic' in node && (!node.traffic || typeof node.traffic.quota_bytes !== 'string' || !/^(?:0|[1-9][0-9]*)$/.test(node.traffic.quota_bytes))) return false;
    const selected = METRICS.filter((key) => key in node);
    if (selected.length === 0) return !('metric_status' in node);
    if (!node.metric_status || typeof node.metric_status !== 'object' || Array.isArray(node.metric_status) || Object.keys(node.metric_status).length !== selected.length) return false;
    return selected.every((key) => {
      const value = node[key], status = node.metric_status[key];
      if (status === 'unavailable') return value === null;
      return (status === 'ok' || status === 'stale') && typeof value === 'number' && Number.isFinite(value) && value >= 0 && (!key.endsWith('_percent') || value <= 100);
    });
  });
}
