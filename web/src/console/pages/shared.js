import {h, icon} from '../../shared/dom.js';
import {api, invalidate} from '../api.js';
import {describeAction, shortID, when} from '../format.js';
import {go} from '../router.js';
import {button} from '../ui/components.js';
import {formDialog} from '../ui/dialogs.js';
import {toast} from '../ui/feedback.js';
import {field, input, validateBytes} from '../ui/forms.js';

export function createNodeDialog() {
  let name;
  formDialog({
    title: 'Add node',
    intro: 'The node stays pending until an agent enrolls with a credential issued through the management API.',
    submit: 'Add node',
    build: (form) => {
      name = input({name: 'name', required: true, autocomplete: 'off', placeholder: 'e.g. tokyo-edge-01'});
      validateBytes(name, 1, 255, 'Node name');
      form.append(field('Node name', name, {hint: 'Internal name, 1–255 UTF-8 bytes. Not shown publicly.'}));
    },
    action: async () => {
      const node = await api('/nodes', {method: 'POST', body: {name: name.value}});
      invalidate('nodes');
      toast(`Added ${node.name}. It stays pending until an agent enrolls.`);
      go(`/nodes/${encodeURIComponent(node.id)}`);
    }
  });
}

export const addNodeButton = () => button('Add node', {icon: 'plus', variant: 'primary', onClick: createNodeDialog});

// One audit event in a timeline; `names` maps user, node and group IDs to names.
export function auditItem(event, names, withDetails) {
  const verb = event.action.split('.').pop();
  const kind = /create|claim|enable/.test(verb) ? 'create' : /delete|revoke|disable/.test(verb) ? 'delete' : /^user\./.test(event.action) ? 'auth' : '';
  const agentActor = !event.actor_id && event.details?.actor_kind === 'agent';
  const actor = event.actor_id ? names.get(event.actor_id) || shortID(event.actor_id) : agentActor ? 'Agent' : 'System';
  const target = event.target_id ? names.get(event.target_id) || shortID(event.target_id) : null;
  const details = withDetails ? h('details', {class: 'ev-details'}, h('summary', null, icon('right'), 'Details'),
    h('dl', null,
      [['Action', event.action], ['Event', event.id], ['Actor', event.actor_id || (agentActor ? 'Agent' : 'None')], ['Target', event.target_id || 'None']]
        .map(([label, value]) => [h('dt', null, label), h('dd', {class: 'mono'}, value)]),
      Object.entries(event.details || {}).map(([key, value]) => [h('dt', null, key), h('dd', null, typeof value === 'string' ? value : JSON.stringify(value))]))) : null;
  return h('li', null, h('span', {class: `dot ${kind}`}),
    h('div', null,
      h('div', {class: 'ev-title'}, describeAction(event.action)),
      h('div', {class: 'ev-meta'}, h('b', null, actor), target ? [' on ', h('b', null, target)] : null, ', ', when(event.created_at)),
      details));
}
