import {h, icon} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {api, session, getNavigationSignal, serviceGroupsList} from '../api.js';
import {formatMilliseconds, formatPercent, shortID} from '../format.js';
import {button, dataTable, emptyState, iconButton, pageHead, panel} from '../ui/components.js';
import {pagePlaceholder, placeholderHead, placeholderPanel, placeholderTable} from '../ui/placeholders.js';
import {confirmDialog, formDialog, modal} from '../ui/dialogs.js';
import {field, input, select, toggleSwitch} from '../ui/forms.js';
import {chart} from '../ui/chart.js';
import {toastError} from '../ui/feedback.js';
import {errorText} from '../../shared/errors.js';
import {SERVICE_GROUPS, groupSelect, manageGroupsDialog} from '../lib/groups.js';

const numeric = ['interval_seconds', 'timeout_seconds', 'latency_threshold_ms', 'display_order'];
const switches = ['public', 'notify', 'enabled'];
export function servicePayload(controls) {
  return {name: controls.name.value.trim(), type: controls.type.value, target: controls.target.value.trim(),
    ...Object.fromEntries(numeric.map((key) => [key, Number(controls[key].value)])),
    ...Object.fromEntries(switches.map((key) => [key, controls[key].checked])),
    // 分组读取失败时编辑框没有分组字段，PATCH 省略 group_id 即保持原分组。
    ...(controls.group_id ? {group_id: controls.group_id.value || null} : {})};
}
// groups 为 null 表示分组读取失败，此时不显示分组字段。
export function serviceEditor(item, {signal, saved, groups = null, onGroupCreated}) {
  if (!session.user || !session.csrf || signal?.aborted) return;
  const controls = {};
  return formDialog({title: t(item ? 'services.edit' : 'services.add'), submit: t('services.save'),
    build(form) {
      const add = (key, props, options) => { const control = controls[key] = input({name: key, ...props}); return field(t(`services.${key}`), control, options); };
      const name = add('name', {value: item?.name || '', required: true, maxlength: 100});
      const type = controls.type = select([['http', 'HTTP / HTTPS'], ['tcp', 'TCP'], ['icmp', 'ICMP']], item?.type || 'http', {name: 'type'});
      const target = add('target', {value: item?.target || '', required: true, maxlength: 2048, autocomplete: 'off'});
      const sync = () => { controls.target.placeholder = {http: 'https://example.com/health', tcp: 'example.com:443', icmp: 'example.com'}[type.value]; };
      type.addEventListener('change', sync); sync();
      const known = groups?.some((group) => group.id === item?.group_id);
      const group = groups ? field(t('nodes.group'), controls.group_id = groupSelect(groups, item?.group_id, {name: 'group_id'}, {kind: SERVICE_GROUPS, onCreated: onGroupCreated,
        extra: item?.group_id && !known ? [[item.group_id, t('nodes.unknownID', {id: shortID(item.group_id)})]] : []})) : null;
      const numberFields = numeric.map((key) => add(key, {type: 'number', required: true,
        value: item?.[key] ?? ({interval_seconds: 60, timeout_seconds: 10, latency_threshold_ms: 0, display_order: 0})[key],
        min: key === 'display_order' ? -1000000 : key === 'interval_seconds' ? 30 : key === 'timeout_seconds' ? 1 : 0,
        max: ({interval_seconds: 3600, timeout_seconds: 30, latency_threshold_ms: 3600000, display_order: 1000000})[key], step: key === 'latency_threshold_ms' ? 'any' : 1},
        key === 'latency_threshold_ms' ? {hint: t('services.thresholdHint')} : undefined));
      const checkTimeout = () => controls.timeout_seconds.setCustomValidity(Number(controls.timeout_seconds.value) >= Number(controls.interval_seconds.value) ? t('services.timeoutHint') : '');
      controls.interval_seconds.addEventListener('input', checkTimeout); controls.timeout_seconds.addEventListener('input', checkTimeout);
      const toggles = switches.map((key) => { const control = controls[key] = input({name: key, type: 'checkbox', checked: item?.[key] ?? key === 'enabled'}); return toggleSwitch(t(`services.${key}`), control); });
      // 原生 append 会把 null 写成文本，缺少分组字段时先滤掉。
      form.append(...[name, field(t('services.type'), type), target, group, h('div', {class: 'service-editor-grid'}, numberFields),
        h('div', {class: 'service-switches'}, toggles)].filter(Boolean));
    },
    async action() {
      if (signal?.aborted || !session.user || !session.csrf) throw new DOMException('Cancelled', 'AbortError');
      const body = servicePayload(controls);
      // 未改动分组时不提交，避免覆盖其它页面期间的移动，也不会提交已被删除的旧分组。
      if (item && (body.group_id ?? null) === (item.group_id ?? null)) delete body.group_id;
      await api(item ? `/services/${encodeURIComponent(item.id)}` : '/services', {method: item ? 'PATCH' : 'POST', body, signal});
      await saved();
    }
  });
}
const statusOf = (item) => item.enabled ? item.latest?.status || 'unknown' : 'disabled';
const statusBadge = (status) => h('span', {class: `badge ${status === 'up' ? 'online' : status === 'down' ? 'offline' : status}`}, t(`services.status.${status}`));
const badge = (item) => statusBadge(statusOf(item));
// 组头汇总只看启用的成员：有故障即故障，否则有未知即未知，否则正常；全部停用时显示停用。
export function groupStatus(members) {
  if (!members.length) return null;
  const statuses = members.filter((item) => item.enabled).map(statusOf);
  if (!statuses.length) return 'disabled';
  return statuses.includes('down') ? 'down' : statuses.includes('unknown') ? 'unknown' : 'up';
}
const errorCodes = new Set(['timeout', 'dns', 'refused', 'denied', 'unsupported', 'http_status', 'error']);
const errorLabel = (latest) => {
  if (!errorCodes.has(latest.error)) return latest.error;
  const text = t(`services.error.${latest.error}`);
  return latest.status_code ? `${text} (${latest.status_code})` : text;
};
export const permissionHint = (item) => item.latest?.error === 'permission_denied' ? h('p', {class: 'text-note'}, t('services.permissionHint')) : null;

