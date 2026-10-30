import {t, tParts} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {absoluteWhen} from '../format.js';
import {panel, readonlyFacts} from '../ui/components.js';

export function agentSettingsTab(node, runtime) {
  const guidance = h('p', {class: 'lede'}, t('nodes.agentReadOnly'));
  const offline = h('p', {class: 'muted', hidden: true});
  const body = h('div');
  const values = readonlyFacts();
  const empty = h('p', {class: 'muted'}, t('nodes.agentEmpty'));
  const element = panel(t('nodes.agentSettings'), {}, guidance, offline, body);
  element.snapshot = (current, observed) => {
    offline.hidden = !observed || current.presence !== 'offline';
    if (!offline.hidden) offline.replaceChildren(...(current.last_seen
      ? tParts('nodes.agentOfflinePrefix', {time: absoluteWhen(current.last_seen)}) : [t('nodes.agentOffline')]));
    if (!observed) {
      if (!body.contains(empty)) body.replaceChildren(empty);
      return;
    }
    values.update([
      [t('nodes.reportInterval'), t('nodes.seconds', {count: observed.report_interval_seconds})],
      [t('nodes.diskInterval'), t('nodes.seconds', {count: observed.disk_interval_seconds})],
      [t('nodes.interfaces'), observed.interfaces.length ? observed.interfaces.join('\n') : t('nodes.defaultRoute')],
      [t('nodes.diskPaths'), observed.disk_paths.length ? observed.disk_paths.join('\n') : t('nodes.agentDefault')],
      [t('nodes.probes'), observed.probe_targets.length ? observed.probe_targets.join('\n') : t('nodes.noneConfigured')],
      [t('nodes.agentVersion'), current.agent_version],
      [t('nodes.lastReported'), current.last_seen ? absoluteWhen(current.last_seen) : t('nodes.never')]
    ]);
    if (!body.contains(values.element)) body.replaceChildren(values.element);
  };
  element.snapshot(node, runtime);
  return element;
}
