import {t} from '../../shared/i18n.js';
import {h, icon} from '../../shared/dom.js';
import {api, invalidate} from '../api.js';
import {describeAction, shortID, when} from '../format.js';
import {go, ignoreNavigationCancellation} from '../router.js';
import {button} from '../ui/components.js';
import {formDialog} from '../ui/dialogs.js';
import {toast} from '../ui/feedback.js';
import {field, input, validateBytes} from '../ui/forms.js';

export function createNodeDialog() {
  let name;
  formDialog({
    title: t('nodes.add'),
    intro: t('nodes.addIntro'),
    submit: t('nodes.add'),
    build: (form) => {
      name = input({name: 'name', required: true, autocomplete: 'off', placeholder: t('nodes.nameExample')});
      validateBytes(name, 1, 255, t('nodes.name'));
      form.append(field(t('nodes.name'), name, {hint: t('nodes.nameHint')}));
    },
    action: async () => {
      const node = await api('/nodes', {method: 'POST', body: {name: name.value}});
      invalidate('nodes');
      toast(t('nodes.addedDone', {name: node.name}));
      void go(`/nodes/${encodeURIComponent(node.id)}`).catch(ignoreNavigationCancellation);
    }
  });
}

export const addNodeButton = () => button(t('nodes.add'), {icon: 'plus', variant: 'primary', onClick: createNodeDialog});

// One audit event in a timeline; `names` maps user, node and group IDs to names.
export function auditItem(event, names, withDetails) {
  const verb = event.action.split('.').pop();
  const kind = /create|claim|enable/.test(verb) ? 'create' : /delete|revoke|disable/.test(verb) ? 'delete' : /^user\./.test(event.action) ? 'auth' : '';
  const agentActor = !event.actor_id && event.details?.actor_kind === 'agent';
  const actor = event.actor_id ? names.get(event.actor_id) || shortID(event.actor_id) : agentActor ? 'Agent' : t('nodes.audit.system');
  const target = event.target_id ? names.get(event.target_id) || shortID(event.target_id) : null;
  const details = withDetails ? h('details', {class: 'ev-details'}, h('summary', null, icon('right'), t('nodes.audit.details')),
    h('dl', null,
      [[t('nodes.audit.action'), event.action], [t('nodes.audit.event'), event.id], [t('nodes.audit.actor'), event.actor_id || (agentActor ? 'Agent' : t('nodes.none'))], [t('nodes.audit.target'), event.target_id || t('nodes.none')]]
        .map(([label, value]) => [h('dt', null, label), h('dd', {class: 'mono'}, value)]),
      Object.entries(event.details || {}).map(([key, value]) => [h('dt', null, key), h('dd', null, typeof value === 'string' ? value : JSON.stringify(value))]))) : null;
  return h('li', null, h('span', {class: `dot ${kind}`}),
    h('div', null,
      h('div', {class: 'ev-title'}, describeAction(event.action)),
      h('div', {class: 'ev-meta'}, h('b', null, actor), target ? [t('nodes.audit.on'), h('b', null, target)] : null, ', ', when(event.created_at)),
      details));
}