export function serviceDetail(item, signal) {
  return modal(item.name, (_close, dialogSignal) => {
    const charts = h('div', {class: 'service-charts'});
    const status = h('p', {class: 'text-note', role: 'status'});
    let range = '24h', request;
    const ranges = ['1h', '24h', '7d', '30d'];
    const controls = ranges.map((value) => h('button', {type: 'button', 'aria-pressed': String(value === range), onclick: () => {
      range = value; controls.forEach((button, i) => button.setAttribute('aria-pressed', String(ranges[i] === range))); load();
    }}, t(`services.range.${value}`)));
    const retry = button(t('common.retry'), {small: true, onClick: () => load()}); retry.hidden = true;
    const stop = () => request?.abort(); signal?.addEventListener('abort', stop, {once: true});
    dialogSignal.addEventListener('abort', () => { stop(); signal?.removeEventListener('abort', stop); }, {once: true});
    async function load() {
      request?.abort(); if (dialogSignal.aborted || signal?.aborted) return;
      const current = request = new AbortController(); status.textContent = t('services.loading'); retry.hidden = true;
      try {
        const data = await api(`/services/${encodeURIComponent(item.id)}/history?range=${range}`, {signal: current.signal});
        if (current.signal.aborted || dialogSignal.aborted || signal?.aborted) return;
        if (!Array.isArray(data.buckets)) throw Object.assign(new Error('Invalid history'), {code: 'unreadable_response'});
        const latency = data.buckets.filter((p) => p.avg_latency_ms !== null).map((p) => ({t: p.time, avg: p.avg_latency_ms, max: p.max_latency_ms}));
        const availability = data.buckets.filter((p) => p.availability !== null).map((p) => ({t: p.time, avg: p.availability, max: p.availability}));
        charts.replaceChildren(...[[latency, 'ms', 'services.latency'], [availability, 'percent', 'services.availability']].filter(([points]) => points.length).map(([points, unit, label]) =>
          h('section', {class: 'history-metric'}, h('h3', null, t(label)), chart({title: t(label), unit, id: `service-${unit}`, series: [{label: t(label), stepSeconds: data.step_seconds, points}]}))));
        status.textContent = availability.length ? '' : t('services.noHistory');
      } catch (error) { if (!current.signal.aborted) { status.textContent = errorText(error); retry.hidden = false; } }
    }
    load();
    return h('div', {class: 'dialog-body stack'}, h('div', {class: 'head-actions'}, badge(item), h('span', {class: 'mono service-target'}, item.target)),
      permissionHint(item), item.latest?.error && item.latest.error !== 'permission_denied' ? h('p', {class: 'text-note'}, errorLabel(item.latest)) : null,
      h('div', {class: 'segmented', role: 'group', 'aria-label': t('services.historyRange')}, controls), status, retry, charts);
  });
}

const COLUMNS = ['name', 'type', 'target', 'status', 'latency', 'availability24h', 'visibility', 'actions'];
const STATUSES = ['', 'down', 'unknown', 'up', 'disabled'];
const COLLAPSED_KEY = 'orbitbeacon-service-groups-collapsed';

