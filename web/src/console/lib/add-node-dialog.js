import {t} from '../i18n.js';
import {api, getNavigationSignal, getSessionEpoch, invalidate} from '../api.js';
import {go, ignoreNavigationCancellation} from '../router.js';
import {button} from '../ui/components.js';
import {formDialog} from '../ui/dialogs.js';
import {toast} from '../ui/feedback.js';
import {field, input, validateBytes} from '../ui/forms.js';
import {issueEnrollment} from '../pages/enrollments.js';

export function createNodeDialog() {
  let name;
  let node;
  const navigation = getNavigationSignal();
  const epoch = getSessionEpoch();
  const active = () => !navigation?.aborted && navigation === getNavigationSignal() && epoch === getSessionEpoch();
  const dialog = formDialog({
    title: t('nodes.add'),
    submit: t('nodes.add'),
    build: (form) => {
      name = input({name: 'name', required: true, autocomplete: 'off', placeholder: t('nodes.nameExample')});
      validateBytes(name, 1, 255, t('nodes.name'));
      form.append(field(t('nodes.name'), name));
    },
    action: async () => {
      node = await api('/nodes', {method: 'POST', body: {name: name.value}});
      invalidate('nodes');
      if (active() && !dialog.signal.aborted) toast(t('nodes.addedDone', {name: node.name}));
    }
  });
  void dialog.closed.then(async (value) => {
    if (value !== 'done' || !node || !active()) return;
    try { await issueEnrollment({node_id: node.id}, t('nodes.enrollment.title', {name: node.name})); }
    catch (error) {
      if (!active() || error.name === 'AbortError') return;
      toast(t('nodes.enrollment.issueFailedAfterCreate'), {error: true});
    }
    if (active()) await go(`/nodes/${encodeURIComponent(node.id)}#enrollment`);
  }).catch(ignoreNavigationCancellation);
  return dialog;
}

export const addNodeButton = () => button(t('nodes.add'), {icon: 'plus', variant: 'primary', onClick: createNodeDialog});
