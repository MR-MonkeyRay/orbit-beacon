import {h, icon} from '../../shared/dom.js';
import {api, ensureSession, groupsList, invalidate, isAdmin} from '../api.js';
import {formatBytes, shortID, when} from '../format.js';
import {refresh} from '../router.js';
import {callout, facts, iconButton, link, note, panel, statusBadge} from '../ui/components.js';
import {toast} from '../ui/feedback.js';
import {field, input, onSubmit, rangeField, saveBar, select, tagEditor, textarea, trackDirty, validateBytes} from '../ui/forms.js';
import {tabSet} from '../ui/tabs.js';

export const NODE_PATH = /^\/nodes\/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/i;

export async function nodePage() {
  await ensureSession();
  const id = location.pathname.match(NODE_PATH)[1];
  const path = `/nodes/${encodeURIComponent(id)}`;
  const [node, config, groups, publicSettings] = await Promise.all([api(path), api(`${path}/config`), groupsList(), api(`${path}/public-settings`)]);
  const editable = isAdmin();
  const copy = iconButton('copy', 'Copy node ID', async () => {
    try {
      await navigator.clipboard.writeText(node.id);
      copy.replaceChildren(icon('check'));
      toast('Node ID copied.');
      setTimeout(() => copy.replaceChildren(icon('copy')), 1600);
    } catch { toast('The browser blocked copying. Select the ID instead.', {error: true}); }
  });
  const back = link('/nodes', icon('left'), 'All nodes');
  back.className = 'back';
  return h('div', null,
    h('header', {class: 'page-head'}, h('div', null, back,
      h('div', {class: 'detail-head'}, h('h1', {tabindex: '-1'}, node.name), statusBadge(node.status)),
      h('div', {class: 'id-chip'}, h('span', {class: 'mono'}, node.id), copy))),
    tabSet([
      {id: 'details', label: 'Details', icon: 'server', build: () => detailsTab(node, groups, editable)},
      {id: 'public', label: 'Public page', icon: 'globe', build: () => publicTab(node, publicSettings, editable)},
      {id: 'config', label: 'Agent configuration', icon: 'sliders', build: () => configTab(node, config, editable)}
    ], 'Node sections'));
}

// Wires the shared save/discard behaviour of the editable sections.
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

function detailsTab(node, groups, editable) {
  const group = groups.find((item) => item.id === node.group_id);
  const summary = panel('Summary', {}, facts([
    ['Status', statusBadge(node.status)],
    ['Group', group ? group.name : node.group_id ? `Unknown (${shortID(node.group_id)})` : 'Ungrouped'],
    ['Region', node.region], ['Provider', node.provider],
    ['Tags', node.tags.length ? h('div', {class: 'cell-tags'}, node.tags.map((tag) => h('span', {class: 'tag static'}, tag))) : null],
    ['Added', when(node.created_at)],
    ['Notes', node.notes]
  ]), node.status === 'unknown' ? note('Unknown is the recorded inventory state. It does not mean the agent is offline.') : null);
  if (!editable) return h('div', null, summary, note('Only administrators can edit node details.'));

  const name = input({name: 'name', required: true, value: node.name, id: 'node-name'});
  validateBytes(name, 1, 255, 'Node name');
  const region = input({name: 'region', value: node.region, id: 'node-region'});
  const provider = input({name: 'provider', value: node.provider, id: 'node-provider'});
  const choices = [['', 'Ungrouped'], ...groups.map((item) => [item.id, item.name])];
  if (node.group_id && !group) choices.push([node.group_id, `Unknown (${shortID(node.group_id)})`]);
  const groupID = select(choices, node.group_id || '', {name: 'group_id', id: 'node-group'});
  const tags = tagEditor(node.tags);
  const tagBox = tags.querySelector('[data-tags]');
  const notes = textarea({name: 'notes', id: 'node-notes'}, node.notes);
  const controls = saveBar('Save changes', 'node-save');
  const form = h('form', {class: 'form form-wide'},
    field('Node name', name, {hint: 'Internal only. The public page uses its own name.'}),
    h('div', {class: 'form-row'}, field('Region', region), field('Provider', provider)),
    field('Group', groupID), tags, field('Notes', notes), controls.bar);
  editableForm(form, controls, async () => {
    if (!tagBox.commit()) throw new Error('Fix the highlighted tag before saving.');
    const result = await api(`/nodes/${encodeURIComponent(node.id)}`, {method: 'PATCH', body: {
      name: name.value, notes: notes.value, region: region.value, provider: provider.value, group_id: groupID.value || null, tags: tagBox.getValue()
    }});
    invalidate('nodes');
    return `Saved ${result.name}.`;
  });
  return h('div', {class: 'split'}, summary, panel('Edit details', {}, form));
}

