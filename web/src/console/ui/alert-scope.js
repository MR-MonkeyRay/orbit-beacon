import {t} from '../i18n.js';
import {combobox, field, select} from './forms.js';

export function alertScope(item, {groups, nodes}) {
  const scope = select([['all', t('alerts.scope.all')], ['group', t('alerts.scope.group')], ['node', t('alerts.scope.node')]], item?.scope_kind || 'all', {name: 'scope_kind'});
  const scopeID = combobox([], '', {name: 'scope_id'});
  const scopeField = field(t('alerts.rules.groupOrNode'), scopeID);
  const syncScope = () => {
    const choices = [['', t('alerts.choose')], ...(scope.value === 'group' ? groups : nodes).map((entry) => [entry.id, entry.name])];
    scopeID.setChoices(choices);
    scopeID.value = scope.value === item?.scope_kind ? item.scope_id || '' : '';
    scopeID.setSearchable(scope.value === 'node');
    scopeField.querySelector('label').setAttribute('for', scopeID.visibleControl.id);
    scopeID.disabled = scope.value === 'all';
    scopeID.required = scope.value !== 'all';
    scopeField.hidden = scope.value === 'all';
  };
  scope.addEventListener('change', syncScope);
  syncScope();
  return {scope, scopeID, fields: [field(t('alerts.column.scope'), scope), scopeField]};
}
