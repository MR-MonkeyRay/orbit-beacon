import {h} from '../../shared/dom.js';
import {api, session} from '../api.js';
import {button, iconButton, panel} from '../ui/components.js';
import {confirmDialog, formDialog} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, select, validateBytes} from '../ui/forms.js';
import {durationSeconds, errorCodeMessage, formatMetricValue, formatRuleDuration, METRICS, metricLabel, metricMax, scopeLabel, SEVERITIES, validateRule, validationError} from './alert-utils.js';
import {severityPill} from './alert-events.js';

export function ruleEditor(item, {groups, nodes, channels, signal, saved}) {
  if (!session.user || !session.csrf || signal?.aborted) return;
  const controls = {};
  return formDialog({
    title: item ? `Edit ${item.name}` : 'Add alert rule', submit: 'Save rule',
    build: (form) => {
      const name = controls.name = input({name: 'name', required: true, value: item?.name || '', autocomplete: 'off'});
      validateBytes(name, 1, 100, 'Rule name');
      const enabled = controls.enabled = input({type: 'checkbox', name: 'enabled', checked: item?.enabled ?? true});
      const metric = controls.metric = select(METRICS, item?.metric || 'cpu_percent', {name: 'metric'});
      const comparator = controls.comparator = select([['>', 'Greater than (>)'], ['<', 'Less than (<)']], item?.comparator || '>', {name: 'comparator'});
      const threshold = controls.threshold = input({type: 'number', name: 'threshold', required: true, min: 0, step: 'any', value: item?.threshold ?? 80});
      const thresholdField = field('Threshold', threshold);
      const thresholdLabel = thresholdField.querySelector('label');
      const condition = h('div', {class: 'form-row'}, field('Comparator', comparator), thresholdField);
      const offlineHint = h('p', {class: 'hint'}, 'Offline uses time since the latest server-received report, or enrollment. It has no comparator or threshold and requires at least 180 seconds (3 minutes).');
      const minutes = controls.minutes = input({name: 'for_seconds', type: 'number', min: 0, max: 1440, step: 1, required: true, value: Math.floor((item?.for_seconds ?? 180) / 60)});
      const seconds = controls.seconds = input({name: 'duration_seconds', type: 'number', min: 0, max: 59, step: 1, required: true, value: (item?.for_seconds ?? 180) % 60});
      const syncMetric = () => {
        const offline = metric.value === 'offline';
        condition.hidden = offline;
        comparator.disabled = threshold.disabled = offline;
        threshold.required = !offline;
        offlineHint.hidden = !offline;
        threshold.max = metricMax(metric.value);
        // Defaults such as 80% are out of range for a count; 0 means any failure.
        if (Number(threshold.value) > threshold.max) threshold.value = metric.value === 'probes_down' ? '0' : String(threshold.max);
        thresholdLabel.textContent = metric.value === 'load1' ? 'Threshold (load)' : metric.value === 'probes_down' ? 'Threshold (failing probes)' : 'Threshold (%)';
        minutes.min = offline ? 3 : 0;
      };
      metric.addEventListener('change', syncMetric);
      syncMetric();
      const scope = controls.scope_kind = select([['all', 'All nodes'], ['group', 'Group'], ['node', 'Node']], item?.scope_kind || 'all', {name: 'scope_kind'});
      const scopeID = controls.scope_id = select([], '', {name: 'scope_id'});
      const scopeField = field('Group or node', scopeID);
      const syncScope = () => {
        const options = scope.value === 'group' ? groups : nodes;
        scopeID.replaceChildren(h('option', {value: ''}, 'Choose…'), ...options.map((entry) => h('option', {value: entry.id}, entry.name)));
        scopeID.value = scope.value === item?.scope_kind ? item.scope_id || '' : '';
        scopeID.disabled = scope.value === 'all';
        scopeID.required = scope.value !== 'all';
        scopeField.hidden = scope.value === 'all';
      };
      scope.addEventListener('change', syncScope);
      syncScope();
      const severity = controls.severity = select(SEVERITIES, item?.severity || 'warning', {name: 'severity'});
      const routing = controls.channel_ids = select(channels.map((entry) => [entry.id, `${entry.name}${entry.enabled ? '' : ' (disabled)'}`]), '', {name: 'channel_ids', multiple: true, size: Math.min(6, Math.max(2, channels.length)), class: 'input alert-multiselect'});
      for (const option of routing.options) option.selected = Boolean(item?.channel_ids.includes(option.value));
      form.append(field('Name', name), field('Enabled', enabled), field('Metric', metric), condition, offlineHint,
        h('div', {class: 'form-row'}, field('Duration (minutes)', minutes, {hint: 'Maximum total duration: 24 hours.'}), field('Additional seconds', seconds)),
        field('Scope', scope), scopeField, field('Severity', severity),
        field('Notification channels', routing, {hint: channels.length ? 'Select any channels to notify. Use Ctrl or Command to select multiple; disabled channels do not deliver.' : 'No channels configured. Rules can fire without sending notifications.'}));
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
      try {
        await api(item ? `/alert-rules/${encodeURIComponent(item.id)}` : '/alert-rules', {method: item ? 'PATCH' : 'POST', body, signal});
      } catch (error) {
        if (error.code === 'alert_rule_capacity_reached') error.message = errorCodeMessage(error.code);
        throw error;
      }
      await saved();
      toast('Alert rule saved.');
    }
  });
}

export function rulesPanel({rules, groups, nodes, channels, signal, saved}) {
  const edit = (rule) => ruleEditor(rule, {groups, nodes, channels, signal, saved});
  const remove = async (rule) => {
    if (!session.user || !session.csrf || signal?.aborted) return;
    if (!(await confirmDialog({title: `Delete ${rule.name}?`, message: 'Event history is kept. Firing episodes resolve on the next evaluation.', confirm: 'Delete rule', danger: true}))) return;
    if (signal?.aborted) return;
    try {
      await api(`/alert-rules/${encodeURIComponent(rule.id)}`, {method: 'DELETE', signal});
      await saved();
      toast('Alert rule deleted.');
    } catch (error) { if (!signal?.aborted) toastError(error); }
  };
  return panel('Rules', {action: button('Add rule', {small: true, variant: 'primary', onClick: () => edit(null)})},
    h('p', {class: 'lede'}, 'Rules are evaluated every 30 seconds. Metric conditions must hold for the full duration.'),
    rules.length ? h('div', {class: 'table-wrap'}, h('table', {class: 'table'},
      h('caption', {class: 'sr'}, 'Alert rules'),
      h('thead', null, h('tr', null, ['Name', 'Condition', 'Scope', 'Severity', 'State', 'Channels', 'Actions'].map((label) => h('th', {scope: 'col'}, label)))),
      h('tbody', null, rules.map((rule) => h('tr', null,
        h('td', null, rule.name),
        h('td', null, metricLabel(rule.metric), rule.metric === 'offline' ? '' : ` ${rule.comparator} ${formatMetricValue(rule.metric, rule.threshold)}`, ` for ${formatRuleDuration(rule.for_seconds)}`),
        h('td', null, scopeLabel(rule, groups, nodes)), h('td', null, severityPill(rule.severity)), h('td', null, rule.enabled ? 'Enabled' : 'Disabled'),
        h('td', null, rule.channel_ids.map((id) => channels.find((channel) => channel.id === id)?.name || id).join(', ') || 'None'),
        h('td', {class: 'actions'}, iconButton('edit', `Edit ${rule.name}`, () => edit(rule)), iconButton('trash', `Delete ${rule.name}`, () => remove(rule))))))))
      : h('p', {class: 'muted'}, 'No alert rules yet.'));
}
