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
  let target = '';
  const dialog = formDialog({
    title: t('nodes.add'),
    submit: t('nodes.add'),
    build: (form) => {
      name = input({name: 'name', required: true, autocomplete: 'off', placeholder: t('nodes.nameExample')});
      validateBytes(name, 1, 255, t('nodes.name'));
      form.append(field(t('nodes.name'), name));
    },
    action: async () => {
      const node = await api('/nodes', {method: 'POST', body: {name: name.value}});
      invalidate('nodes');
      toast(t('nodes.addedDone', {name: node.name}));
      target = `/nodes/${encodeURIComponent(node.id)}#enrollment`;
    }
  });
  // 导航等对话框关闭后再发起：打开中的草稿表单会让 go() 按未保存更改取消。
  void dialog.closed.then((value) => { if (value === 'done' && target) go(target).catch(ignoreNavigationCancellation); });
  return dialog;
}

export const addNodeButton = () => button(t('nodes.add'), {icon: 'plus', variant: 'primary', onClick: createNodeDialog});
