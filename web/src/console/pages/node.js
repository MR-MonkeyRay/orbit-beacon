import {h, icon} from '../../shared/dom.js';
import {api, ensureSession, getNavigationSignal, groupsList, invalidate} from '../api.js';
import {formatDuration, formatPercent, shortID, when} from '../format.js';
import {refresh} from '../router.js';
import {callout, facts, iconButton, link, note, panel, statusBadge} from '../ui/components.js';
import {toast} from '../ui/feedback.js';
import {field, input, onSubmit, saveBar, select, tagEditor, textarea, trackDirty, validateBytes} from '../ui/forms.js';
import {tabSet} from '../ui/tabs.js';
import {agentSettingsTab} from './node-agent-settings.js';
import {enrollmentTab} from './enrollments.js';
import {diskSummary, loadSummary, memorySummary, networkSummary} from './latest-sample-helpers.js';
import {lifecycleActions} from './node-lifecycle.js';
import {historyPanel} from './node-history.js';
import {trafficPanel} from './node-traffic.js';
import {trafficSettingsPanel} from './node-traffic-settings.js';
import {probesPanel} from './node-probes.js';

export const NODE_PATH = /^\/nodes\/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/i;

export async function nodePage() {
  const signal = getNavigationSignal();
  await ensureSession();
  const id = location.pathname.match(NODE_PATH)[1];
  const path = `/nodes/${encodeURIComponent(id)}`;
  const [node, runtime, trafficSettings, groups, publicSettings, latest] = await Promise.all([
    api(path), api(`${path}/runtime`), api(`${path}/traffic-settings`), groupsList(), api(`${path}/public-settings`),
    api(`${path}/latest`).catch((error) => {
      if (error.status === 404) return null;
      throw error;
    })
  ]);
  const idText = h('span', {class: 'mono'}, node.id);
  const showCopied = () => {
    copy.replaceChildren(icon('check'));
    setTimeout(() => copy.replaceChildren(icon('copy')), 1600);
  };
  const copy = iconButton('copy', 'Copy node ID', async () => {
    try {
      if (!navigator.clipboard?.writeText) throw new Error('Clipboard API unavailable');
      await navigator.clipboard.writeText(node.id);
      showCopied();
      toast('Node ID copied.');
    } catch {
      const range = document.createRange();
      range.selectNodeContents(idText);
      const selection = getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      let copied = false;
      try { copied = document.execCommand('copy'); } catch {}
      if (copied) {
        showCopied();
        toast('Node ID copied using the browser fallback.');
      } else toast('The node ID is selected. Press Ctrl+C or Command+C to copy it.', {error: true});
    }
  });
  const back = link('/nodes', icon('left'), 'All nodes');
  back.className = 'back';
  let history;
  let traffic;
  let billingSettings = trafficSettings;
  let probes;
  return h('div', null,
    h('header', {class: 'page-head'}, h('div', null, back,
      h('div', {class: 'detail-head'}, h('h1', {tabindex: '-1'}, node.name), statusBadge(node.presence)),
      h('div', {class: 'id-chip'}, idText, copy)), lifecycleActions(node)),
    tabSet([
      {id: 'details', label: 'Details', icon: 'server', build: () => detailsTab(node, groups, latest,
        history ||= historyPanel(id, signal), traffic ||= trafficPanel(id, signal),
        probes ||= probesPanel(id, signal),
        trafficSettingsPanel(id, billingSettings, (settings) => {
          billingSettings = settings;
          return traffic.reload();
        }, signal))},
      {id: 'public', label: 'Public page', icon: 'globe', build: () => publicTab(node, publicSettings)},
      {id: 'agent', label: 'Agent settings', icon: 'sliders', build: () => agentSettingsTab(node, runtime.runtime)},
      {id: 'enrollment', label: 'Enroll agent', icon: 'server', build: () => enrollmentTab(node)}
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

function latestPanel(node, latest) {
  if (!latest) return panel('Latest sample', {}, h('p', {class: 'muted'}, 'No current sample. The agent has not reported since the server started, or its credential was revoked or the node disabled.'));
  const metrics = latest.metrics || {};
  return panel('Latest sample', {action: h('span', {class: 'revision'}, `Sequence ${latest.seq}`)}, facts([
    ['CPU', formatPercent(metrics.cpu_percent)],
    ['Memory', memorySummary(metrics)],
    ['Load (1 / 5 / 15 min)', loadSummary(metrics)],
    ['Disk', diskSummary(metrics.disks)],
    ['Network', networkSummary(metrics.networks)],
    ['Uptime', formatDuration(metrics.uptime_seconds)],
    ['Agent version', latest.system?.agent_version || node.agent_version],
    ['Sampled', latest.sampled_at ? when(latest.sampled_at) : null],
    ['Received', latest.received_at ? when(latest.received_at) : null]
  ]));
}

function detailsTab(node, groups, latest, history, traffic, probes, billing) {
  const group = groups.find((item) => item.id === node.group_id);
  const summary = panel('Summary', {}, facts([
    ['Presence', statusBadge(node.presence)],
    ['Last seen', node.last_seen ? when(node.last_seen) : 'Never'],
    ['Group', group ? group.name : node.group_id ? `Unknown (${shortID(node.group_id)})` : 'Ungrouped'],
    ['Region', node.region], ['Provider', node.provider],
    ['Tags', node.tags.length ? h('div', {class: 'cell-tags'}, node.tags.map((tag) => h('span', {class: 'tag static'}, tag))) : null],
    ['Added', when(node.created_at)],
    ['Notes', node.notes]
  ]), note('Presence may lag an accepted report by about 30 seconds.'));
  const sample = latestPanel(node, latest);

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
  return h('div', {class: 'stack'}, h('div', {class: 'split'}, summary, panel('Edit details', {}, form)), sample, history, traffic, billing, probes);
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

function publicTab(node, settings) {
  const hidden = node.status === 'pending' || node.status === 'disabled'
    ? callout(`This node is ${node.status}. It is not shown on the public page until it is enrolled and enabled.`) : null;
  const privacy = callout('Publishing shows the public name, region and provider, plus online status, CPU, memory, load and uptime. Internal details are never copied. Do not enter IP addresses, hostnames, domains or credentials.', 'info');
  const preview = publicPreview();
  const previewPanel = panel('Preview', {action: h('a', {class: 'btn btn-sm', href: '/'}, icon('external'), 'View public page')}, preview);


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
