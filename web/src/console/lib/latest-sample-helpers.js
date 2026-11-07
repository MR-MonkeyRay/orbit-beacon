import {t} from '../i18n.js';
import {formatBytes, formatPercent, formatRate} from '../format.js';

function metricBytes(value) {
  return Number.isSafeInteger(value) && value >= 0 ? formatBytes(value) : t('common.unavailable');
}

export function memorySummary(metrics) {
  const total = metrics.memory_total_bytes;
  const available = metrics.memory_available_bytes;
  if (!Number.isSafeInteger(total) || !Number.isSafeInteger(available) || total < 0 || available < 0 || available > total) return t('common.unavailable');
  const used = total - available;
  return `${formatBytes(used)} / ${formatBytes(total)} (${formatPercent(total ? used / total * 100 : 0)})`;
}

export function diskSummary(disks) {
  if (!Array.isArray(disks) || disks.length === 0) return t('common.unavailable');
  return disks.map((disk) => {
    const total = disk.total_bytes;
    const used = disk.used_bytes;
    const usage = Number.isSafeInteger(total) && Number.isSafeInteger(used) && total >= 0 && used >= 0
      ? `${metricBytes(used)} / ${metricBytes(total)}${total ? ` (${formatPercent(used / total * 100)})` : ''}` : t('common.unavailable');
    return `${disk.path}: ${usage}`;
  }).join('\n');
}

export function networkSummary(networks) {
  if (!Array.isArray(networks) || networks.length === 0) return t('common.unavailable');
  return networks.map((network) => t('nodes.networkSummary', {name: network.name, rx: formatRate(network.rx_bytes_per_sec), tx: formatRate(network.tx_bytes_per_sec)})).join('\n');
}
