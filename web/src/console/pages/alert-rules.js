import {h} from '../../shared/dom.js';
import {formatList, t} from '../i18n.js';
import {api, session} from '../api.js';
import {button, dataTable, iconButton, panel} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, select, validateBytes} from '../ui/forms.js';
import {durationSeconds, formatMetricValue, formatRuleDuration, isProbeMetric, METRICS, metricLabel, metricMax, scopeLabel, SEVERITIES, validateRule, validationError} from '../lib/alert-utils.js';
import {alertScope} from '../ui/alert-scope.js';
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
      const threshold = controls.threshold = input({type: 'number', name: 'threshold', required: true, min: 0, step: 'any', value: String(item?.threshold ?? 80)});
      const thresholdField = field(t('alerts.rules.threshold'), threshold);
      const thresholdLabel = thresholdField.querySelector('label');
      const comparatorField = field(t('alerts.rules.comparator'), comparator);
      const condition = h('div', {class: 'form-row'}, comparatorField, thresholdField);
      const target = controls.target = input({name: 'target', value: item?.target || '', autocomplete: 'off', class: 'input mono'});
      const targetField = field(t('nodes.audit.target'), target);
      const minutes = controls.minutes = input({name: 'for_seconds', type: 'number', min: 0, max: 1440, step: 1, required: true, value: Math.floor((item?.for_seconds ?? 180) / 60)});
      const seconds = controls.seconds = input({name: 'duration_seconds', type: 'number', min: 0, max: 59, step: 1, required: true, value: (item?.for_seconds ?? 180) % 60});
      let previousMetric = metric.value;
      let previousComparator = item?.comparator || '>';
      const validateThreshold = () => {
        const value = Number(threshold.value);
        threshold.setCustomValidity(metric.value === 'node_expiry_days' && (!threshold.value.trim() || !Number.isFinite(value) || value < 1 || value > 365) ? t('alerts.validation.expiryThreshold') : '');
      };
      threshold.addEventListener('input', validateThreshold);
      const syncMetric = () => {
        const expiry = ['node_expiry_days', 'probe_tls_expiry_days'].includes(metric.value);
        const failed = metric.value === 'probe_failed';
        const probe = isProbeMetric(metric.value);
        targetField.hidden = !probe;
        target.disabled = !probe;
        target.required = probe;
        if (expiry) {
          if (!['node_expiry_days', 'probe_tls_expiry_days'].includes(previousMetric)) {
            previousComparator = comparator.value;
            threshold.value = '30';
          }
          comparator.value = '<';
        } else if (failed) {
          if (previousMetric !== 'probe_failed' && !['node_expiry_days', 'probe_tls_expiry_days'].includes(previousMetric)) previousComparator = comparator.value;
          comparator.value = '>';
        } else if (['node_expiry_days', 'probe_tls_expiry_days', 'probe_failed'].includes(previousMetric)) comparator.value = previousComparator;
        previousMetric = metric.value;
        comparatorField.hidden = expiry || failed;
        const offline = metric.value === 'offline';
        condition.hidden = offline || failed;
        comparator.disabled = offline || expiry || failed;
        threshold.disabled = offline || failed;
        threshold.required = !offline && !failed;
        threshold.max = metricMax(metric.value);
        threshold.min = metric.value === 'node_expiry_days' ? 1 : 0;
        // 80% 之类的默认值对计数无意义；0 表示任意一次失败。
        if (!failed && Number(threshold.value) > threshold.max) threshold.value = metric.value === 'probes_down' ? '0' : String(threshold.max);
        validateThreshold();
        thresholdLabel.textContent = t(expiry ? 'alerts.rules.thresholdDays' : metric.value === 'probe_latency_ms' ? 'alerts.rules.thresholdMilliseconds' : metric.value === 'load1' ? 'alerts.rules.thresholdLoad' : metric.value === 'probes_down' ? 'alerts.rules.thresholdProbes' : 'alerts.rules.thresholdPercent');
        minutes.min = offline ? 3 : 0;
      };
      metric.addEventListener('change', syncMetric);
      syncMetric();
      const {scope, scopeID, fields: scopeFields} = alertScope(item, {groups, nodes});
      controls.scope_kind = scope;
      controls.scope_id = scopeID;
      const severity = controls.severity = select(SEVERITIES, item?.severity || 'warning', {name: 'severity'});
      const routing = controls.channel_ids = select(channels.map((entry) => [entry.id, entry.enabled ? entry.name : t('alerts.disabledName', {name: entry.name})]), '', {name: 'channel_ids', multiple: true, size: Math.min(6, Math.max(2, channels.length)), class: 'input alert-multiselect'});
      for (const option of routing.options) option.selected = Boolean(item?.channel_ids.includes(option.value));
      form.append(field(t('alerts.column.name'), name), field(t('alerts.state.enabled'), enabled), field(t('alerts.column.metric'), metric), targetField, condition,
        h('div', {class: 'form-row'}, field(t('alerts.rules.durationMinutes'), minutes), field(t('alerts.rules.additionalSeconds'), seconds)),
        ...scopeFields, field(t('alerts.column.severity'), severity),
        field(t('alerts.channels.title'), routing, {hint: t(channels.length ? 'alerts.rules.routingHint' : 'alerts.rules.noChannelsHint')}));
    },
    action: async () => {
      if (!session.user || !session.csrf || signal?.aborted) throw new DOMException('Cancelled', 'AbortError');
      const c = controls;
      const offline = c.metric.value === 'offline';
      const body = {name: c.name.value, enabled: c.enabled.checked, metric: c.metric.value,
        target: isProbeMetric(c.metric.value) ? c.target.value.trim() : null,
        comparator: offline ? null : c.metric.value === 'probe_failed' ? '>' : ['node_expiry_days', 'probe_tls_expiry_days'].includes(c.metric.value) ? '<' : c.comparator.value, threshold: offline ? null : c.metric.value === 'probe_failed' ? 0 : c.threshold.value.trim() === '' ? NaN : Number(c.threshold.value),
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
    rules.length ? dataTable({caption: t('alerts.rules.caption'),
      columns: ['name', 'condition', 'scope', 'severity', 'state', 'channels', 'actions'].map((key) => t(`alerts.column.${key}`)),
      body: h('tbody', null, rules.map((rule) => h('tr', null,
        h('td', null, rule.name),
        h('td', null, t('alerts.rules.conditionDuration', {condition: ['offline', 'probe_failed'].includes(rule.metric) ? metricLabel(rule.metric) : t('alerts.rules.metricCondition', {metric: metricLabel(rule.metric), comparator: rule.comparator, value: formatMetricValue(rule.metric, rule.threshold)}), duration: formatRuleDuration(rule.for_seconds)}), rule.target ? h('div', null, h('code', {class: 'probe-target mono'}, rule.target)) : null),
        h('td', null, scopeLabel(rule, groups, nodes)), h('td', null, severityPill(rule.severity)), h('td', null, t(rule.enabled ? 'alerts.state.enabled' : 'alerts.state.disabled')),
        h('td', null, formatList(rule.channel_ids.map((id) => channels.find((channel) => channel.id === id)?.name || id)) || t('common.none')),
        h('td', {class: 'actions'}, iconButton('edit', t('alerts.editNamed', {name: rule.name}), () => edit(rule)), iconButton('trash', t('alerts.deleteAction', {name: rule.name}), () => remove(rule))))))})
      : h('p', {class: 'text-note'}, t('alerts.rules.empty')));
}
