import {t} from '../i18n.js';
import {api, getNavigationSignal, getSessionEpoch, groupsList, invalidate} from '../api.js';
import {go, ignoreNavigationCancellation} from '../router.js';
import {button} from '../ui/components.js';
import {formDialog} from '../ui/dialogs.js';
import {toast} from '../ui/feedback.js';
import {field, input, validateBytes} from '../ui/forms.js';
import {issueEnrollment} from '../pages/enrollments.js';
import {groupSelect} from './node-groups.js';

let opening = false;

// 分组读取失败时仍可添加节点，只是不提供分组选择。
// onGroupCreated：在下拉中新建分组后立即通知调用页，即使随后取消添加节点。
export async function createNodeDialog({group = '', onGroupCreated} = {}) {
  if (opening) return null;
  const navigation = getNavigationSignal();
  const epoch = getSessionEpoch();
  const active = () => !navigation?.aborted && navigation === getNavigationSignal() && epoch === getSessionEpoch();
  opening = true;
  let groups;
  try { groups = await groupsList(); } catch { groups = null; } finally { opening = false; }
  if (!active()) return null;
  let name;
  let groupID;
  let node;
  const dialog = formDialog({
    title: t('nodes.add'),
    submit: t('nodes.add'),
    build: (form) => {
      name = input({name: 'name', required: true, autocomplete: 'off', placeholder: t('nodes.nameExample')});
      validateBytes(name, 1, 255, t('nodes.name'));
      form.append(field(t('nodes.name'), name));
      if (groups) {
        groupID = groupSelect(groups, groups.some((item) => item.id === group) ? group : '', {name: 'group_id'}, {onCreated: onGroupCreated});
        form.append(field(t('nodes.group'), groupID));
      }
    },
    action: async () => {
      node = await api('/nodes', {method: 'POST', body: {name: name.value, ...(groupID?.value ? {group_id: groupID.value} : {})}});
      invalidate('nodes');
      if (active() && !dialog.signal.aborted) toast(t('nodes.addedDone', {name: node.name}));
    }
  });
  void dialog.closed.then(async (value) => {
    if (value !== 'done' || !node || !active()) return;
    let outcome;
    try { outcome = await issueEnrollment({node_id: node.id}, t('nodes.enrollment.title', {name: node.name})); }
    catch (error) {
      if (!active() || error.name === 'AbortError') return;
      toast(t('nodes.enrollment.issueFailedAfterCreate'), {error: true});
    }
    if (active() && outcome !== 'view') await go(`/nodes/${encodeURIComponent(node.id)}`);
  }).catch(ignoreNavigationCancellation);
  return dialog;
}

// group 为函数时在点击时读取，节点页据此默认选中正在查看的分组。
export const addNodeButton = ({group, onGroupCreated} = {}) => button(t('nodes.add'), {icon: 'plus', variant: 'primary',
  onClick: () => createNodeDialog({group: typeof group === 'function' ? group() : group || '', onGroupCreated})});
