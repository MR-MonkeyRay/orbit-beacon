import {t} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {api, invalidate} from '../api.js';
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
