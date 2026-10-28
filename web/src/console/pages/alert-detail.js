import {h} from '../../shared/dom.js';
import {api} from '../api.js';
import {when} from '../format.js';
import {facts, link} from '../ui/components.js';
import {modal} from '../ui/dialogs.js';
import {formatMetricValue} from './alert-utils.js';

export function showAlert(id, signal) {
  const controller = new AbortController();
  let disposed = false;
  const body = h('div', {class: 'dialog-body stack'}, h('p', {role: 'status'}, 'Loading alert…'));
  const dialog = modal('Alert details', () => body);
  const dispose = () => { disposed = true; controller.abort(); signal?.removeEventListener('abort', stop); };
  const stop = () => { dispose(); dialog.close('navigation'); };
  signal?.addEventListener('abort', stop, {once: true});
  dialog.closed.then(dispose);
  if (signal?.aborted) { stop(); return; }
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(id)) {
    body.replaceChildren(h('p', {role: 'alert'}, 'Alert not found. Check the event link.'));
    return;
  }
  api(`/alerts/${encodeURIComponent(id)}`, {signal: controller.signal}).then((event) => {
    if (disposed) return;
    body.replaceChildren(facts([
      ['Rule', event.rule_name || 'Deleted rule'], ['Node', event.node_name || event.node_id], ['State', event.state === 'firing' ? 'Firing' : 'Resolved'],
      ['Metric', event.metric || 'Unknown'], ['Severity', event.severity || 'Unknown'], ['Value', formatMetricValue(event.metric, event.value)],
      ['Started', when(event.started_at)], ['Resolved', event.resolved_at ? when(event.resolved_at) : null], ['Event ID', event.id]
    ]), link(`/nodes/${encodeURIComponent(event.node_id)}`, 'View node'));
  }).catch((error) => {
    if (disposed || error.name === 'AbortError') return;
    body.replaceChildren(h('p', {role: 'alert'}, error.status === 404 ? 'Alert not found. It may have expired.' : `Could not load alert: ${error.message}`));
  });
}
