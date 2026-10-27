import {h} from '../../shared/dom.js';
import {ensureSession} from '../api.js';
import {emptyState, linkButton, pageHead} from '../ui/components.js';

export async function alertsPage() {
  await ensureSession();
  return h('div', null, pageHead('Alerts'), h('div', {class: 'panel'},
    emptyState('Alerts are not available yet', 'Alert rules and notifications are not part of this release. Node inventory and the public status page are available today.',
      linkButton('/nodes', 'Go to nodes'))));
}
