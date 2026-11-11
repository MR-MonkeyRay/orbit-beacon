import {t} from '../i18n.js';
import {h, icon} from '../../shared/dom.js';
import {copyElementText} from '../../shared/clipboard.js';
import {api, getNavigationSignal, groupsList, invalidate, onSession, session} from '../api.js';
import {displayStatus, absoluteWhen, shortID} from '../format.js';
import {connectConsoleLive} from '../live.js';
import {callout, iconButton, link, liveStatus, pageHead, panel, patchStatusBadge, statusBadge} from '../ui/components.js';
import {toast} from '../ui/feedback.js';
import {field, input, onSubmit, saveBar, select, tagEditor, textarea, trackDirty, validateBytes} from '../ui/forms.js';
import {tabSet} from '../ui/tabs.js';
import {nodeCommandsPanel} from './node-commands.js';
import {agentSettingsTab} from './node-agent-settings.js';
import {enrollmentTab} from './enrollments.js';
import {latestSamplePanel} from './latest-sample.js';
import {lifecycleActions} from './node-lifecycle.js';
import {historyPanel} from './node-history.js';
import {availabilityPanel} from './node-availability.js';
import {trafficPanel} from './node-traffic.js';
import {trafficSettingsPanel} from './node-traffic-settings.js';
import {probesPanel} from './node-probes.js';
import {NODE_PATH} from '../routes.js';

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
  // 视觉上由 CSS 截断，textContent 保持完整，复制和屏幕阅读器都能拿到全值。
  const idText = h('span', {class: 'mono', title: node.id}, node.id);
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
  let currentLatest = latest;
  let currentServerTime = latest?.server_time;
  let sample;
  let traffic;
  let availability;
  let billingSettings = trafficSettings;
  let probes;
  let observedRuntime = runtime.runtime;
  let tickets;
  let agent;
  let commands;
  const badge = statusBadge(displayStatus(node));
  const connection = liveStatus();
  const actions = lifecycleActions(node, () => tickets?.reload());
  const summary = h('div', {class: 'node-summary'});
  const updateSummary = () => {
    const group = groups.find((item) => item.id === node.group_id);
    const parts = [group?.name || (node.group_id ? t('nodes.unknownID', {id: shortID(node.group_id)}) : null), node.region, node.provider,
      node.tags?.length ? h('span', {class: 'cell-tags'}, node.tags.map((tag) => h('span', {class: 'tag static'}, tag))) : null,
      node.created_at ? h('span', null, t('nodes.added'), ' ', absoluteWhen(node.created_at)) : null,
      node.last_seen ? h('span', null, t('nodes.lastSeen'), ' ', absoluteWhen(node.last_seen)) : null].filter(Boolean);
    summary.replaceChildren(...parts.flatMap((part, index) => index ? [h('span', {'aria-hidden': 'true'}, '·'), part] : [part]));
  };
  updateSummary();
  const deletedNotice = h('div', {class: 'node-deleted-notice', hidden: true}, callout(t('nodes.deletedNotice')));
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
    {id: 'monitor', label: t('common.monitoring'), icon: 'monitor', build: () => {
      if (!sample) {
        sample = latestSamplePanel(node, currentLatest, {signal});
        sample.detach();
        sample.snapshot(currentLatest, currentServerTime);
        if (node.deleted) sample.close();
      }
      return {element: h('div', {class: 'stack'}, sample.element,
        availability ||= availabilityPanel(id, signal), history ||= historyPanel(id, signal, currentLatest)),
      attach: sample.attach, detach: sample.detach};
    }},
    {id: 'traffic', label: t('nodes.traffic'), icon: 'cloud', build: () => {
      traffic ||= trafficPanel(id, signal);
      const billing = trafficSettingsPanel(id, billingSettings, (settings) => {
        if (node.deleted) return;
        billingSettings = settings;
        return traffic.reload();
      }, signal, () => node.deleted);
      return h('div', {class: 'stack'}, traffic, billing);
    }},
    {id: 'probes', label: t('nodes.probes'), icon: 'check', build: () => probes ||= probesPanel(id, signal)},
    {id: 'settings', label: t('shell.settings'), icon: 'sliders', hashes: {
      'settings-details': 'node-details', details: 'node-details',
      'settings-public': 'node-public', public: 'node-public',
      'settings-agent': 'node-agent', agent: 'node-agent',
      'settings-enrollment': 'node-enrollment', enrollment: 'node-enrollment'
    }, build: () => {
      agent ||= agentSettingsTab(node, observedRuntime);
      agent.snapshot(node, observedRuntime);
      commands ||= nodeCommandsPanel(node, observedRuntime, signal, {heading: 'h3'});
      commands.snapshot(node, observedRuntime);
      tickets ||= enrollmentTab(node, {listOnly: true});
      const section = (id, label, ...content) => h('section', {class: 'stack node-settings-section'},
        h('h2', {id, tabindex: '-1'}, label), content);
      return {element: h('div', {class: 'stack node-settings'},
        section('node-details', t('nodes.details'), detailsTab(node, groups, () => {
          title.textContent = node.name;
          updateSummary();
          actions.snapshot();
        })),
        section('node-public', t('nodes.publicPage'), publicTab(node, publicSettings)),
        section('node-agent', 'Agent', agent, commands.element,
          h('section', {class: 'stack node-enrollments'}, h('h3', {id: 'node-enrollment', tabindex: '-1'}, t('nodes.enrollment.caption')), tickets))),
        attach: commands.attach, detach: commands.detach};
    }}
  ], t('nodes.sections'));
  const head = pageHead(node.name, connection.element, actions);
  const title = head.querySelector('h1');
  head.querySelector('.head-actions').classList.add('detail-actions');
  head.firstElementChild.replaceChildren(back,
    h('div', {class: 'detail-head'}, title, badge), summary, h('div', {class: 'id-chip'}, idText, copy));
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
    const enrollment = root.querySelector('.node-enrollments');
    for (const control of enrollment?.querySelectorAll('button') || []) control.disabled = true;
  };
  root.addEventListener('submit', (event) => {
    if (node.deleted) { event.preventDefault(); event.stopImmediatePropagation(); }
  }, true);
  root.addEventListener('input', lockDeleted);
  // 懒加载标签页及其独立的异步读取器可能在删除后仍添加控件。
  const deletionObserver = new MutationObserver(lockDeleted);
  signal?.addEventListener('abort', () => deletionObserver.disconnect(), {once: true});
  connectConsoleLive({nodeID: id, signal, onState: connection.update, onClock: (serverTime) => { currentServerTime = serverTime; sample?.clock(serverTime); },
    onSnapshot(data, frame) {
      if (!data.node) {
        node.deleted = true;
        observedRuntime = null;
        agent?.snapshot(node, null);
        commands?.snapshot(node, null);
        deletionObserver.observe(root, {childList: true, subtree: true});
        deletedNotice.hidden = false;
        patchStatusBadge(badge, 'deleted');
        currentLatest = null;
        sample?.snapshot(null);
        sample?.close();
        lockDeleted();
        return;
      }
      if (node.deleted) return;
      Object.assign(node, data.node);
      observedRuntime = data.runtime;
      if (title.textContent !== node.name) title.textContent = node.name;
      patchStatusBadge(badge, displayStatus(node));
      actions.snapshot();
      updateSummary();
      currentLatest = data.latest;
      history?.snapshot(currentLatest);
      currentServerTime = frame.server_time;
      sample?.snapshot(data.latest, currentServerTime);
      agent?.snapshot(node, observedRuntime);
      commands?.snapshot(node, observedRuntime);
    }
  });
  return root;
}

