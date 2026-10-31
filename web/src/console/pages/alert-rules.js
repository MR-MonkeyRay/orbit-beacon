import {h} from '../../shared/dom.js';
import {formatList, t} from '../i18n.js';
import {api, session} from '../api.js';
import {button, dataTable, iconButton, panel} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, select, validateBytes} from '../ui/forms.js';
import {durationSeconds, formatMetricValue, formatRuleDuration, METRICS, metricLabel, metricMax, scopeLabel, SEVERITIES, validateRule, validationError} from '../lib/alert-utils.js';
import {severityPill} from './alert-events.js';

export function ruleEditor(item, {groups, nodes, channels, signal, saved}) {
  if (!session.user || !session.csrf || signal?.aborted) return;
  const controls = {};
  return formDialog({
    title: item ? t('alerts.editNamed', {name: item.name}) : t('alerts.rules.addTitle'), submit: t('alerts.rules.save'),
    build: (form) => {
      const name = controls.name = input({name: 'name', required: true, value: item?.name || '', autocomplete: 'off'});
      validateBytes(name, 1, 100, t('alerts.rules.name'));
      const enabled = controls.enabled = input({type: 'checkbox', name: 'enabled', checked: item?.enabled ?? true});
      const metric = controls.metric = select(METRICS, item?.metric || 'cpu_percent', {name: 'metric'});
      const comparator = controls.comparator = select([['>', t('alerts.rules.greaterThan')], ['<', t('alerts.rules.lessThan')]], item?.comparator || '>', {name: 'comparator'});
      const threshold = controls.threshold = input({type: 'number', name: 'threshold', required: true, min: 0, step: 'any', value: item?.threshold ?? 80});
      const thresholdField = field(t('alerts.rules.threshold'), threshold);
      const thresholdLabel = thresholdField.querySelector('label');
      const condition = h('div', {class: 'form-row'}, field(t('alerts.rules.comparator'), comparator), thresholdField);
      const offlineHint = h('p', {class: 'hint'}, t('alerts.rules.offlineHint'));
      const minutes = controls.minutes = input({name: 'for_seconds', type: 'number', min: 0, max: 1440, step: 1, required: true, value: Math.floor((item?.for_seconds ?? 180) / 60)});
      const seconds = controls.seconds = input({name: 'duration_seconds', type: 'number', min: 0, max: 59, step: 1, required: true, value: (item?.for_seconds ?? 180) % 60});
      const syncMetric = () => {
        const offline = metric.value === 'offline';
        condition.hidden = offline;
        comparator.disabled = threshold.disabled = offline;
        threshold.required = !offline;
        offlineHint.hidden = !offline;
        threshold.max = metricMax(metric.value);
        // 80% 之类的默认值对计数无意义；0 表示任意一次失败。
        if (Number(threshold.value) > threshold.max) threshold.value = metric.value === 'probes_down' ? '0' : String(threshold.max);
        thresholdLabel.textContent = t(metric.value === 'load1' ? 'alerts.rules.thresholdLoad' : metric.value === 'probes_down' ? 'alerts.rules.thresholdProbes' : 'alerts.rules.thresholdPercent');
        minutes.min = offline ? 3 : 0;
      };
      metric.addEventListener('change', syncMetric);
      syncMetric();
      const scope = controls.scope_kind = select([['all', t('alerts.scope.all')], ['group', t('alerts.scope.group')], ['node', t('alerts.scope.node')]], item?.scope_kind || 'all', {name: 'scope_kind'});
      const scopeID = controls.scope_id = select([], '', {name: 'scope_id'});
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
      const severity = controls.severity = select(SEVERITIES, item?.severity || 'warning', {name: 'severity'});
      const routing = controls.channel_ids = select(channels.map((entry) => [entry.id, entry.enabled ? entry.name : t('alerts.disabledName', {name: entry.name})]), '', {name: 'channel_ids', multiple: true, size: Math.min(6, Math.max(2, channels.length)), class: 'input alert-multiselect'});
      for (const option of routing.options) option.selected = Boolean(item?.channel_ids.includes(option.value));
      form.append(field(t('alerts.column.name'), name), field(t('alerts.state.enabled'), enabled), field(t('alerts.column.metric'), metric), condition, offlineHint,
        h('div', {class: 'form-row'}, field(t('alerts.rules.durationMinutes'), minutes, {hint: t('alerts.rules.durationHint')}), field(t('alerts.rules.additionalSeconds'), seconds)),
        field(t('alerts.column.scope'), scope), scopeField, field(t('alerts.column.severity'), severity),
        field(t('alerts.channels.title'), routing, {hint: t(channels.length ? 'alerts.rules.routingHint' : 'alerts.rules.noChannelsHint')}));
    },
    action: async () => {
      if (!session.user || !session.csrf || signal?.aborted) throw new DOMException('Cancelled', 'AbortError');
      const c = controls;
      const offline = c.metric.value === 'offline';
      const body = {name: c.name.value, enabled: c.enabled.checked, metric: c.metric.value,
        comparator: offline ? null : c.comparator.value, threshold: offline ? null : c.threshold.value.trim() === '' ? NaN : Number(c.threshold.value),
        for_seconds: durationSeconds(c.minutes.value, c.seconds.value), scope_kind: c.scope_kind.value,
        scope_id: c.scope_kind.value === 'all' ? null : c.scope_id.value, severity: c.severity.value,
        channel_ids: [...c.channel_ids.selectedOptions].map((option) => option.value)};
      const errors = validateRule(body);
      if (Object.keys(errors).length) throw validationError(errors);
      await api(item ? `/alert-rules/${encodeURIComponent(item.id)}` : '/alert-rules', {method: item ? 'PATCH' : 'POST', body, signal});
      await saved();
      toast(t('alerts.rules.saved'));
    }
  });
}

