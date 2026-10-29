import {t} from '../i18n.js';
import {absoluteWhen} from '../format.js';
import {patchStatusBadge} from '../ui/components.js';
import {tombstoneNodeRow} from './nodes-table.js';

export function patchNodesFromFleet(data, state) {
  const {nodes, filter, rowRefs, deleted, selected, visibleIDs} = state;
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
      if (refs) tombstoneNodeRow(refs);
      continue;
    }
    if (deleted.has(node.id)) continue;
    if (fresh.presence !== node.presence && (filter.status || filter.sort === 'status')) statusChanged = true;
    const seenChanged = fresh.last_seen !== node.last_seen;
    Object.assign(node, fresh);
    if (refs) {
      patchStatusBadge(refs.badge, node.presence);
      if (seenChanged) refs.lastSeen.replaceChildren(node.last_seen ? absoluteWhen(node.last_seen) : t('nodes.never'));
    }
  }
  return {inventoryChanged, statusChanged, selectionRemoved};
}
