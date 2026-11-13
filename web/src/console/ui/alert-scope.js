import {t} from '../i18n.js';
import {combobox, field, select} from './forms.js';

// 静默才传 serviceGroups：静默可以覆盖监测分组，范围“全部”也包含监测；告警规则只针对节点。
// serviceGroups 为 null 时（静默但监测分组读取失败）不提供监测分组选项。
export function alertScope(item, {groups, nodes, serviceGroups, silence = serviceGroups !== undefined}) {
  const kinds = silence
    ? [['all', t('alerts.silences.scope.all')], ['group', t('alerts.silences.scope.group')], ['node', t('alerts.scope.node')],
      ...(Array.isArray(serviceGroups) ? [['service_group', t('alerts.silences.scope.serviceGroup')]] : [])]
    : [['all', t('alerts.scope.all')], ['group', t('alerts.scope.group')], ['node', t('alerts.scope.node')]];
  const scope = select(kinds, item?.scope_kind || 'all', {name: 'scope_kind'});
  const scopeID = combobox([], '', {name: 'scope_id'});
  const scopeField = field(t('alerts.rules.groupOrNode'), scopeID);
  const label = scopeField.querySelector('label');
  const entries = (kind) => kind === 'group' ? groups : kind === 'service_group' ? serviceGroups || [] : nodes;
  const syncScope = () => {
    const choices = [['', t('alerts.choose')], ...entries(scope.value).map((entry) => [entry.id, entry.name])];
    scopeID.setChoices(choices);
    scopeID.value = scope.value === item?.scope_kind ? item.scope_id || '' : '';
    scopeID.setSearchable(scope.value === 'node');
    label.setAttribute('for', scopeID.visibleControl.id);
    // 静默有三类对象，标签跟随所选范围；规则沿用“分组或节点”。
    if (silence) label.textContent = kinds.find(([kind]) => kind === scope.value)?.[1] || t('alerts.rules.groupOrNode');
    scopeID.disabled = scope.value === 'all';
    scopeID.required = scope.value !== 'all';
    scopeField.hidden = scope.value === 'all';
  };
  scope.addEventListener('change', syncScope);
  syncScope();
  return {scope, scopeID, fields: [field(t('alerts.column.scope'), scope), scopeField]};
}
