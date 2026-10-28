import {h} from '../../shared/dom.js';
import {api, invalidate} from '../api.js';
import {configStateLabel, formatBytes} from '../format.js';
import {refresh} from '../router.js';
import {callout, facts, panel} from '../ui/components.js';
import {toast} from '../ui/feedback.js';
import {field, input, onSubmit, rangeField, saveBar, select, trackDirty} from '../ui/forms.js';
import {configTagEditor} from '../ui/config-tags.js';
import {changedConfig, quotaToBytes, validateProbeTargets, validateSelection} from './node-config-helpers.js';

function selectionEditor(values, kind) {
  return configTagEditor(values || [], {
    label: kind === 'interfaces' ? 'Network interfaces' : 'Disk paths',
    fieldName: kind,
    hint: kind === 'interfaces' ? 'Press Enter or comma to add. Empty uses the default route.' : 'Press Enter or comma to add. Empty uses the agent default.',
    maxBytes: kind === 'interfaces' ? 255 : 1024,
    validate: (value) => validateSelection([value], kind)
  });
}

function probeTargetEditor(values) {
  return configTagEditor(values || [], {
    label: 'Service probes',
    fieldName: 'probe_targets',
    hint: 'Up to 5 targets: tcp://host:port, https://url, icmp://host. Checked every 60 seconds by the agent.',
    maxItems: 5,
    maxBytes: 1024,
    trim: false,
    separators: /\n/,
    commitOnComma: false,
    validate: (value) => validateProbeTargets([value])
  });
}

function editableForm(form, controls, save) {
  trackDirty(form, controls);
  controls.discard.addEventListener('click', refresh);
  onSubmit(form, controls.save, async () => {
    const message = await save();
    form.markClean();
    await refresh();
    toast(message);
  });
}

function valuesFor(config) {
  return facts([
    ['Report interval', `${config.report_interval_seconds} seconds`],
    ['Disk interval', `${config.disk_interval_seconds} seconds`],
    ['Billing day', `Day ${config.billing_day} of each month`],
    ['Traffic quota', h('span', {title: `${config.quota_bytes} bytes`}, formatBytes(config.quota_bytes))],
    ['Network interfaces', config.interfaces.length ? config.interfaces.join('\n') : 'Default route'],
    ['Disk paths', config.disk_paths.length ? config.disk_paths.join('\n') : 'Agent default'],
    ['Service probes', config.probe_targets?.length ? config.probe_targets.join('\n') : 'None configured']
  ]);
}

export function configTab(node, config) {
  const values = valuesFor(config);
  const raw = h('details', {class: 'json-toggle'}, h('summary', null, 'Raw configuration'), h('pre', {class: 'code'}, JSON.stringify(config, null, 2)));
  const revision = h('span', {class: 'revision'}, `Revision ${config.revision}`);
  const application = node.config;
  const applicationFacts = facts([
    ['Application state', configStateLabel(application)],
    ['Desired revision', application?.desired_revision],
    ['Applied revision', application?.applied_revision],
    ['Error fields', application?.error?.fields && Object.keys(application.error.fields).length ? JSON.stringify(application.error.fields) : null]
  ]);
  const lede = h('p', {class: 'lede'}, 'The agent receives saved values with its next report. Application state updates when the agent reports again.');

  const interfaces = selectionEditor(config.interfaces, 'interfaces');
  const diskPaths = selectionEditor(config.disk_paths, 'disk_paths');
  const probeTargets = probeTargetEditor(config.probe_targets);
  const interfaceBox = interfaces.querySelector('[data-tags]');
  const diskBox = diskPaths.querySelector('[data-tags]');
  const probeBox = probeTargets.querySelector('[data-tags]');
  const quota = input({type: 'number', name: 'quota_value', min: 0, step: 1, value: config.quota_bytes, inputMode: 'numeric', autocomplete: 'off', id: 'field-quota'});
  const quotaUnit = select([
    ['bytes', 'Bytes'], ['KB', 'KB (1,000 bytes)'], ['MB', 'MB (1,000,000 bytes)'], ['GB', 'GB (1,000,000,000 bytes)'], ['TB', 'TB (1,000,000,000,000 bytes)']
  ], 'bytes', {name: 'quota_unit', id: 'field-quota-unit', 'aria-label': 'Quota unit'});
  const checkQuota = () => {
    const result = quotaToBytes(quota.value, quotaUnit.value);
    quota.setCustomValidity(result.ok ? '' : result.error);
  };
  quota.addEventListener('input', checkQuota);
  quotaUnit.addEventListener('change', checkQuota);
  checkQuota();
  const billing = input({type: 'number', name: 'billing', min: 1, max: 28, required: true, value: config.billing_day, id: 'field-billing', inputMode: 'numeric'});
  const controls = saveBar('Save configuration', 'config-save');
  const form = h('form', {class: 'form form-wide'},
    rangeField('Report interval (seconds)', 'report', config.report_interval_seconds, {min: 5, max: 60, step: 1, hint: 'How often the agent reports metrics, 5–60 seconds.'}),
    rangeField('Disk interval (seconds)', 'disk', config.disk_interval_seconds, {min: 60, max: 3600, step: 60, hint: 'How often disk usage is collected, 60–3600 seconds.'}),
    interfaces,
    diskPaths,
    probeTargets,
    field('Traffic quota', quota, {inner: h('div', {class: 'form-row'}, quota, h('div', null, quotaUnit)), hint: 'Decimal units (1 KB = 1,000 bytes). Enter 0 for unlimited.'}),
    field('Billing day', billing, {hint: 'Day of the month a traffic cycle starts, 1–28.'}),
    callout('Quota units use powers of 1,000. A quota of 0 means unlimited.', 'info'),
    controls.bar);
  editableForm(form, controls, async () => {
    const interfaceCommitted = interfaceBox.commit();
    const diskCommitted = diskBox.commit();
    const probeCommitted = probeBox.commit();
    const interfaceValues = interfaceBox.getValue();
    const diskValues = diskBox.getValue();
    const probeValues = probeBox.getValue();
    const interfaceError = validateSelection(interfaceValues, 'interfaces');
    const diskError = validateSelection(diskValues, 'disk_paths');
    const probeError = validateProbeTargets(probeValues);
    if (!interfaceCommitted || !diskCommitted || !probeCommitted || interfaceError || diskError || probeError) {
      if (interfaceError) interfaceBox.showError(interfaceError);
      if (diskError) diskBox.showError(diskError);
      if (probeError) probeBox.showError(probeError);
      throw new Error(interfaceError || diskError || probeError || 'Fix the highlighted selection before saving.');
    }
    const quotaResult = quotaToBytes(quota.value, quotaUnit.value);
    if (!quotaResult.ok) { quota.setCustomValidity(quotaResult.error); throw new Error(quotaResult.error); }
    const patch = changedConfig(config, {
      report_interval_seconds: form.elements.namedItem('report').value,
      disk_interval_seconds: form.elements.namedItem('disk').value,
      billing_day: billing.value,
      interfaces: interfaceValues,
      disk_paths: diskValues,
      probe_targets: probeValues,
      quota_bytes: quotaResult.value
    });
    if (!Object.keys(patch).length) return 'No configuration changes.';
    const result = await api(`/nodes/${encodeURIComponent(node.id)}/config`, {method: 'PATCH', body: patch});
    invalidate('nodes');
    return `Configuration saved as revision ${result.revision}.`;
  });
  return h('div', {class: 'split'},
    panel('Edit configuration', {action: revision}, h('div', {class: 'stack'}, applicationFacts, lede, form)),
    panel('Current values', {}, values, raw));
}