export function rulesPanel({rules, groups, nodes, channels, signal, saved}) {
  const edit = (rule) => ruleEditor(rule, {groups, nodes, channels, signal, saved});
  const remove = async (rule) => {
    if (!session.user || !session.csrf || signal?.aborted) return;
    if (!(await confirmDialog({title: t('alerts.deleteNamed', {name: rule.name}), message: t('alerts.rules.deleteHint'), confirm: t('alerts.rules.delete'), danger: true}))) return;
    if (signal?.aborted) return;
    try {
      await api(`/alert-rules/${encodeURIComponent(rule.id)}`, {method: 'DELETE', signal});
      await saved();
      toast(t('alerts.rules.deleted'));
    } catch (error) { if (!signal?.aborted) toastError(error); }
  };
  return panel(t('alerts.tab.rules'), {action: button(t('alerts.rules.add'), {small: true, variant: 'primary', onClick: () => edit(null)})},
    h('p', {class: 'lede'}, t('alerts.rules.evaluationHint')),
    rules.length ? dataTable({caption: t('alerts.rules.caption'),
      columns: ['name', 'condition', 'scope', 'severity', 'state', 'channels', 'actions'].map((key) => t(`alerts.column.${key}`)),
      body: h('tbody', null, rules.map((rule) => h('tr', null,
        h('td', null, rule.name),
        h('td', null, t('alerts.rules.conditionDuration', {condition: rule.metric === 'offline' ? metricLabel(rule.metric) : t('alerts.rules.metricCondition', {metric: metricLabel(rule.metric), comparator: rule.comparator, value: formatMetricValue(rule.metric, rule.threshold)}), duration: formatRuleDuration(rule.for_seconds)})),
        h('td', null, scopeLabel(rule, groups, nodes)), h('td', null, severityPill(rule.severity)), h('td', null, t(rule.enabled ? 'alerts.state.enabled' : 'alerts.state.disabled')),
        h('td', null, formatList(rule.channel_ids.map((id) => channels.find((channel) => channel.id === id)?.name || id)) || t('common.none')),
        h('td', {class: 'actions'}, iconButton('edit', t('alerts.editNamed', {name: rule.name}), () => edit(rule)), iconButton('trash', t('alerts.deleteAction', {name: rule.name}), () => remove(rule))))))})
      : h('p', {class: 'muted'}, t('alerts.rules.empty')));
}
