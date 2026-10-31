import {h} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {errorText} from '../../shared/errors.js';
import {api} from '../api.js';
import {when} from '../format.js';
import {facts, link} from '../ui/components.js';
import {formatMetricValue, metricLabel, severityLabel} from '../lib/alert-utils.js';
import {detailDialog} from '../ui/detail-dialog.js';
import {isDetailUUID} from '../../shared/uuid.js';

export function showAlert(id, signal) {
  detailDialog({
    title: t('alerts.detail.title'), loading: t('alerts.detail.loading'), signal, abortBeforeValidate: true,
    valid: isDetailUUID(id), invalid: t('alerts.detail.invalidLink'),
    load: (requestSignal) => api(`/alerts/${encodeURIComponent(id)}`, {signal: requestSignal}),
    render: (event) => [facts([
      [t('alerts.column.rule'), event.rule_name || t('alerts.deletedRule')], [t('alerts.column.node'), event.node_name || event.node_id], [t('alerts.column.state'), t(event.state === 'firing' ? 'alerts.state.firing' : 'alerts.state.resolved')],
      [t('alerts.column.metric'), metricLabel(event.metric)], [t('alerts.column.severity'), severityLabel(event.severity)], [t('alerts.column.value'), formatMetricValue(event.metric, event.value)],
      [t('alerts.column.started'), when(event.started_at)], [t('alerts.column.resolved'), event.resolved_at ? when(event.resolved_at) : null], [t('alerts.detail.eventID'), event.id]
    ]), link(`/nodes/${encodeURIComponent(event.node_id)}`, t('alerts.detail.viewNode'))],
    failure: (error) => h('p', {role: 'alert'}, error.status === 404 ? t('alerts.detail.expired') : t('alerts.detail.loadFailed', {error: errorText(error)}))
  });
}
