import {displayStatus} from '../format.js';
import {patchStatusBadge} from '../ui/components.js';
import {patchNodeUsage, patchNodeLastSeen, tombstoneNodeRow} from './nodes-table.js';
import {sortedNodes} from './nodes-filters.js';

const metricKey = {cpu: 'cpu_percent', memory: 'memory_used_percent'};
const order = (nodes, filter) => sortedNodes(nodes, filter).map((node) => node.id).join();

export function patchNodesFromFleet(data, state) {
  const {nodes, filter, rowRefs, deleted, selected, visibleIDs, tween} = state;
  const current = new Map(data.nodes.map((node) => [node.id, node]));
  const known = new Set(nodes.map((node) => node.id));
  let inventoryChanged = data.nodes.some((node) => !known.has(node.id));
  let statusChanged = false;
  let selectionRemoved = false;
  // 按用量排序时数值每帧都在变：只在顺序真正变化时提示；首次拿到指标直接重排。
  const key = metricKey[filter.sort];
  const before = key ? order(nodes, filter) : '';
  let metricsArrived = false;
  for (const node of nodes) {
    const fresh = current.get(node.id);
    const refs = rowRefs.get(node.id);
    if (!fresh) {
      deleted.add(node.id);
      inventoryChanged = true;
      if (selected.delete(node.id)) selectionRemoved = true;
      visibleIDs.delete(node.id);
      if (refs) {
        tween.finish(refs.cpu.firstElementChild.lastElementChild);
        tween.finish(refs.memory.firstElementChild.lastElementChild);
        tombstoneNodeRow(refs);
      }
      continue;
    }
    if (deleted.has(node.id)) continue;
    if (displayStatus(fresh) !== displayStatus(node) && (filter.status || filter.sort === 'status')) statusChanged = true;
    if (fresh.agent_version !== node.agent_version && filter.sort === 'agent_version') statusChanged = true;
    if (key && node[key] == null && fresh[key] != null) metricsArrived = true;
    Object.assign(node, fresh, {cpu_percent: fresh.cpu_percent ?? null, memory_used_percent: fresh.memory_used_percent ?? null});
    if (refs) {
      patchStatusBadge(refs.badge, displayStatus(node));
      patchNodeUsage(refs.cpu, node.cpu_percent, tween);
      patchNodeUsage(refs.memory, node.memory_used_percent, tween);
      refs.version.textContent = node.agent_version;
      patchNodeLastSeen(refs.lastSeen, node);
    }
  }
  if (key && order(nodes, filter) !== before) {
    if (metricsArrived) return {inventoryChanged, statusChanged, selectionRemoved, resort: true};
    statusChanged = true;
  }
  return {inventoryChanged, statusChanged, selectionRemoved};
}
