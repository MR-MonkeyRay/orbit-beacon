export const metricNumber = (value) => Number.isFinite(value) && value >= 0;
export const metricBytes = (value) => Number.isSafeInteger(value) && value >= 0;

export function usedPercent(used, total) {
  return metricBytes(used) && metricBytes(total) && used <= total ? (total ? used / total * 100 : 0) : null;
}

export function highestDisk(disks) {
  return disks.reduce((highest, disk) => {
    const percent = usedPercent(disk.used_bytes, disk.total_bytes);
    return percent !== null && (!highest || percent > usedPercent(highest.used_bytes, highest.total_bytes)) ? disk : highest;
  }, null);
}

// A total is unavailable if any interface lacks that direction's rate.
export function networkRate(networks, direction) {
  return networks.length && networks.every((item) => metricNumber(item[direction]))
    ? networks.reduce((sum, item) => sum + item[direction], 0) : null;
}
