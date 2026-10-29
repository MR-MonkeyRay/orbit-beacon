import {h} from '../../shared/dom.js';
import {absoluteWhen} from '../format.js';
import {panel, readonlyFacts} from '../ui/components.js';

export function agentSettingsTab(node, runtime) {
  const guidance = h('p', {class: 'lede'}, 'Read-only. Edit the agent’s local configuration and reload it to change these settings.');
  const offline = h('p', {class: 'muted', hidden: true});
  const body = h('div');
  const values = readonlyFacts();
  const empty = h('p', {class: 'muted'}, 'No agent settings reported yet.');
  const element = panel('Agent settings', {}, guidance, offline, body);
  element.snapshot = (current, observed) => {
    offline.hidden = !observed || current.presence !== 'offline';
    if (!offline.hidden) offline.replaceChildren(...(current.last_seen
      ? ['Agent offline. Showing the last report received ', absoluteWhen(current.last_seen), '.'] : ['Agent offline.']));
    if (!observed) {
      if (!body.contains(empty)) body.replaceChildren(empty);
      return;
    }
    values.update([
      ['Report interval', `${observed.report_interval_seconds} seconds`],
      ['Disk interval', `${observed.disk_interval_seconds} seconds`],
      ['Network interfaces', observed.interfaces.length ? observed.interfaces.join('\n') : 'Default route'],
      ['Disk paths', observed.disk_paths.length ? observed.disk_paths.join('\n') : 'Agent default'],
      ['Service probes', observed.probe_targets.length ? observed.probe_targets.join('\n') : 'None configured'],
      ['Agent version', current.agent_version],
      ['Last reported', current.last_seen ? absoluteWhen(current.last_seen) : 'Never']
    ]);
    if (!body.contains(values.element)) body.replaceChildren(values.element);
  };
  element.snapshot(node, runtime);
  return element;
}
