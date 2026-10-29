import {h} from '../../shared/dom.js';
import {t} from '../../shared/i18n.js';
import {errorText} from '../../shared/errors.js';
import {api} from '../api.js';
import {when} from '../format.js';
import {facts, link} from '../ui/components.js';
import {modal} from '../ui/dialogs.js';
import {formatMetricValue, metricLabel, severityLabel} from './alert-utils.js';

export function showAlert(id, signal) {
  const controller = new AbortController();
  let disposed = false;
  const body = h('div', {class: 'dialog-body stack'}, h('p', {role: 'status'}, t('alerts.detail.loading')));
  const dialog = modal(t('alerts.detail.title'), () => body);
  const dispose = () => { disposed = true; controller.abort(); signal?.removeEventListener('abort', stop); };
  const stop = () => { dispose(); dialog.close('navigation'); };
  signal?.addEventListener('abort', stop, {once: true});
  dialog.closed.then(dispose);
  if (signal?.aborted) { stop(); return; }
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(id)) {
    body.replaceChildren(h('p', {role: 'alert'}, t('alerts.detail.invalidLink')));
    return;
  }
  api(`/alerts/${encodeURIComponent(id)}`, {signal: controller.signal}).then((event) => {
    if (disposed) return;
    body.replaceChildren(facts([
      [t('alerts.column.rule'), event.rule_name || t('alerts.deletedRule')], [t('alerts.column.node'), event.node_name || event.node_id], [t('alerts.column.state'), t(event.state === 'firing' ? 'alerts.state.firing' : 'alerts.state.resolved')],
      [t('alerts.column.metric'), metricLabel(event.metric)], [t('alerts.column.severity'), severityLabel(event.severity)], [t('alerts.column.value'), formatMetricValue(event.metric, event.value)],
      [t('alerts.column.started'), when(event.started_at)], [t('alerts.column.resolved'), event.resolved_at ? when(event.resolved_at) : null], [t('alerts.detail.eventID'), event.id]
    ]), link(`/nodes/${encodeURIComponent(event.node_id)}`, t('alerts.detail.viewNode')));
  }).catch((error) => {
    if (disposed || error.name === 'AbortError') return;
    body.replaceChildren(h('p', {role: 'alert'}, error.status === 404 ? t('alerts.detail.expired') : t('alerts.detail.loadFailed', {error: errorText(error)})));
  });
}
