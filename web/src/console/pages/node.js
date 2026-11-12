import {t} from '../i18n.js';
import {h, icon} from '../../shared/dom.js';
import {copyElementText} from '../../shared/clipboard.js';
import {api, getNavigationSignal, groupsList, invalidate, onSession, session} from '../api.js';
import {displayStatus, absoluteWhen, shortID} from '../format.js';
import {connectConsoleLive} from '../live.js';
import {callout, iconButton, link, liveStatus, panel, patchStatusBadge, statusBadge} from '../ui/components.js';
import {toast} from '../ui/feedback.js';
import {field, input, onSubmit, saveBar, select, tagEditor, textarea, toggleSwitch, trackDirty, validateBytes} from '../ui/forms.js';
import {tabSet} from '../ui/tabs.js';
import {nodeCommandsPanel} from './node-commands.js';
import {latestSamplePanel} from './latest-sample.js';
import {lifecycleActions} from './node-lifecycle.js';
import {historyPanel} from './node-history.js';
import {availabilityPanel} from './node-availability.js';
import {trafficPanel} from './node-traffic.js';
import {trafficSettingsPanel} from './node-traffic-settings.js';
import {probesPanel} from './node-probes.js';
import {NODE_PATH} from '../routes.js';
import {pagePlaceholder, placeholderChart, placeholderPanel, skLines, skText, skValue} from '../ui/placeholders.js';

const NODE_TABS = [['monitor', 'common.monitoring', 'monitor'], ['traffic', 'nodes.traffic', 'cloud'], ['probes', 'nodes.probes', 'check'],
  ['public', 'settings.publicPage', 'globe'], ['manage', 'nodes.manage', 'key'], ['settings', 'shell.settings', 'sliders']];

// 与节点详情同构：返回链接、标题行、标签条（按 hash 选中）与监控页的指标卡和图框先出现。
export function nodePlaceholder() {
  const hash = location.hash.slice(1);
  const selected = NODE_TABS.some(([id]) => id === hash) ? hash : 'monitor';
  const head = h('header', {class: 'page-head node-head'},
    h('div', {class: 'detail-head'}, h('span', {class: 'back'}, icon('left')), h('h1', null, skText('8em')), h('span', {class: 'sk sk-badge', 'aria-hidden': 'true'})),
    h('div', {class: 'detail-actions'}, h('span', {class: 'live-status'}, t('controls.live.connecting'))));
  const summary = h('div', {class: 'node-summary'}, skText('6em'), skText('8em'), skText('5em'), skText('9em'));
  const tabs = h('div', {class: 'tabs', 'aria-hidden': 'true'}, NODE_TABS.map(([id, label, name]) =>
    h('span', {class: 'tab-placeholder', dataset: {selected: String(id === selected)}}, icon(name), t(label))));
  const tile = (label) => h('div', {class: 'sample-tile'}, h('dt', null, t(label)), h('dd', null, skValue(), h('span', {class: 'sk sk-meter', 'aria-hidden': 'true'})));
  const body = selected === 'monitor'
    ? h('div', {class: 'stack'},
      placeholderPanel(t('nodes.sample'), h('div', {class: 'sample-dashboard'},
        h('dl', {class: 'sample-primary'}, tile('nodes.cpu'), tile('nodes.memory'), tile('nodes.disk'), tile('nodes.loadMinute')))),
      placeholderPanel(t('public.availability'), h('span', {class: 'sk sk-availability', 'aria-hidden': 'true'})),
      placeholderPanel(t('nodes.history'), h('div', {class: 'history-grid'},
        [0, 1].map(() => h('section', {class: 'history-metric'}, h('h3', null, skText('6em')), placeholderChart())))))
    : placeholderPanel(null, skLines('42%', '64%', '36%', '58%'));
  return pagePlaceholder(head, summary, tabs, body);
}

