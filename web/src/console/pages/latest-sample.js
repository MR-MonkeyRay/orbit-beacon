import {t, tParts} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {usageMeter} from '../../shared/usage.js';
import {createNumberTween} from '../../shared/tween.js';
import {createFreshnessController, sourceDeadline, timestampMillis} from '../../shared/freshness.js';
import {absoluteWhen, formatBytes, formatCelsius, formatDuration, formatLoad, formatPercent, formatRate} from '../format.js';
import {panel} from '../ui/components.js';
import {highestDisk, metricBytes as bytes, metricNumber as number, networkRate, usedPercent} from '../lib/latest-sample-helpers.js';

const usableStatus = (value) => value === 'ok' || value === 'stale';
const percent = (value) => number(value) && value <= 100;
const setText = (element, text) => { if (element.textContent !== text) element.textContent = text; };

// Keep tiles, meters and keyed table rows in place across live snapshots.
export function latestSamplePanel(node, initial, {signal} = {}) {
  const body = h('div');
  const element = panel(t('nodes.sample'), {}, body);
  const tween = createNumberTween({signal});
  const slots = new Map();
  let records = [];
  let closed = false;
  const metric = (id, metered = false) => {
    if (slots.has(id)) return slots.get(id).element;
    const meter = metered ? usageMeter(null) : null;
    const value = meter ? meter.lastElementChild : h('span');
    value.classList.add('sample-value');
    const state = h('small', {class: 'sample-state', hidden: true});
    const record = {value, meter, state, element: h('span', {class: 'sample-metric'}, meter || value, state)};
    slots.set(id, record);
    return record.element;
  };
  const fact = (label, content, className = '') => h('div', {class: className}, h('dt', null, t(label)), h('dd', null, content));
  const memoryDetail = h('small', {class: 'sample-detail'});
  const diskDetail = h('small', {class: 'sample-detail'});
  const diskPath = h('span');
  const diskCapacity = h('span');
  const diskInodes = h('span');
  diskDetail.append(diskPath, ' ', diskCapacity, ' ', diskInodes);
  const loadDetail = h('small', {class: 'sample-detail sample-load'},
    tParts('nodes.loadSecondary', {five: metric('load5'), fifteen: metric('load15')}));
  const primary = h('dl', {class: 'sample-primary'},
    fact('nodes.cpu', metric('cpu', true), 'sample-tile'),
    fact('nodes.memory', [metric('memory', true), memoryDetail], 'sample-tile'),
    fact('nodes.disk', [metric('disk', true), diskDetail], 'sample-tile'),
    fact('nodes.loadMinute', [metric('load1'), loadDetail], 'sample-tile'));
  const direction = (label, id) => h('span', {class: 'sample-rate'}, h('small', null, t(label)), metric(id));
  const networkName = h('small', {class: 'sample-detail'});
  const rates = h('dl', {class: 'sample-rates'},
    fact('nodes.network', [h('div', {class: 'sample-directions'}, direction('nodes.receiveRx', 'rx'), direction('nodes.sendTx', 'tx')), networkName]),
    fact('nodes.diskIO', h('div', {class: 'sample-directions'}, direction('nodes.diskRead', 'read'), direction('nodes.diskWrite', 'write'), direction('nodes.diskBusy', 'busy'))));
  const agent = h('span');
  const sampled = h('span');
  const secondary = h('dl', {class: 'sample-secondary'},
    fact('nodes.pressure', h('span', null, tParts('nodes.pressureSummary', {
      cpu: metric('pressure-cpu'), memory: metric('pressure-memory'), io: metric('pressure-io')}))),
    fact('nodes.temperature', metric('temperature')),
    fact('nodes.uptime', metric('uptime')), fact('nodes.agentVersion', agent), fact('nodes.sampled', sampled));
  const table = (label, columns, className) => {
    const rows = new Map();
    const tbody = h('tbody');
    const wrapper = h('div', {class: `sample-table ${className}`, hidden: true}, h('table', {class: 'table'},
      h('caption', {class: 'sr'}, t(label)), h('thead', null, h('tr', null, columns.map((key) => h('th', {scope: 'col'}, t(key))))), tbody));
    return {wrapper, update(items, key, cells) {
      wrapper.hidden = items.length <= 1;
      const keys = new Set(items.map(key));
      for (const [id, row] of rows) if (!keys.has(id)) {
        for (const [slotID, record] of slots) if (row.contains(record.element)) { tween.finish(record.value); slots.delete(slotID); }
        row.remove(); rows.delete(id);
      }
      if (wrapper.hidden) return;
      items.forEach((item, index) => {
        const id = key(item);
        let row = rows.get(id);
        if (!row) { row = h('tr', null, cells(item).map((cell, column) => h(column ? 'td' : 'th', column ? null : {scope: 'row'}, cell))); rows.set(id, row); }
        if (tbody.children[index] !== row) tbody.insertBefore(row, tbody.children[index] || null);
      });
    }};
  };
  const disksTable = table('nodes.disk', ['nodes.diskPath', 'nodes.usedTotal', 'nodes.usagePercent', 'nodes.inode'], 'sample-disks');
  const networksTable = table('nodes.network', ['nodes.interface', 'nodes.receiveRx', 'nodes.sendTx'], 'sample-networks');
  const lastReport = h('p', {class: 'last-report', hidden: true});
  const content = h('div', {class: 'sample-dashboard'}, primary, rates, disksTable.wrapper, networksTable.wrapper, secondary, lastReport);
  const freshness = createFreshnessController({signal, onChange(now) {
    let next = Infinity;
    for (const record of records) {
      const old = record.available && (record.originalStale || !Number.isFinite(now) || now > record.deadline);
      record.value.classList.toggle('metric-stale', old);
      record.state.hidden = !old;
      setText(record.state, old ? t('nodes.stale') : '');
      if (record.available && !record.originalStale && record.deadline >= now) next = Math.min(next, record.deadline);
    }
    lastReport.hidden = !lastReport.querySelector('time');
    return Number.isFinite(next) ? next : null;
  }});
  const finish = () => { for (const {value} of slots.values()) tween.finish(value); };
  const close = () => {
    if (closed) return;
    closed = true;
    finish();
    freshness.close();
    signal?.removeEventListener('abort', close);
  };
  signal?.addEventListener('abort', close, {once: true});
  if (signal?.aborted) close();

  const snapshot = (latest, serverTime = latest?.server_time) => {
    if (closed || signal?.aborted) return;
    records = [];
    if (!latest) {
      finish();
      body.replaceChildren(h('p', {class: 'text-note'}, t('nodes.sampleEmpty')));
      freshness.refresh();
      return;
    }
    const metrics = latest.metrics || {};
    const sourceState = (keys, families) => {
      const statuses = keys.map((key) => metrics.metric_status?.[key]);
      const deadlines = families.map((family) => sourceDeadline(latest.received_at, metrics.sources?.[family]));
      return {available: statuses.every(usableStatus) && deadlines.every(Number.isFinite),
        deadline: Math.min(...deadlines), originalStale: statuses.includes('stale')};
    };
    const update = (id, value, format, keys = [], families = [], context) => {
      metric(id);
      const record = slots.get(id);
      const state = sourceState(keys, families);
      Object.assign(record, state, {available: state.available && value !== null});
      records.push(record);
      if (record.context !== context) { tween.set(record.value, null, () => '—'); record.context = context; }
      tween.set(record.value, record.available ? value : null, (current) => {
        if (record.meter) { record.meter.update(current); return record.value.textContent; }
        return current === null ? '—' : format(current);
      });
      return record.available;
    };
    update('cpu', percent(metrics.cpu_percent) ? metrics.cpu_percent : null, formatPercent, ['cpu_percent'], ['cpu']);
    const memory = bytes(metrics.memory_total_bytes) && bytes(metrics.memory_available_bytes) && metrics.memory_available_bytes <= metrics.memory_total_bytes
      ? metrics.memory_total_bytes - metrics.memory_available_bytes : null;
    const memoryAvailable = update('memory', usedPercent(memory, metrics.memory_total_bytes), formatPercent,
      ['memory_total_bytes', 'memory_available_bytes'], ['memory']);
    setText(memoryDetail, memoryAvailable ? `${formatBytes(memory)} / ${formatBytes(metrics.memory_total_bytes)}` : '—');
    for (const key of ['load1', 'load5', 'load15']) update(key, number(metrics[key]) ? metrics[key] : null, formatLoad, [key], ['load']);
    const disks = Array.isArray(metrics.disks) ? metrics.disks : [];
    const disk = highestDisk(disks);
    const diskKey = disk ? `disk:${disk.path}` : '';
    const diskAvailable = update('disk', disk ? usedPercent(disk.used_bytes, disk.total_bytes) : null, formatPercent, [diskKey], [diskKey]);
    setText(diskPath, diskAvailable ? disk.path : '—');
    setText(diskCapacity, diskAvailable ? `${formatBytes(disk.used_bytes)} / ${formatBytes(disk.total_bytes)}` : '—');
    diskInodes.hidden = disks.length !== 1;
    setText(diskInodes, `${t('nodes.inode')} ${diskAvailable && disk.inodes_total > 0 ? formatPercent(usedPercent(disk.inodes_used, disk.inodes_total)) : '—'}`);
    disksTable.update(disks, (item) => item.path, (item) => [item.path, metric(`disk-capacity:${item.path}`), metric(`disk-usage:${item.path}`), metric(`disk-inodes:${item.path}`)]);
    for (const item of disks.length > 1 ? disks : []) {
      const key = `disk:${item.path}`;
      const valid = usedPercent(item.used_bytes, item.total_bytes) !== null;
      // Capacity is a pair, so only its used value is tweened.
      update(`disk-capacity:${item.path}`, valid ? item.used_bytes : null, (used) => `${formatBytes(used)} / ${formatBytes(item.total_bytes)}`, [key], [key], item.total_bytes);
      update(`disk-usage:${item.path}`, usedPercent(item.used_bytes, item.total_bytes), formatPercent, [key], [key]);
      update(`disk-inodes:${item.path}`, item.inodes_total > 0 ? usedPercent(item.inodes_used, item.inodes_total) : null, formatPercent, [key], [key]);
    }
    const networks = Array.isArray(metrics.networks) ? metrics.networks : [];
    const networkKeys = networks.map((item) => `network:${item.name}`);
    for (const [id, direction] of [['rx', 'rx_bytes_per_sec'], ['tx', 'tx_bytes_per_sec']]) {
      update(id, networkRate(networks, direction), formatRate, networkKeys, networkKeys);
    }
    networkName.hidden = networks.length !== 1;
    setText(networkName, networks.length === 1 ? networks[0].name : '');
    networksTable.update(networks, (item) => item.name, (item) => [item.name, metric(`rx:${item.name}`), metric(`tx:${item.name}`)]);
    for (const item of networks.length > 1 ? networks : []) {
      const key = `network:${item.name}`;
      for (const [id, direction] of [['rx', 'rx_bytes_per_sec'], ['tx', 'tx_bytes_per_sec']]) update(`${id}:${item.name}`, number(item[direction]) ? item[direction] : null, formatRate, [key], [key]);
    }
    const io = metrics.disk_io;
    update('read', number(io?.read_bytes_per_sec) ? io.read_bytes_per_sec : null, formatRate);
    update('write', number(io?.write_bytes_per_sec) ? io.write_bytes_per_sec : null, formatRate);
    update('busy', percent(io?.busy_percent) ? io.busy_percent : null, formatPercent);
    for (const key of ['cpu', 'memory', 'io']) update(`pressure-${key}`, percent(metrics.pressure?.[key]) ? metrics.pressure[key] : null, formatPercent);
    update('temperature', number(metrics.temperature_celsius) ? metrics.temperature_celsius : null, formatCelsius);
    update('uptime', number(metrics.uptime_seconds) ? metrics.uptime_seconds : null, formatDuration, ['uptime_seconds'], ['uptime']);
    setText(agent, latest.system?.agent_version || node.agent_version || '—');
    const sampledTime = Number.isFinite(timestampMillis(latest.sampled_at)) ? absoluteWhen(latest.sampled_at) : null;
    if (sampledTime) sampledTime.title = `${sampledTime.getAttribute('title')} · ${t('nodes.sequence', {value: latest.seq})}`;
    const currentTime = sampled.querySelector('time');
    if (currentTime && sampledTime) {
      currentTime.setAttribute('datetime', sampledTime.getAttribute('datetime'));
      currentTime.title = sampledTime.title;
      setText(currentTime, sampledTime.textContent);
    } else sampled.replaceChildren(sampledTime || '—');
    lastReport.replaceChildren(...(Number.isFinite(timestampMillis(latest.received_at))
      ? tParts('nodes.lastReport', {time: absoluteWhen(latest.received_at)}) : []));
    if (!body.contains(content)) body.replaceChildren(content);
    if (!freshness.clock(serverTime)) freshness.refresh();
  };
  snapshot(initial);
  return {element, snapshot, clock: freshness.clock, attach: freshness.resume, detach() { finish(); freshness.pause(); }, close};
}
