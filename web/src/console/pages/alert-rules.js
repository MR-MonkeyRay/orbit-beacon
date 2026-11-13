import {h} from '../../shared/dom.js';
import {formatList, t} from '../i18n.js';
import {api, session} from '../api.js';
import {button, dataTable, iconButton, panel} from '../ui/components.js';
import {confirmDialog, confirmDiscard, formDialog, hasUnsavedDialogChanges} from '../ui/dialogs.js';
import {toast, toastError} from '../ui/feedback.js';
import {field, input, select, validateBytes} from '../ui/forms.js';
import {durationParts, durationSeconds, formatMetricValue, formatRuleDuration, isProbeMetric, METRICS, metricLabel, metricMax, scopeLabel, severityLabel, SEVERITIES, validateRule, validationError} from '../lib/alert-utils.js';
import {alertScope} from '../ui/alert-scope.js';
import {severityPill} from './alert-events.js';

export function ruleEditor(item, {groups, nodes, channels, signal, saved, openChannels}) {
  if (!session.user || !session.csrf || signal?.aborted) return;
  const controls = {};
  const dialog = formDialog({
    title: item ? t('alerts.editNamed', {name: item.name}) : t('alerts.rules.addTitle'), submit: t('alerts.rules.save'),
    build: (form) => {
      form.noValidate = true;
      const name = controls.name = input({name: 'name', required: true, value: item?.name || '', autocomplete: 'off'});
      validateBytes(name, 1, 100, t('alerts.rules.name'));
      const enabled = controls.enabled = input({type: 'checkbox', name: 'enabled', checked: item?.enabled ?? true});
      const metric = controls.metric = select(METRICS, item?.metric || 'cpu_percent', {name: 'metric'});
      const comparator = controls.comparator = select([['>', t('alerts.rules.greaterThan')], ['<', t('alerts.rules.lessThan')]], item?.comparator || '>', {name: 'comparator'});
      const threshold = controls.threshold = input({type: 'number', name: 'threshold', required: true, min: 0, step: 'any', value: String(item?.threshold ?? 80)});
      const thresholdField = field(t('alerts.rules.threshold'), threshold);
      const thresholdLabel = thresholdField.querySelector('label');
      const comparatorField = field(t('alerts.rules.comparator'), comparator);
      // 比较方式与阈值在同一网格里，与持续时长、单位共用一行；离线类指标整组隐藏。
      const condition = h('div', {class: 'rule-condition-pair'}, comparatorField, thresholdField);
      const target = controls.target = input({name: 'target', value: item?.target || '', autocomplete: 'off', class: 'input mono'});
      const targetField = field(t('nodes.audit.target'), target);
      const initialDuration = durationParts(item?.for_seconds ?? 180);
      const duration = controls.duration = input({name: 'for_seconds', type: 'number', step: 'any', required: true, value: initialDuration.value});
      const unit = controls.unit = select([[1, t('alerts.rules.unit.seconds')], [60, t('alerts.rules.unit.minutes')], [3600, t('alerts.rules.unit.hours')]], String(initialDuration.unit), {name: 'duration_unit'});
      const syncDuration = () => {
        duration.min = (metric.value === 'offline' ? 180 : 0) / Number(unit.value);
        duration.max = 86400 / Number(unit.value);
      };
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
        syncDuration();
      };
      metric.addEventListener('change', syncMetric);
      syncMetric();
      const {scope, scopeID, fields: scopeFields} = alertScope(item, {groups, nodes});
      controls.scope_kind = scope;
      controls.scope_id = scopeID;
      const severity = controls.severity = select(SEVERITIES, item?.severity || 'warning', {name: 'severity'});
      // 通知通道是可多选的勾选片，没有通道时以添加入口替代。
      const routing = controls.channel_ids = channels.map((entry) => h('input', {type: 'checkbox', name: 'channel_ids', value: entry.id,
        checked: Boolean(item?.channel_ids.includes(entry.id))}));
      const preview = h('p', {class: 'alert-rule-preview', role: 'status'});
      const syncPreview = () => {
        const seconds = durationSeconds(duration.value, unit.value);
        if (!Number.isFinite(seconds) || threshold.value.trim() === '' && !threshold.disabled) { preview.textContent = ''; return; }
        const part = durationParts(seconds);
        const condition = ['offline', 'probe_failed'].includes(metric.value) ? metricLabel(metric.value) : t('alerts.rules.metricCondition', {
          metric: metricLabel(metric.value), comparator: comparator.value, value: formatMetricValue(metric.value, Number(threshold.value))});
        preview.textContent = t('alerts.rules.preview', {condition,
          duration: t(`alerts.rules.previewDuration.${part.unit}`, {count: part.value}),
          severity: severityLabel(severity.value), scope: scopeLabel({scope_kind: scope.value, scope_id: scopeID.value}, groups, nodes)});
      };
      for (const control of [metric, comparator, threshold, duration, unit, scope, scopeID, severity]) {
        control.addEventListener('input', syncPreview);
        control.addEventListener('change', syncPreview);
      }
      unit.addEventListener('change', syncDuration);
      const presets = !item ? h('div', {class: 'rule-presets'}, [
        ['offline', 'alerts.rules.preset.offline', 180, 'critical'],
        ['cpu_percent', 'alerts.rules.preset.cpu', 300, 'warning'],
        ['memory_used_percent', 'alerts.rules.preset.memory', 300, 'warning'],
        ['disk_used_percent', 'alerts.rules.preset.disk', 600, 'critical']
      ].map(([value, label, seconds, level]) => button(t(label), {small: true, onClick: () => {
        metric.value = value;
        syncMetric();
        comparator.value = '>';
        threshold.value = '90';
        const part = durationParts(seconds);
        duration.value = part.value;
        unit.value = String(part.unit);
        severity.value = level;
        if (!name.value.trim()) name.value = t(label);
        name.dispatchEvent(new Event('input', {bubbles: true}));
        syncDuration(); syncPreview();
      }}))) : null;
      const addChannel = button(t('alerts.channels.add'), {small: true, onClick: async () => {
        if (hasUnsavedDialogChanges() && !(await confirmDiscard())) return;
        dialog.close('navigate');
        await dialog.closed;
        if (!signal?.aborted) openChannels?.();
      }});
      form.classList.add('rule-form');
      // 原生 append 会把 null 写成文本，编辑已有规则时没有预设行。
      form.append(h('div', {class: 'rule-name-row'}, field(t('alerts.column.name'), name), field(t('alerts.state.enabled'), enabled)), ...(presets ? [presets] : []),
        field(t('alerts.column.metric'), metric), targetField,
        h('div', {class: 'rule-condition'}, condition, field(t('alerts.column.duration'), duration), field(t('alerts.rules.durationUnit'), unit)),
        h('div', {class: 'rule-scope'}, ...scopeFields, field(t('alerts.column.severity'), severity)),
        h('div', {class: 'field', role: 'group', 'aria-labelledby': 'rule-channels-label'}, h('span', {class: 'field-label', id: 'rule-channels-label'}, t('alerts.channels.title')),
          channels.length ? h('div', {class: 'channel-chips'}, channels.map((entry, index) => h('label', {class: 'check-row'}, routing[index],
            h('span', null, entry.enabled ? entry.name : t('alerts.disabledName', {name: entry.name}))))) : h('div', null, addChannel)),
        preview);
      syncPreview();
    },
    action: async () => {
      if (!session.user || !session.csrf || signal?.aborted) throw new DOMException('Cancelled', 'AbortError');
      const c = controls;
      const offline = c.metric.value === 'offline';
      const body = {name: c.name.value, enabled: c.enabled.checked, metric: c.metric.value,
        target: isProbeMetric(c.metric.value) ? c.target.value.trim() : null,
        comparator: offline ? null : c.metric.value === 'probe_failed' ? '>' : ['node_expiry_days', 'probe_tls_expiry_days'].includes(c.metric.value) ? '<' : c.comparator.value, threshold: offline ? null : c.metric.value === 'probe_failed' ? 0 : c.threshold.value.trim() === '' ? NaN : Number(c.threshold.value),
        for_seconds: durationSeconds(c.duration.value, c.unit.value), scope_kind: c.scope_kind.value,
        scope_id: c.scope_kind.value === 'all' ? null : c.scope_id.value, severity: c.severity.value,
        channel_ids: c.channel_ids.filter((check) => check.checked).map((check) => check.value)};
      const errors = validateRule(body);
      if (Object.keys(errors).length) throw validationError(errors);
      await api(item ? `/alert-rules/${encodeURIComponent(item.id)}` : '/alert-rules', {method: item ? 'PATCH' : 'POST', body, signal});
      await saved();
      toast(t('alerts.rules.saved'));
    }
  });
  return dialog;
}

