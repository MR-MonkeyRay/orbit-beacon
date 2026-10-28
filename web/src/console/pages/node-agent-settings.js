import {h} from '../../shared/dom.js';
import {when} from '../format.js';
import {facts, panel} from '../ui/components.js';

export function agentSettingsTab(node, runtime) {
  const guidance = h('p', {class: 'lede'}, 'Read-only. Edit the agent’s local configuration and reload it to change these settings.');
  if (!runtime) return panel('Agent settings', {}, guidance,
    h('p', {class: 'muted'}, 'No agent settings reported yet.'));
  const offline = node.presence === 'offline' ? h('p', {class: 'muted'}, node.last_seen
    ? ['Agent offline. Showing the last report received ', when(node.last_seen), '.'] : 'Agent offline.') : null;
  return panel('Agent settings', {}, guidance, offline, facts([
    ['Report interval', `${runtime.report_interval_seconds} seconds`],
    ['Disk interval', `${runtime.disk_interval_seconds} seconds`],
    ['Network interfaces', runtime.interfaces.length ? runtime.interfaces.join('\n') : 'Default route'],
    ['Disk paths', runtime.disk_paths.length ? runtime.disk_paths.join('\n') : 'Agent default'],
    ['Service probes', runtime.probe_targets.length ? runtime.probe_targets.join('\n') : 'None configured'],
    ['Agent version', node.agent_version],
    ['Last reported', node.last_seen ? when(node.last_seen) : 'Never']
  ]));
}