function publicPreview() {
  const name = h('h3');
  const where = h('div', {class: 'where'});
  const badge = h('span', {class: 'badge'});
  const element = h('div', {class: 'preview'}, h('p', {class: 'preview-label'}, 'How the card appears on the public page'),
    h('div', {class: 'preview-card'}, h('div', {class: 'top'}, name, badge), where));
  element.paint = ({enabled, name: title, region, provider}) => {
    name.replaceChildren(title || h('span', {class: 'placeholder'}, 'Public name'));
    where.replaceChildren(...[region && h('span', null, icon('pin'), region), provider && h('span', null, icon('cloud'), provider)].filter(Boolean));
    if (!region && !provider) where.append(h('span', {class: 'placeholder'}, 'No region or provider'));
    badge.className = `badge ${enabled ? 'online' : 'disabled'}`;
    badge.textContent = enabled ? 'Published' : 'Hidden';
  };
  return element;
}

function publicTab(node, settings, editable) {
  const hidden = node.status === 'pending' || node.status === 'disabled'
    ? callout(`This node is ${node.status}. It is not shown on the public page until it is enrolled and enabled.`) : null;
  const privacy = callout('Publishing shows the public name, region and provider, plus online status, CPU, memory, load and uptime. Internal details are never copied. Do not enter IP addresses, hostnames, domains or credentials.', 'info');
  const preview = publicPreview();
  const previewPanel = panel('Preview', {action: h('a', {class: 'btn btn-sm', href: '/'}, icon('external'), 'View public page')}, preview);

  if (!editable) {
    preview.paint(settings);
    return h('div', {class: 'split'}, panel('Public page', {}, h('div', {class: 'stack'},
      facts([['Published', settings.enabled ? 'Yes' : 'No'], ['Public name', settings.name], ['Public region', settings.region], ['Public provider', settings.provider]]), hidden, privacy)), previewPanel);
  }

  const enabled = h('input', {type: 'checkbox', name: 'enabled', id: 'public-enabled', checked: settings.enabled});
  const name = input({name: 'public_name', value: settings.name, id: 'public-name', autocomplete: 'off', dataset: {field: 'name'}});
  const region = input({name: 'public_region', value: settings.region, id: 'public-region', autocomplete: 'off', dataset: {field: 'region'}});
  const provider = input({name: 'public_provider', value: settings.provider, id: 'public-provider', autocomplete: 'off', dataset: {field: 'provider'}});
  const checkName = validateBytes(name, 0, 255, 'Public name');
  validateBytes(region, 0, 128, 'Public region');
  validateBytes(provider, 0, 128, 'Public provider');
  // The server trims text, so the preview and the required check do too.
  const sync = () => {
    name.required = enabled.checked;
    if (enabled.checked && name.value.trim() === '') name.setCustomValidity('A public name is required while the node is published.');
    else checkName();
    preview.paint({enabled: enabled.checked, name: name.value.trim(), region: region.value.trim(), provider: provider.value.trim()});
  };
  for (const control of [enabled, name, region, provider]) control.addEventListener('input', sync);
  enabled.addEventListener('change', sync);
  sync();
  const controls = saveBar('Save public settings', 'public-save');
  const form = h('form', {class: 'form form-wide'},
    h('label', {class: 'switch'}, enabled, h('span', {class: 'switch-track', 'aria-hidden': 'true'}),
      h('span', {class: 'switch-text'}, h('strong', null, 'Publish this node'), h('span', null, 'Show this node on the public status page.'))),
    field('Public name', name, {hint: 'Shown as the card title. Up to 255 UTF-8 bytes.'}),
    h('div', {class: 'form-row'}, field('Public region', region, {hint: 'Up to 128 UTF-8 bytes.'}), field('Public provider', provider, {hint: 'Up to 128 UTF-8 bytes.'})),
    controls.bar);
  editableForm(form, controls, async () => {
    await api(`/nodes/${encodeURIComponent(node.id)}/public-settings`, {method: 'PATCH', body: {enabled: enabled.checked, name: name.value, region: region.value, provider: provider.value}});
    return enabled.checked ? 'Public settings saved. Online status depends on valid agent reports.' : 'Public settings saved. The node is hidden from the public page.';
  });
  return h('div', {class: 'split'}, panel('Public page', {}, h('div', {class: 'stack'}, hidden, privacy, form)), previewPanel);
}

