import {h} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {field, select} from './forms.js';

export function alertScope(item, {groups, nodes}) {
  const scope = select([['all', t('alerts.scope.all')], ['group', t('alerts.scope.group')], ['node', t('alerts.scope.node')]], item?.scope_kind || 'all', {name: 'scope_kind'});
  const scopeID = select([], '', {name: 'scope_id'});
  const scopeField = field(t('alerts.rules.groupOrNode'), scopeID);
  const syncScope = () => {
    const options = scope.value === 'group' ? groups : nodes;
    scopeID.replaceChildren(h('option', {value: ''}, t('alerts.choose')), ...options.map((entry) => h('option', {value: entry.id}, entry.name)));
    scopeID.value = scope.value === item?.scope_kind ? item.scope_id || '' : '';
    scopeID.disabled = scope.value === 'all';
    scopeID.required = scope.value !== 'all';
    scopeField.hidden = scope.value === 'all';
  };
  scope.addEventListener('change', syncScope);
  syncScope();
  return {scope, scopeID, fields: [field(t('alerts.column.scope'), scope), scopeField]};
}
