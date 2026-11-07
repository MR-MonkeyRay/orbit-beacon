import {t} from '../i18n.js';
import {displayStatus, absoluteWhen} from '../format.js';
import {patchStatusBadge} from '../ui/components.js';
import {metricText, tombstoneNodeRow} from './nodes-table.js';

export function patchNodesFromFleet(data, state) {
  const {nodes, filter, rowRefs, deleted, selected, visibleIDs, tween} = state;
  const current = new Map(data.nodes.map((node) => [node.id, node]));
  const known = new Set(nodes.map((node) => node.id));
  let inventoryChanged = data.nodes.some((node) => !known.has(node.id));
  let statusChanged = false;
  let selectionRemoved = false;
  for (const node of nodes) {
    const fresh = current.get(node.id);
    const refs = rowRefs.get(node.id);
    if (!fresh) {
      deleted.add(node.id);
      inventoryChanged = true;
      if (selected.delete(node.id)) selectionRemoved = true;
      visibleIDs.delete(node.id);
      if (refs) {
        tween.finish(refs.cpu);
        tween.finish(refs.memory);
        tombstoneNodeRow(refs);
      }
      continue;
    }
    if (deleted.has(node.id)) continue;
    if (displayStatus(fresh) !== displayStatus(node) && (filter.status || filter.sort === 'status')) statusChanged = true;
    if (fresh.agent_version !== node.agent_version && filter.sort === 'agent_version') statusChanged = true;
    const seenChanged = fresh.last_seen !== node.last_seen;
    Object.assign(node, fresh, {cpu_percent: fresh.cpu_percent ?? null, memory_used_percent: fresh.memory_used_percent ?? null});
    if (refs) {
      patchStatusBadge(refs.badge, displayStatus(node));
      tween.set(refs.cpu, node.cpu_percent, metricText);
      tween.set(refs.memory, node.memory_used_percent, metricText);
      refs.version.textContent = node.agent_version;
      if (seenChanged) refs.lastSeen.replaceChildren(node.last_seen ? absoluteWhen(node.last_seen) : t('nodes.never'));
    }
  }
  return {inventoryChanged, statusChanged, selectionRemoved};
}