function configTab(node, config, editable) {
  const values = facts([
    ['Report interval', `${config.report_interval_seconds} seconds`],
    ['Disk interval', `${config.disk_interval_seconds} seconds`],
    ['Billing day', `Day ${config.billing_day} of each month`],
    ['Traffic quota', h('span', {title: `${config.quota_bytes} bytes`}, formatBytes(config.quota_bytes))],
    ['Network interfaces', config.interfaces.length ? config.interfaces.join('\n') : 'Default route'],
    ['Disk paths', config.disk_paths.length ? config.disk_paths.join('\n') : 'Agent default']
  ]);
  const raw = h('details', {class: 'json-toggle'}, h('summary', null, 'Raw configuration'), h('pre', {class: 'code'}, JSON.stringify(config, null, 2)));
  const revision = h('span', {class: 'revision'}, `Revision ${config.revision}`);
  const lede = h('p', {class: 'lede'}, 'The agent receives saved values with its next report. Saving records the desired state; the console does not confirm that the agent applied it.');
  if (!editable) return panel('Desired configuration', {action: revision}, h('div', {class: 'stack'}, lede, values, raw));

  const controls = saveBar('Save configuration', 'config-save');
  const billing = input({type: 'number', name: 'billing', min: 1, max: 28, required: true, value: config.billing_day, id: 'field-billing', inputMode: 'numeric'});
  const form = h('form', {class: 'form'},
    rangeField('Report interval (seconds)', 'report', config.report_interval_seconds, {min: 5, max: 60, step: 1, hint: 'How often the agent reports metrics, 5–60 seconds.'}),
    rangeField('Disk interval (seconds)', 'disk', config.disk_interval_seconds, {min: 60, max: 3600, step: 60, hint: 'How often disk usage is collected, 60–3600 seconds.'}),
    field('Billing day', billing, {hint: 'Day of the month a traffic cycle starts, 1–28.'}),
    controls.bar);
  editableForm(form, controls, async () => {
    const result = await api(`/nodes/${encodeURIComponent(node.id)}/config`, {method: 'PATCH', body: {
      report_interval_seconds: Number(form.elements.namedItem('report').value),
      disk_interval_seconds: Number(form.elements.namedItem('disk').value),
      billing_day: Number(billing.value)
    }});
    return `Configuration saved as revision ${result.revision}.`;
  });
  return h('div', {class: 'split'},
    panel('Edit configuration', {action: revision}, h('div', {class: 'stack'}, lede, form)),
    panel('Current values', {}, values, raw));
}