// 接入可编辑区块共享的保存/放弃行为。
function editableForm(form, controls, save, node, discard) {
  trackDirty(form, controls);
  const syncDirty = form.syncDirty;
  form.syncDirty = () => {
    syncDirty();
    if (node.deleted) for (const control of form.querySelectorAll('button')) control.disabled = true;
  };
  controls.discard.addEventListener('click', () => { if (!node.deleted) discard(); });
  onSubmit(form, controls.save, async () => {
    if (node.deleted) return;
    const message = await save();
    if (node.deleted) return;
    form.markClean();
    toast(message);
  });
}

function detailsTab(node, groups, onSaved) {
  const group = groups.find((item) => item.id === node.group_id);
  const name = input({name: 'name', required: true, value: node.name, id: 'node-name'});
  validateBytes(name, 1, 255, t('nodes.name'));
  const region = input({name: 'region', value: node.region, id: 'node-region'});
  const provider = input({name: 'provider', value: node.provider, id: 'node-provider'});
  const choices = [['', t('nodes.ungrouped')], ...groups.map((item) => [item.id, item.name])];
  if (node.group_id && !group) choices.push([node.group_id, t('nodes.unknownID', {id: shortID(node.group_id)})]);
  const groupID = select(choices, node.group_id || '', {name: 'group_id', id: 'node-group'});
  const tags = tagEditor(node.tags);
  const tagBox = tags.querySelector('[data-tags]');
  const expiresOn = input({name: 'expires_on', type: 'date', value: node.expires_on || '', id: 'node-expires-on'});
  const notes = textarea({name: 'notes', id: 'node-notes'}, node.notes);
  const controls = saveBar(t('nodes.saveChanges'), 'node-save');
  const form = h('form', {class: 'form form-wide'},
    field(t('nodes.name'), name),
    h('div', {class: 'form-row'}, field(t('nodes.region'), region), field(t('nodes.provider'), provider)),
    field(t('nodes.group'), groupID), tags, field(t('nodes.expiresOn'), expiresOn), field(t('nodes.notes'), notes), controls.bar);
  editableForm(form, controls, async () => {
    if (!tagBox.commit()) throw new Error(t('nodes.tagInvalid'));
    const result = await api(`/nodes/${encodeURIComponent(node.id)}`, {method: 'PATCH', body: {
      name: name.value, expires_on: expiresOn.value || null, notes: notes.value, region: region.value, provider: provider.value, group_id: groupID.value || null, tags: tagBox.getValue()
    }});
    if (node.deleted) return;
    Object.assign(node, result);
    invalidate('nodes');
    onSaved();
    return t('nodes.saved', {name: result.name});
  }, node, () => {
    name.value = node.name; region.value = node.region; provider.value = node.provider; groupID.value = node.group_id || '';
    expiresOn.value = node.expires_on || ''; notes.value = node.notes;
    tagBox.setValue(node.tags);
    form.markClean();
  });
  return panel(null, {}, form);
}