export function rulesPanel({rules, groups, nodes, channels, signal, saved, openChannels}) {
  const edit = (rule) => ruleEditor(rule, {groups, nodes, channels, signal, saved, openChannels});
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
  return panel(null, {action: h('div', {class: 'alert-toolbar'}, !channels.length ? button(t('alerts.channels.add'), {small: true, onClick: openChannels}) : null, button(t('alerts.rules.add'), {small: true, variant: 'primary', onClick: () => edit(null)}))},
    rules.length ? dataTable({caption: t('alerts.rules.caption'),
      columns: ['name', 'condition', 'scope', 'severity', 'channels', 'actions'].map((key) => t(`alerts.column.${key}`)),
      // 启用状态并入名称：只有停用的规则带一个弱化标记。
      body: h('tbody', null, rules.map((rule) => h('tr', {class: rule.enabled ? null : 'rule-disabled'},
        h('td', null, rule.name, rule.enabled ? null : [' ', h('span', {class: 'badge disabled'}, t('alerts.state.disabled'))]),
        h('td', null, t('alerts.rules.conditionDuration', {condition: ['offline', 'probe_failed'].includes(rule.metric) ? metricLabel(rule.metric) : t('alerts.rules.metricCondition', {metric: metricLabel(rule.metric), comparator: rule.comparator, value: formatMetricValue(rule.metric, rule.threshold)}), duration: formatRuleDuration(rule.for_seconds)}), rule.target ? h('div', null, h('code', {class: 'probe-target mono'}, rule.target)) : null),
        h('td', null, scopeLabel(rule, groups, nodes)), h('td', null, severityPill(rule.severity)),
        h('td', null, formatList(rule.channel_ids.map((id) => channels.find((channel) => channel.id === id)?.name || id)) || t('common.none')),
        h('td', {class: 'actions'}, iconButton('edit', t('alerts.editNamed', {name: rule.name}), () => edit(rule)), iconButton('trash', t('alerts.deleteAction', {name: rule.name}), () => remove(rule))))))})
      : h('p', {class: 'text-note'}, t('alerts.rules.empty')));
}