export function servicesPlaceholder() {
  return pagePlaceholder(h('div', {class: 'services-page'}, placeholderHead(t('services.title'), {status: false, actions: 2}),
    placeholderPanel(null, h('div', {class: 'toolbar services-toolbar', 'aria-hidden': 'true'},
      h('div', {class: 'segmented'}, h('span', {class: 'segment'}, t('nodes.all'))), h('div', {class: 'toolbar-end'}, h('span', {class: 'sk sk-field'}))),
    placeholderTable(COLUMNS.map((key) => t(`services.${key}`)), {rows: 4, className: 'services-table'}))));
}

// 折叠状态只是本机偏好，读写失败时全部展开。
function readCollapsed() {
  try {
    const stored = JSON.parse(localStorage.getItem(COLLAPSED_KEY));
    return new Set(Array.isArray(stored) ? stored.filter((key) => typeof key === 'string') : []);
  } catch { return new Set(); }
}

export async function servicesPage() {
  const signal = getNavigationSignal();
  const content = h('div');
  const alerts = h('div');
  let polling, request;
  let services = null;
  let groups = [];
  let groupsKnown = false;
  const filter = {q: '', status: ''};
  const collapsed = readCollapsed();
  const editor = (item) => serviceEditor(item, {signal, saved: load, groups: groupsKnown ? [...groups] : null, onGroupCreated: () => load()});
  const add = () => editor(null);
  const manage = button(t('nodes.manageGroups'), {icon: 'folder', onClick: () => manageGroupsDialog({kind: SERVICE_GROUPS, groups: () => groups,
    counts: () => { const sizes = new Map(); for (const item of services || []) if (item.group_id) sizes.set(item.group_id, (sizes.get(item.group_id) || 0) + 1); return sizes; },
    changed: () => load()})});
  const root = h('div', {class: 'services-page'}, pageHead(t('services.title'), manage, button(t('services.add'), {variant: 'primary', icon: 'plus', onClick: add})), alerts, content);

  const statusButtons = new Map(STATUSES.map((status) => [status, h('button', {type: 'button', onclick: () => { filter.status = status; render(); }},
    status ? t(`services.status.${status}`) : t('nodes.all'), h('span', {class: 'count'}))]));
  const search = input({type: 'search', id: 'service-search', placeholder: t('services.search'), 'aria-label': t('services.search'), autocomplete: 'off'});
  search.addEventListener('input', () => { filter.q = search.value; render(); });
  const toolbar = h('div', {class: 'toolbar services-toolbar'},
    h('div', {class: 'segmented', role: 'group', 'aria-label': t('services.filterStatus')}, [...statusButtons.values()]),
    h('div', {class: 'toolbar-end'}, h('div', {class: 'search'}, icon('search'), search)));
  const table = h('div');
  const listPanel = panel(null, {}, toolbar, table);

  async function remove(item) {
    if (!(await confirmDialog({title: t('services.deleteNamed', {name: item.name}), message: t('services.deleteHint'), confirm: t('services.delete'), danger: true})) || signal?.aborted) return;
    try { await api(`/services/${encodeURIComponent(item.id)}`, {method: 'DELETE', signal}); await load(); }
    catch (error) { if (!signal?.aborted) toastError(error); }
  }
  function toggle(key) {
    if (collapsed.has(key)) collapsed.delete(key); else collapsed.add(key);
    try { localStorage.setItem(COLLAPSED_KEY, JSON.stringify([...collapsed])); } catch {}
    render();
  }
  function row(item) {
    const detail = () => serviceDetail(item, signal);
    const name = button(item.name, {className: 'service-name', onClick: detail}); name.id = `service-${item.id}`;
    const edit = iconButton('edit', t('services.editNamed', {name: item.name}), () => editor(item)); edit.id = `service-edit-${item.id}`;
    const removeButton = iconButton('trash', t('services.deleteNamed', {name: item.name}), () => remove(item)); removeButton.id = `service-delete-${item.id}`;
    return h('tr', {onclick: (event) => { if (!event.target.closest('button,a')) detail(); }},
      h('td', {class: 'service-name-cell'}, name), h('td', {class: 'service-type'}, item.type.toUpperCase()), h('td', {class: 'service-target'}, item.target, permissionHint(item)),
      h('td', {class: 'service-status'}, badge(item)), h('td', {class: 'num service-latency', 'data-label': t('services.latency')}, formatMilliseconds(item.latest?.latency_ms)),
      h('td', {class: 'num service-availability', 'data-label': t('services.availability24h')}, formatPercent(item.availability_24h)),
      h('td', {class: 'service-visibility'}, [item.public ? t('services.public') : '', item.notify ? t('services.notify') : ''].filter(Boolean).join(' · ') || '—'),
      h('td', {class: 'actions'}, edit, removeButton));
  }
  // 筛选或搜索时各组强制展开，只列出有匹配项的组；否则空组也列出，便于看到刚建的组。
  function section(key, label, members, visible, filtering) {
    const open = filtering || !collapsed.has(key);
    const status = groupStatus(members);
    const control = h('button', {type: 'button', class: 'service-group-toggle', id: `service-group-${key}`, 'aria-expanded': String(open), disabled: filtering,
      onclick: () => toggle(key)}, icon(open ? 'down' : 'right'), h('span', {class: 'service-group-name'}, label), h('span', {class: 'count'}, String(members.length)));
    const head = h('tr', {class: 'service-group-head'}, h('th', {colspan: COLUMNS.length, scope: 'rowgroup'},
      h('div', {class: 'service-group-bar'}, control, status ? statusBadge(status) : null)));
    const rows = !open ? [] : visible.length ? visible.map(row) : [h('tr', {class: 'service-group-empty'}, h('td', {colspan: COLUMNS.length}, t('services.empty')))];
    return h('tbody', {class: 'service-group', dataset: {group: key}}, head, rows);
  }
  function render() {
    if (!services) return;
    const focusID = content.contains(document.activeElement) ? document.activeElement.id : null;
    // 已有分组时即使还没有监测也列出各组，刚建的组才看得见。
    if (!services.length && !groups.length) {
      content.replaceChildren(emptyState(t('services.empty'), null, button(t('services.add'), {variant: 'primary', onClick: add})));
      return;
    }
    const query = filter.q.trim().toLowerCase();
    const scoped = query ? services.filter((item) => `${item.name}\n${item.target}`.toLowerCase().includes(query)) : services;
    const counts = {'': scoped.length};
    for (const item of scoped) counts[statusOf(item)] = (counts[statusOf(item)] || 0) + 1;
    for (const [status, control] of statusButtons) {
      control.lastElementChild.textContent = String(counts[status] || 0);
      control.setAttribute('aria-pressed', String(filter.status === status));
      control.hidden = Boolean(status) && !counts[status] && filter.status !== status;
    }
    const matches = new Set(scoped.filter((item) => !filter.status || statusOf(item) === filter.status).map((item) => item.id));
    const filtering = Boolean(query || filter.status);
    const known = new Set(groups.map((group) => group.id));
    const sections = groups.map((group) => [group.id, group.name, services.filter((item) => item.group_id === group.id)]);
    const ungrouped = services.filter((item) => !known.has(item.group_id));
    if (ungrouped.length) sections.push(['none', t('nodes.ungrouped'), ungrouped]);
    const bodies = groups.length
      ? sections.map(([key, label, members]) => [key, label, members, members.filter((item) => matches.has(item.id))])
        .filter(([, , , visible]) => !filtering || visible.length).map(([key, label, members, visible]) => section(key, label, members, visible, filtering))
      : [h('tbody', null, services.filter((item) => matches.has(item.id)).map(row))];
    const empty = filtering && !matches.size ? h('p', {class: 'text-note services-no-match'}, t('services.noMatch')) : null;
    table.replaceChildren(...[dataTable({caption: t('services.title'), className: 'services-table', columns: COLUMNS.map((key) => t(`services.${key}`)), body: matches.size || !filtering ? bodies : []}), empty].filter(Boolean));
    if (content.firstElementChild !== listPanel) content.replaceChildren(listPanel);
    if (focusID) document.getElementById(focusID)?.focus({preventScroll: true});
  }
  async function load() {
    clearTimeout(polling); request?.abort(); if (signal?.aborted) return;
    const current = request = new AbortController();
    try {
      // 分组读取失败不影响监测列表，沿用上次的分组；从未读到时编辑框不提供分组字段。
      const [data, groupList] = await Promise.all([api('/services', {signal: current.signal}), serviceGroupsList().catch(() => null)]);
      if (current.signal.aborted || signal?.aborted) return;
      if (!Array.isArray(data.services)) throw Object.assign(new Error('Invalid services'), {code: 'unreadable_response'});
      services = data.services;
      if (groupList) { groups = groupList; groupsKnown = true; }
      alerts.replaceChildren();
      render();
    } catch (error) {
      if (!current.signal.aborted && !signal?.aborted) alerts.replaceChildren(h('div', {class: 'callout', role: 'alert'}, errorText(error), button(t('common.retry'), {onClick: load})));
    } finally { if (!current.signal.aborted && !signal?.aborted) polling = setTimeout(load, 10000); }
  }
  signal?.addEventListener('abort', () => { clearTimeout(polling); request?.abort(); }, {once: true});
  await load(); return root;
}