export async function nodePage() {
  const signal = getNavigationSignal();
  const id = location.pathname.match(NODE_PATH)[1];
  const path = `/nodes/${encodeURIComponent(id)}`;
  const [node, trafficSettings, groups, publicSettings, latest] = await Promise.all([
    api(path), api(`${path}/traffic-settings`), groupsList(), api(`${path}/public-settings`),
    api(`${path}/latest`).catch((error) => {
      if (error.status === 404) return null;
      throw error;
    })
  ]);
  const idText = h('span', {class: 'mono', title: node.id}, shortID(node.id));
  const fullID = h('span', {class: 'sr'}, node.id);
  const showCopied = () => {
    copy.replaceChildren(icon('check'));
    setTimeout(() => copy.replaceChildren(icon('copy')), 1600);
  };
  const copy = iconButton('copy', t('nodes.copyID'), async () => {
    const result = await copyElementText(fullID);
    if (result === 'clipboard') {
      showCopied();
      toast(t('nodes.copiedID'));
    } else if (result === 'fallback') {
      showCopied();
      toast(t('nodes.copiedIDFallback'));
    } else toast(t('nodes.copyIDManual'), {error: true});
  });
  // 固定页头只留一行：返回链接收成图标，文字保留为可访问名称与悬停提示。
  const back = link('/nodes', icon('left'), h('span', {class: 'sr'}, t('nodes.allNodes')));
  back.className = 'back';
  back.title = t('nodes.allNodes');
  let history;
  let currentLatest = latest;
  let currentServerTime = latest?.server_time;
  let sample;
  let traffic;
  let availability;
  let billingSettings = trafficSettings;
  let probes;
  let commands;
  const badge = statusBadge(displayStatus(node));
  const connection = liveStatus();
  const actions = lifecycleActions(node);
  const publicBadge = h('a', {class: 'badge node-public-badge', href: '#public', onclick: (event) => {
    event.preventDefault();
    void tabs.selectTab('public');
  }});
  const publicLabel = h('span');
  const publicDot = h('span', {class: 'publication-dot', 'aria-hidden': 'true'});
  let publication;
  const updatePublic = () => {
    const state = publicSettings.enabled ? t('nodes.published') : t('nodes.notPublished');
    publicBadge.textContent = state;
    publicBadge.dataset.published = String(publicSettings.enabled);
    publicLabel.textContent = t('settings.publicPage');
    publicDot.dataset.published = String(publicSettings.enabled);
    const tab = tabs.querySelector('#tab-public');
    tab.setAttribute('aria-label', `${t('settings.publicPage')}: ${state}`);
    publication?.snapshot();
  };
  const summary = h('div', {class: 'node-summary'});
  const updateSummary = () => {
    const group = groups.find((item) => item.id === node.group_id);
    const detected = !node.region && node.geo_location;
    const location = node.region || node.geo_location;
    const parts = [group?.name || (node.group_id ? t('nodes.unknownID', {id: shortID(node.group_id)}) : null),
      location ? h('span', {class: 'node-location'}, detected ? countryFlag(node.geo_country) : '', location,
        detected ? h('small', {class: 'node-auto', title: t('nodes.detectedLocation')}, t('nodes.auto')) : null) : null,
      node.provider,
      node.tags?.length ? h('span', {class: 'cell-tags'}, node.tags.map((tag) => h('span', {class: 'tag static'}, tag))) : null,
      node.remote_ip ? h('span', {class: 'mono'}, node.remote_ip) : null,
      h('span', {class: 'node-id'}, idText, fullID, copy),
      node.last_seen ? h('span', null, t('nodes.lastSeen'), ' ', absoluteWhen(node.last_seen)) : null].filter(Boolean);
    summary.replaceChildren(...parts.map((part) => part instanceof Node ? part : h('span', null, part)));
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
    {id: 'public', label: h('span', {class: 'node-public-label'}, publicLabel, publicDot), icon: 'globe', build: () => {
      publication = publicTab(node, publicSettings, updatePublic);
      return publication.element;
    }},
    {id: 'manage', label: t('nodes.manage'), icon: 'key', build: () => {
      commands ||= nodeCommandsPanel(node, signal);
      commands.snapshot(node);
      return {element: h('div', {class: 'stack node-manage'}, commands.element, actions.manage()),
        attach: commands.attach, detach: commands.detach};
    }},
    {id: 'settings', label: t('shell.settings'), icon: 'sliders', build: () => detailsTab(node, groups, () => {
      title.textContent = node.name;
      updateSummary();
      actions.snapshot();
    })}
  ], t('nodes.sections'));
  updatePublic();
  const title = h('h1', {tabindex: '-1'}, node.name);
  const head = h('header', {class: 'page-head node-head'},
    h('div', {class: 'detail-head'}, back, title, badge, publicBadge),
    h('div', {class: 'detail-actions'}, connection.element, actions));
  const root = h('div', null, head, summary,
    deletedNotice, tabs);
  const lockDeleted = () => {
    if (!node.deleted) return;
    for (const form of root.querySelectorAll('form')) {
      for (const control of form.querySelectorAll('input,select,textarea,button')) {
        if (control.tagName.toLowerCase() === 'textarea' || (control.tagName.toLowerCase() === 'input' && !['checkbox', 'radio', 'submit', 'button'].includes(control.type))) control.readOnly = true;
        else control.disabled = true;
      }
    }
    actions.snapshot();
    for (const control of root.querySelector('.node-manage')?.querySelectorAll('button') || []) control.disabled = true;
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
        commands?.snapshot(node);
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
      if (title.textContent !== node.name) title.textContent = node.name;
      patchStatusBadge(badge, displayStatus(node));
      actions.snapshot();
      updateSummary();
      publication?.snapshot();
      currentLatest = data.latest;
      history?.snapshot(currentLatest);
      currentServerTime = frame.server_time;
      sample?.snapshot(data.latest, currentServerTime);
      commands?.snapshot(node);
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

function countryFlag(country) {
  const code = (country || '').toUpperCase();
  return /^[A-Z]{2}$/.test(code) ? `${String.fromCodePoint(...[...code].map((letter) => 127397 + letter.charCodeAt(0)))} ` : '';
}

function detailsTab(node, groups, onSaved) {
  const group = groups.find((item) => item.id === node.group_id);
  const name = input({name: 'name', required: true, value: node.name, id: 'node-name'});
  validateBytes(name, 1, 255, t('nodes.name'));
  const region = input({name: 'region', value: node.region, id: 'node-region',
    placeholder: node.geo_location ? t('nodes.autoLocation', {location: node.geo_location}) : t('nodes.autoDetect')});
  const provider = input({name: 'provider', value: node.provider, id: 'node-provider'});
  const choices = [['', t('nodes.ungrouped')], ...groups.map((item) => [item.id, item.name])];
  if (node.group_id && !group) choices.push([node.group_id, t('nodes.unknownID', {id: shortID(node.group_id)})]);
  const groupID = select(choices, node.group_id || '', {name: 'group_id', id: 'node-group'});
  const tags = tagEditor(node.tags);
  const tagBox = tags.querySelector('[data-tags]');
  const expiresOn = input({name: 'expires_on', type: 'date', value: node.expires_on || '', id: 'node-expires-on'});
  const notes = textarea({name: 'notes', id: 'node-notes'}, node.notes);
  const extra = ['display_order', 'price_amount', 'price_currency', 'billing_cycle', 'auto_renew', 'bandwidth', 'ipv4', 'ipv6', 'network_route', 'plan_extra'].map((key) => {
    const boolean = ['auto_renew', 'ipv4', 'ipv6'].includes(key);
    const control = key === 'billing_cycle' ? select(['', 'month', 'quarter', 'half_year', 'year', 'once'].map((value) => [value, t(value ? `nodes.cycle.${value}` : 'nodes.cycle.none')]), node[key] || '', {name: key})
      : input({name: key, type: boolean ? 'checkbox' : key === 'display_order' ? 'number' : 'text', ...(boolean ? {checked: node[key]} : {value: node[key] ?? ''})});
    if (key === 'display_order') { control.min = '-1000000'; control.max = '1000000'; control.step = '1'; }
    return [key, control, boolean];
  });
  const get = (key) => extra.find(([name]) => name === key)[1];
  const row = (key) => field(t(`nodes.field.${key}`), get(key));
  const price = h('div', {class: 'node-price'}, row('price_amount'), row('price_currency'));
  const ip = h('div', {class: 'node-inline-checks'}, ['ipv4', 'ipv6'].map((key) => h('label', null, get(key), t(`nodes.field.${key}`))));
  const controls = saveBar(t('nodes.saveChanges'), 'node-save');
  controls.bar.classList.add('node-save-bar');
  const card = (label, ...rows) => {
    const result = panel(label, {}, h('div', {class: 'node-settings-fields'}, rows));
    result.classList.add('node-settings-card');
    return result;
  };
  const form = h('form', {class: 'form form-wide node-settings'},
    card(t('nodes.basic'), field(t('nodes.name'), name), field(t('nodes.group'), groupID), tags, row('display_order')),
    card(t('nodes.location'), field(t('nodes.region'), region, {hint: t('nodes.regionHint')}), field(t('nodes.provider'), provider)),
    card(t('nodes.plan'), field(t('nodes.expiresOn'), expiresOn),
      h('div', {class: 'field node-price-row'}, h('span', {class: 'field-label'}, t('nodes.field.price')), price), row('billing_cycle'),
      h('div', {class: 'node-switch-row'}, toggleSwitch(t('nodes.field.auto_renew'), get('auto_renew'))),
      row('bandwidth'), h('div', {class: 'field node-ip-row'}, h('span', {class: 'field-label'}, t('nodes.field.ip_version')), ip),
      row('network_route'), row('plan_extra')),
    card(t('nodes.notes'), field(t('nodes.privateNotes'), notes)), controls.bar);
  editableForm(form, controls, async () => {
    if (!tagBox.commit()) throw new Error(t('nodes.tagInvalid'));
    const result = await api(`/nodes/${encodeURIComponent(node.id)}`, {method: 'PATCH', body: {
      ...Object.fromEntries(extra.map(([key, control, boolean]) => [key, boolean ? control.checked : key === 'display_order' ? Number(control.value) : control.value])),
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
    for (const [key, control, boolean] of extra) { if (boolean) control.checked = node[key]; else control.value = node[key] ?? ''; }
    tagBox.setValue(node.tags);
    form.markClean();
  });
  return form;
}

const PUBLIC_GROUPS = [
  ['location', ['region', 'provider']],
  ['metrics', ['cpu', 'memory', 'load', 'uptime', 'availability', 'traffic']],
  ['plan', ['expires_on', 'price', 'billing_cycle', 'bandwidth', 'ip_version', 'network_route', 'plan_extra']]
];

function publicTab(node, settings, onSaved) {
  const enabled = input({type: 'checkbox', name: 'enabled', checked: settings.enabled});
  const checks = PUBLIC_GROUPS.flatMap(([, keys]) => keys.map((key) => [key, input({type: 'checkbox', name: key, checked: settings.fields.includes(key)})]));
  const controls = saveBar(t('nodes.savePublic'), 'public-save');
  const warning = callout(t('nodes.publicUnavailable'));
  const snapshot = () => { warning.hidden = !['pending', 'disabled'].includes(node.status) && !['pending', 'never', 'disabled'].includes(node.presence); };
  const syncChecks = () => { for (const [, check] of checks) check.disabled = !enabled.checked || Boolean(node.deleted); };
  enabled.addEventListener('change', syncChecks);
  syncChecks(); snapshot();
  const groups = PUBLIC_GROUPS.map(([group, keys]) => h('fieldset', {class: 'node-public-group'},
    h('legend', null, t(`nodes.${group}`)),
    h('div', {class: 'node-public-checks'}, keys.map((key) => h('label', {class: 'check-row'},
      checks.find(([name]) => name === key)[1], h('span', null, t(`nodes.field.${key}`)))))));
  const form = h('form', {class: 'form form-wide node-public-form'}, warning,
    toggleSwitch(t('nodes.publish'), enabled), h('div', {class: 'node-public-groups'}, groups), controls.bar);
  editableForm(form, controls, async () => {
    const result = await api(`/nodes/${encodeURIComponent(node.id)}/public-settings`, {method: 'PATCH', body: {
      enabled: enabled.checked, fields: checks.filter(([, check]) => check.checked).map(([key]) => key)
    }});
    if (node.deleted) return;
    Object.assign(settings, result);
    onSaved();
    return enabled.checked ? t('nodes.publicSaved') : t('nodes.publicHidden');
  }, node, () => {
    enabled.checked = settings.enabled;
    for (const [key, check] of checks) check.checked = settings.fields.includes(key);
    syncChecks();
    form.markClean();
  });
  const view = h('a', {href: '/', target: '_blank', rel: 'noopener'}, t('nodes.viewStatusPage'));
  return {element: panel(t('settings.publicPage'), {action: view}, form), snapshot};
}