function publicPreview() {
  const name = h('h3');
  const where = h('div', {class: 'where'});
  const badge = h('span', {class: 'badge'});
  const element = h('div', {class: 'preview-card'}, h('div', {class: 'top'}, name, badge), where);
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
  const previewPanel = panel(t('nodes.preview'), {heading: 'h3', action: h('a', {class: 'btn btn-sm', href: '/'}, icon('external'), t('nodes.viewPublic'))}, preview);
  previewPanel.classList.add('split-aside');


  const enabled = h('input', {type: 'checkbox', name: 'enabled', id: 'public-enabled', checked: settings.enabled});
  const name = input({name: 'public_name', value: settings.name, id: 'public-name', autocomplete: 'off', dataset: {field: 'name'}});
  const region = input({name: 'public_region', value: settings.region, id: 'public-region', autocomplete: 'off', dataset: {field: 'region'}});
  const provider = input({name: 'public_provider', value: settings.provider, id: 'public-provider', autocomplete: 'off', dataset: {field: 'provider'}});
  const checkName = validateBytes(name, 0, 255, t('nodes.publicName'));
  validateBytes(region, 0, 128, t('nodes.publicRegion'));
  validateBytes(provider, 0, 128, t('nodes.publicProvider'));
  // 与服务端一致，预览和必填检查都先去除文本首尾空白。
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
      h('span', {class: 'switch-text'}, h('strong', null, t('nodes.publish')))),
    field(t('nodes.publicName'), name),
    h('div', {class: 'form-row'}, field(t('nodes.publicRegion'), region), field(t('nodes.publicProvider'), provider)),
    controls.bar);
  editableForm(form, controls, async () => {
    const result = await api(`/nodes/${encodeURIComponent(node.id)}/public-settings`, {method: 'PATCH', body: {enabled: enabled.checked, name: name.value, region: region.value, provider: provider.value}});
    if (node.deleted) return;
    Object.assign(settings, result);
    return enabled.checked ? t('nodes.publicSaved') : t('nodes.publicHidden');
  }, node, () => {
    enabled.checked = settings.enabled; name.value = settings.name; region.value = settings.region; provider.value = settings.provider;
    sync();
    form.markClean();
  });
  // 分段标题已标识状态页设置，预览保留自己的标题。
  return h('div', {class: 'split'}, panel(null, {}, h('div', {class: 'stack'}, hidden, privacy, form)), previewPanel);
}
