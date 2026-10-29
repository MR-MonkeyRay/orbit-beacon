import {t} from '../i18n.js';
import {h, icon} from '../../shared/dom.js';
import {copyElementText} from '../../shared/clipboard.js';
import {api, getNavigationSignal, groupsList, invalidate, onSession, session} from '../api.js';
import {absoluteWhen, shortID, when} from '../format.js';
import {connectConsoleLive} from '../live.js';
import {ignoreNavigationCancellation, refresh} from '../router.js';
import {callout, iconButton, link, liveStatus, pageHead, panel, patchStatusBadge, readonlyFacts, statusBadge} from '../ui/components.js';
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
import {uuid} from '../../shared/uuid.js';

export const NODE_PATH = new RegExp(`^/nodes/(${uuid.source.slice(1, -1)})$`, 'i');

export async function nodePage() {
  const signal = getNavigationSignal();
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
  const copy = iconButton('copy', t('nodes.copyID'), async () => {
    const result = await copyElementText(idText);
    if (result === 'clipboard') {
      showCopied();
      toast(t('nodes.copiedID'));
    } else if (result === 'fallback') {
      showCopied();
      toast(t('nodes.copiedIDFallback'));
    } else toast(t('nodes.copyIDManual'), {error: true});
  });
  const back = link('/nodes', icon('left'), t('nodes.allNodes'));
  back.className = 'back';
  let history;
  let traffic;
  let billingSettings = trafficSettings;
  let probes;
  let observedRuntime = runtime.runtime;
  let detailsView;
  let agent;
  const badge = statusBadge(node.presence);
  const connection = liveStatus();
  const actions = lifecycleActions(node);
  const deletedNotice = h('div', {class: 'node-deleted-notice', hidden: true}, callout(t('nodes.deletedNotice')));
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
    {id: 'details', label: t('nodes.details'), icon: 'server', build: () => {
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
    {id: 'public', label: t('nodes.publicPage'), icon: 'globe', build: () => publicTab(node, publicSettings)},
    {id: 'agent', label: t('nodes.agentSettings'), icon: 'sliders', build: () => {
      agent ||= agentSettingsTab(node, observedRuntime);
      agent.snapshot(node, observedRuntime);
      return agent;
    }},
    {id: 'enrollment', label: t('nodes.enrollAgent'), icon: 'server', build: () => enrollmentTab(node)}
  ], t('nodes.sections'));
  const head = pageHead(node.name, null, connection.element, actions);
  const title = head.querySelector('h1');
  head.lastElementChild.className = 'detail-actions';
  head.firstElementChild.replaceChildren(back,
    h('div', {class: 'detail-head'}, title, badge), h('div', {class: 'id-chip'}, idText, copy));
  const root = h('div', null, head,
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
      [t('nodes.presence'), summaryBadge],
      [t('nodes.lastSeen'), current.last_seen ? absoluteWhen(current.last_seen) : t('nodes.never')],
      [t('nodes.group'), currentGroup ? currentGroup.name : current.group_id ? t('nodes.unknownID', {id: shortID(current.group_id)}) : t('nodes.ungrouped')],
      [t('nodes.region'), current.region], [t('nodes.provider'), current.provider],
      [t('nodes.tags'), current.tags.length ? h('div', {class: 'cell-tags'}, current.tags.map((tag) => h('span', {class: 'tag static'}, tag))) : null],
      [t('nodes.added'), when(current.created_at)], [t('nodes.notes'), current.notes]
    ]);
  };
  snapshot(node);
  const summary = panel(t('nodes.summary'), {}, summaryValues.element);
  summary.classList.add('split-aside');

  const name = input({name: 'name', required: true, value: node.name, id: 'node-name'});
  validateBytes(name, 1, 255, t('nodes.name'));
  const region = input({name: 'region', value: node.region, id: 'node-region'});
  const provider = input({name: 'provider', value: node.provider, id: 'node-provider'});
  const choices = [['', t('nodes.ungrouped')], ...groups.map((item) => [item.id, item.name])];
  if (node.group_id && !group) choices.push([node.group_id, t('nodes.unknownID', {id: shortID(node.group_id)})]);
  const groupID = select(choices, node.group_id || '', {name: 'group_id', id: 'node-group'});
  const tags = tagEditor(node.tags);
  const tagBox = tags.querySelector('[data-tags]');
  const notes = textarea({name: 'notes', id: 'node-notes'}, node.notes);
  const controls = saveBar(t('nodes.saveChanges'), 'node-save');
  const form = h('form', {class: 'form form-wide'},
    field(t('nodes.name'), name, {hint: t('nodes.internalNameHint')}),
    h('div', {class: 'form-row'}, field(t('nodes.region'), region), field(t('nodes.provider'), provider)),
    field(t('nodes.group'), groupID), tags, field(t('nodes.notes'), notes), controls.bar);
  editableForm(form, controls, async () => {
    if (!tagBox.commit()) throw new Error(t('nodes.tagInvalid'));
    const result = await api(`/nodes/${encodeURIComponent(node.id)}`, {method: 'PATCH', body: {
      name: name.value, notes: notes.value, region: region.value, provider: provider.value, group_id: groupID.value || null, tags: tagBox.getValue()
    }});
    invalidate('nodes');
    return t('nodes.saved', {name: result.name});
  }, node);
  return {element: h('div', {class: 'stack'}, h('div', {class: 'split'}, summary, panel(t('nodes.editDetails'), {}, form)), sample, history, traffic, billing, probes), snapshot};
}

