import {h, icon} from '../../shared/dom.js';
import {api, ensureSession, getNavigationSignal, groupsList, invalidate, onSession, session} from '../api.js';
import {absoluteWhen, shortID, when} from '../format.js';
import {connectConsoleLive} from '../live.js';
import {ignoreNavigationCancellation, refresh} from '../router.js';
import {callout, iconButton, link, liveStatus, panel, patchStatusBadge, readonlyFacts, statusBadge} from '../ui/components.js';
import {toast} from '../ui/feedback.js';
import {field, input, onSubmit, saveBar, select, tagEditor, textarea, trackDirty, validateBytes} from '../ui/forms.js';
import {tabSet} from '../ui/tabs.js';
import {agentSettingsTab} from './node-agent-settings.js';
import {enrollmentTab} from './enrollments.js';
import {latestSamplePanel} from './latest-sample.js';
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
  let observedRuntime = runtime.runtime;
  let detailsView;
  let agent;
  const title = h('h1', {tabindex: '-1'}, node.name);
  const badge = statusBadge(node.presence);
  const connection = liveStatus();
  const actions = lifecycleActions(node);
  const deletedNotice = h('div', {class: 'node-deleted-notice', hidden: true}, callout('This node has been deleted.'));
  const sample = latestSamplePanel(node, latest, {signal});
  sample.detach();
  const userID = session.user?.id;
  const unsubscribe = onSession((current) => {
    if (current.user?.id === userID) return;
    sample?.snapshot(null);
    sample?.close();
    unsubscribe();
  });
  signal?.addEventListener('abort', unsubscribe, {once: true});
  if (signal?.aborted) unsubscribe();
  const tabs = tabSet([
    {id: 'details', label: 'Details', icon: 'server', build: () => {
      detailsView = detailsTab(node, groups, sample.element,
        history ||= historyPanel(id, signal), traffic ||= trafficPanel(id, signal),
        probes ||= probesPanel(id, signal),
        trafficSettingsPanel(id, billingSettings, (settings) => {
          if (node.deleted) return;
          billingSettings = settings;
          return traffic.reload();
        }, signal, () => node.deleted));
      return {element: detailsView.element, attach: sample.attach, detach: sample.detach};
    }},
    {id: 'public', label: 'Public page', icon: 'globe', build: () => publicTab(node, publicSettings)},
    {id: 'agent', label: 'Agent settings', icon: 'sliders', build: () => {
      agent ||= agentSettingsTab(node, observedRuntime);
      agent.snapshot(node, observedRuntime);
      return agent;
    }},
    {id: 'enrollment', label: 'Enroll agent', icon: 'server', build: () => enrollmentTab(node)}
  ], 'Node sections');
  const root = h('div', null,
    h('header', {class: 'page-head'}, h('div', null, back,
      h('div', {class: 'detail-head'}, title, badge),
      h('div', {class: 'id-chip'}, idText, copy)), h('div', {class: 'detail-actions'}, connection.element, actions)),
    deletedNotice, tabs);
  const lockDeleted = () => {
    if (!node.deleted) return;
    for (const form of root.querySelectorAll('form')) {
      for (const control of form.querySelectorAll('input,select,textarea,button')) {
        if (control.tagName.toLowerCase() === 'textarea' || (control.tagName.toLowerCase() === 'input' && !['checkbox', 'radio', 'submit', 'button'].includes(control.type))) control.readOnly = true;
        else control.disabled = true;
      }
    }
    for (const control of actions.querySelectorAll('button')) control.disabled = true;
    const enrollment = root.querySelector('#panel-enrollment');
    for (const control of enrollment?.querySelectorAll('button') || []) control.disabled = true;
    root.querySelector('#tab-enrollment').disabled = true;
  };
  root.addEventListener('submit', (event) => {
    if (node.deleted) { event.preventDefault(); event.stopImmediatePropagation(); }
  }, true);
  root.addEventListener('input', lockDeleted);
  // Lazy tabs and their independent async readers can add controls after deletion.
  const deletionObserver = new MutationObserver(lockDeleted);
  signal?.addEventListener('abort', () => deletionObserver.disconnect(), {once: true});
  connectConsoleLive({nodeID: id, signal, onState: connection.update, onClock: sample.clock,
    onSnapshot(data, frame) {
      if (!data.node) {
        node.deleted = true;
        observedRuntime = null;
        agent?.snapshot(node, null);
        deletionObserver.observe(root, {childList: true, subtree: true});
        deletedNotice.hidden = false;
        patchStatusBadge(badge, 'deleted');
        detailsView?.snapshot(node);
        sample.snapshot(null);
        sample.close();
        lockDeleted();
        return;
      }
      if (node.deleted) return;
      Object.assign(node, data.node);
      observedRuntime = data.runtime;
      if (title.textContent !== node.name) title.textContent = node.name;
      patchStatusBadge(badge, node.presence);
      actions.snapshot();
      detailsView?.snapshot(node);
      sample.snapshot(data.latest, frame.server_time);
      agent?.snapshot(node, observedRuntime);
    }
  });
  return root;
}

// Wires the shared save/discard behaviour of the editable sections.
function editableForm(form, controls, save, node) {
  trackDirty(form, controls);
  const syncDirty = form.syncDirty;
  form.syncDirty = () => {
    syncDirty();
    if (node.deleted) for (const control of form.querySelectorAll('button')) control.disabled = true;
  };
  controls.discard.addEventListener('click', () => { if (!node.deleted) void refresh().catch(ignoreNavigationCancellation); });
  onSubmit(form, controls.save, async () => {
    if (node.deleted) return;
    const message = await save();
    if (node.deleted) return;
    form.markClean();
    await refresh();
    toast(message);
  });
}

function detailsTab(node, groups, sample, history, traffic, probes, billing) {
  const group = groups.find((item) => item.id === node.group_id);
  const summaryValues = readonlyFacts();
  const summaryBadge = statusBadge(node.presence);
  const snapshot = (current) => {
    const currentGroup = groups.find((item) => item.id === current.group_id);
    patchStatusBadge(summaryBadge, current.deleted ? 'deleted' : current.presence);
    summaryValues.update([
      ['Presence', summaryBadge],
      ['Last seen', current.last_seen ? absoluteWhen(current.last_seen) : 'Never'],
      ['Group', currentGroup ? currentGroup.name : current.group_id ? `Unknown (${shortID(current.group_id)})` : 'Ungrouped'],
      ['Region', current.region], ['Provider', current.provider],
      ['Tags', current.tags.length ? h('div', {class: 'cell-tags'}, current.tags.map((tag) => h('span', {class: 'tag static'}, tag))) : null],
      ['Added', when(current.created_at)], ['Notes', current.notes]
    ]);
  };
  snapshot(node);
  const summary = panel('Summary', {}, summaryValues.element);
  summary.classList.add('split-aside');

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
  }, node);
  return {element: h('div', {class: 'stack'}, h('div', {class: 'split'}, summary, panel('Edit details', {}, form)), sample, history, traffic, billing, probes), snapshot};
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
  previewPanel.classList.add('split-aside');


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
  }, node);
  return h('div', {class: 'split'}, panel('Public page', {}, h('div', {class: 'stack'}, hidden, privacy, form)), previewPanel);
}