function publicPreview() {
  const name = h('h3');
  const where = h('div', {class: 'where'});
  const badge = h('span', {class: 'badge'});
  const element = h('div', {class: 'preview'}, h('p', {class: 'preview-label'}, t('nodes.previewIntro')),
    h('div', {class: 'preview-card'}, h('div', {class: 'top'}, name, badge), where));
  element.paint = ({enabled, name: title, region, provider}) => {
    name.replaceChildren(title || h('span', {class: 'placeholder'}, t('nodes.publicName')));
    where.replaceChildren(...[region && h('span', null, icon('pin'), region), provider && h('span', null, icon('cloud'), provider)].filter(Boolean));
    if (!region && !provider) where.append(h('span', {class: 'placeholder'}, t('nodes.noLocation')));
    badge.className = `badge ${enabled ? 'online' : 'disabled'}`;
    badge.textContent = enabled ? t('nodes.published') : t('nodes.hidden');
  };
  return element;
}

function publicTab(node, settings) {
  const hidden = node.status === 'pending' || node.status === 'disabled'
    ? callout(t('nodes.publicUnavailable', {status: t(`common.status.${node.status}`)})) : null;
  const privacy = callout(t('nodes.privacy'), 'info');
  const preview = publicPreview();
  const previewPanel = panel(t('nodes.preview'), {action: h('a', {class: 'btn btn-sm', href: '/'}, icon('external'), t('nodes.viewPublic'))}, preview);
  previewPanel.classList.add('split-aside');


  const enabled = h('input', {type: 'checkbox', name: 'enabled', id: 'public-enabled', checked: settings.enabled});
  const name = input({name: 'public_name', value: settings.name, id: 'public-name', autocomplete: 'off', dataset: {field: 'name'}});
  const region = input({name: 'public_region', value: settings.region, id: 'public-region', autocomplete: 'off', dataset: {field: 'region'}});
  const provider = input({name: 'public_provider', value: settings.provider, id: 'public-provider', autocomplete: 'off', dataset: {field: 'provider'}});
  const checkName = validateBytes(name, 0, 255, t('nodes.publicName'));
  validateBytes(region, 0, 128, t('nodes.publicRegion'));
  validateBytes(provider, 0, 128, t('nodes.publicProvider'));
  // The server trims text, so the preview and the required check do too.
  const sync = () => {
    name.required = enabled.checked;
    if (enabled.checked && name.value.trim() === '') name.setCustomValidity(t('nodes.publicNameRequired'));
    else checkName();
    preview.paint({enabled: enabled.checked, name: name.value.trim(), region: region.value.trim(), provider: provider.value.trim()});
  };
  for (const control of [enabled, name, region, provider]) control.addEventListener('input', sync);
  enabled.addEventListener('change', sync);
  sync();
  const controls = saveBar(t('nodes.savePublic'), 'public-save');
  const form = h('form', {class: 'form form-wide'},
    h('label', {class: 'switch'}, enabled, h('span', {class: 'switch-track', 'aria-hidden': 'true'}),
      h('span', {class: 'switch-text'}, h('strong', null, t('nodes.publish')), h('span', null, t('nodes.publishHint')))),
    field(t('nodes.publicName'), name, {hint: t('nodes.publicNameHint')}),
    h('div', {class: 'form-row'}, field(t('nodes.publicRegion'), region, {hint: t('nodes.publicLocationHint')}), field(t('nodes.publicProvider'), provider, {hint: t('nodes.publicLocationHint')})),
    controls.bar);
  editableForm(form, controls, async () => {
    await api(`/nodes/${encodeURIComponent(node.id)}/public-settings`, {method: 'PATCH', body: {enabled: enabled.checked, name: name.value, region: region.value, provider: provider.value}});
    return enabled.checked ? t('nodes.publicSaved') : t('nodes.publicHidden');
  }, node);
  return h('div', {class: 'split'}, panel(t('nodes.publicPage'), {}, h('div', {class: 'stack'}, hidden, privacy, form)), previewPanel);
}
