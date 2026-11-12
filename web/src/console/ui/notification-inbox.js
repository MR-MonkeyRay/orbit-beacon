import {errorText} from '../../shared/errors.js';
import {formatList, t, getLocale} from '../i18n.js';
import {h, icon} from '../../shared/dom.js';
import {formatBytes, when} from '../format.js';
import {formatMetricValue, metricLabel, severityLabel} from '../lib/alert-utils.js';
import {taskCategoryKey, taskStateKey} from '../lib/task-labels.js';
import {button, facts} from './components.js';
import {modal} from './dialogs.js';
import {nextCursor as pageCursor} from './pager.js';

const requestOptions = (signal, extra = {}) => ({navigation: false, signal, ...extra});

// 接口快照在规则或节点删除后仍保留其 ID。
// 同一规则、同一状态、距组内第一条不超过十分钟的告警通知合并；组位置取第一条出现处。
// 同一时刻多条规则会交替出现，因此不要求相邻。
export function groupNotifications(entries) {
  const groups = [];
  const open = new Map();
  for (const entry of entries) {
    const key = entry.kind === 'alert' && entry.details?.rule_id ? `${entry.details.rule_id}\n${entry.transition}` : null;
    const group = key && open.get(key);
    if (group && Math.abs(Date.parse(group[0].created_at) - Date.parse(entry.created_at)) <= 600000) { group.push(entry); continue; }
    const created = [entry];
    groups.push(created);
    if (key) open.set(key, created);
  }
  return groups;
}

export function createNotificationInbox({api, isActive, isStale, failureText, refreshSummary, openTaskDetails}) {
  let closed = false;
  let unread = false;
  let entries = [];
  let nextCursor = null;
  let hasMore = false;
  let readThrough = null;
  let listRequest;
  let detailRequest;
  let readAllRequest;
  const groupRequests = new Map();
  const expanded = new Set();
  let selected = null;
  let component;
  let loaded = false;
  let refreshPending = false;
  const alive = () => !closed && isActive();
  const stateLabels = {firing: t('notifications.firing'), resolved: t('notifications.resolved')};
  const stateLabel = (value) => stateLabels[value] || (taskStateKey(value) ? t(taskStateKey(value)) : undefined);
  const categories = (values) => formatList((values || []).map((value) => taskCategoryKey(value) ? t(taskCategoryKey(value)) : value));
  const sourceName = (details) => details.destination?.plan_name || details.destination?.actor_name || (details.kind === 'backup_export' ? t('notifications.manualExport') : t('notifications.deletedPlan'));
  const notificationTitle = (notification) => {
    const details = notification.details || {};
    if (notification.kind === 'service') return `${details.name || t('services.title')} · ${t(`services.event.${notification.transition}`)}`;
    return notification.kind === 'alert'
      ? t('notifications.alertSummary', {source: details.rule_name || t('notifications.alert'), state: stateLabel(notification.transition) || notification.transition})
      : t('notifications.taskSummary', {source: sourceName(details), state: stateLabel(details.state || notification.transition) || t('notifications.unknownState')});
  };
  const snapshot = (notification) => {
    const details = notification.details || {};
    const rows = [[t('notifications.received'), when(notification.created_at)], [t('notifications.status'), notification.kind === 'service' ? t(`services.event.${notification.transition}`) : stateLabel(notification.transition) || notification.transition]];
    if (notification.kind === 'service') rows.push([t('services.name'), details.name], [t('services.target'), details.target], [t('notifications.failureCode'), details.error || null], [t('services.latency'), formatMetricValue('probe_latency_ms', details.latency_ms)], [t('services.certificateExpiry'), details.tls_expires_at ? when(details.tls_expires_at) : null]);
    else if (notification.kind === 'alert') rows.push(
      [t('notifications.rule'), details.rule_name || details.rule_id], [t('notifications.node'), details.node_name || details.node_id],
      [t('notifications.metric'), metricLabel(details.metric)], [t('notifications.severity'), severityLabel(details.severity || notification.severity)],
      [t('notifications.value'), formatMetricValue(details.metric, details.value)], [t('notifications.started'), details.started_at ? when(details.started_at) : null],
      [t('notifications.resolved'), details.resolved_at ? when(details.resolved_at) : null]);
    else rows.push(
      [t('notifications.source'), sourceName(details)], [t('notifications.categories'), categories(details.categories)], [t('notifications.phase'), details.phase ? t(({snapshot: 'data.phase.snapshot', packaging: 'data.phase.packaging', storing: 'data.phase.storing'})[details.phase] || 'data.phase.starting') : null],
      [t('notifications.file'), details.artifact_name], [t('notifications.size'), details.artifact_bytes != null ? formatBytes(details.artifact_bytes) : null],
      ['SHA-256', details.artifact_sha256 ? h('span', {class: 'mono'}, details.artifact_sha256) : null],
      [details.kind === 'backup_remote' ? t('notifications.objectKey') : t('notifications.location'), details.result_location], [t('notifications.failureCode'), details.error_code || null], [t('notifications.warningCode'), details.warning_code || null]);
    return h('section', {class: 'notification-snapshot'}, h('h3', null, t('notifications.notificationSnapshot')), facts(rows),
      details.warning_code === 'storage_outcome_unknown' ? h('p', {class: 'callout'}, t('notifications.storageUnknown')) : null,
      details.warning_code === 'retention_cleanup_failed' ? h('p', {class: 'callout'}, t('notifications.retentionWarning')) : null);
  };
  const status = h('p', {class: 'status-message', role: 'status', hidden: !isStale()}, isStale() ? failureText() : '');
  const actionError = h('p', {class: 'status-message error', role: 'alert', hidden: true});
  const list = h('div', {class: 'notification-list'});
  const detail = h('div', {class: 'notification-detail', hidden: true});
  const allButton = h('button', {type: 'button', onclick: () => changeFilter(false)}, t('notifications.all'));
  const unreadButton = h('button', {type: 'button', onclick: () => changeFilter(true)}, t('notifications.unread'));
  const markAllButton = button(t('notifications.markAllRead'), {onClick: markAll});
  const refreshButton = button(t('common.refresh'), {icon: 'refresh', onClick: () => { loadList(); refreshSummary(); }});
  const moreButton = button(t('common.loadMore'), {onClick: () => loadList(true)});
  const toolbar = h('div', {class: 'notification-toolbar'}, h('div', {class: 'segmented', role: 'group', 'aria-label': t('notifications.notificationFilter')}, allButton, unreadButton), markAllButton, refreshButton);
  const content = h('div', {class: 'dialog-body notification-inbox'}, toolbar, status, actionError, list, moreButton, detail);
  const dialog = modal(t('shell.notifications'), () => content);
  content.parentElement.classList.add('notifications-dialog');
  function clearComponent() { component?.dispose(); component = null; }
  function stopRequests() {
    listRequest?.abort(); listRequest = null;
    detailRequest?.abort(); detailRequest = null;
    for (const controller of groupRequests.values()) controller.abort();
    groupRequests.clear();
    readAllRequest?.abort(); readAllRequest = null;
    markAllButton.disabled = false;
    clearComponent();
  }
  const handle = {
    close: () => { stopRequests(); dialog.close('session'); },
    pause: stopRequests,
    resume: () => { if (selected) showDetail(selected); else loadList(); },
    refresh: () => { if (selected || readAllRequest || groupRequests.size) refreshPending = true; else loadList(); },
    setStale: (value) => { status.hidden = !value; status.textContent = value ? failureText() : ''; }
  };
  handle.closed = dialog.closed;
  dialog.closed.then(() => { closed = true; stopRequests(); });

  function showError(message) { actionError.textContent = message; actionError.hidden = false; }
  function changeFilter(value) {
    if (unread === value && !selected) return;
    unread = value; entries = []; nextCursor = null; hasMore = false; readThrough = null; loaded = false; selected = null;
    for (const controller of groupRequests.values()) controller.abort();
    groupRequests.clear();
    detailRequest?.abort(); detailRequest = null; clearComponent(); detail.hidden = true; toolbar.hidden = false; list.hidden = false; loadList();
  }
  function renderList() {
    allButton.setAttribute('aria-pressed', String(!unread));
    unreadButton.setAttribute('aria-pressed', String(unread));
    markAllButton.disabled = !readThrough || Boolean(readAllRequest);
    moreButton.hidden = !hasMore || Boolean(selected);
    moreButton.disabled = Boolean(listRequest);
    const row = (notification) => {
      const control = button('', {className: 'notification-row', onClick: () => showDetail(notification.id)});
      control.append(h('span', {class: 'notification-row-title'}, notificationTitle(notification)),
        h('span', {class: 'notification-row-meta'}, h('span', {class: notification.read ? 'notification-read' : 'notification-unread'}, notification.read ? t('notifications.read') : t('notifications.unread')), when(notification.created_at)),
        notification.kind === 'service' ? h('span', {class: 'notification-row-subject'}, notification.details?.target) : notification.kind === 'alert' ? h('span', {class: 'notification-row-subject'}, notification.details?.node_name || notification.details?.node_id || t('notifications.unknownNode')) : h('span', {class: 'notification-row-subject'}, categories(notification.details?.categories)));
      return control;
    };
    list.replaceChildren(...groupNotifications(entries).map((group) => {
      if (group.length === 1) return row(group[0]);
      const first = group[0];
      const nodes = new Set(group.map((item) => item.details.node_id));
      const isRead = group.every((item) => item.read);
      const members = h('div', {id: `notification-group-${first.id}`, hidden: !expanded.has(first.id)}, group.map(row));
      const toggle = button('', {id: `notification-toggle-${first.id}`, className: 'notification-row', onClick: () => {
        members.hidden = !members.hidden;
        toggle.setAttribute('aria-expanded', String(!members.hidden));
        chevron.replaceChildren(icon(members.hidden ? 'right' : 'up'));
        if (members.hidden) expanded.delete(first.id);
        else { expanded.add(first.id); void markGroup(group, readState); }
      }});
      const chevron = h('span', {class: 'notification-expander', 'aria-hidden': 'true'}, icon(members.hidden ? 'right' : 'up'));
      const readState = h('span', {class: isRead ? 'notification-read' : 'notification-unread', role: 'status'}, t(isRead ? 'notifications.read' : 'notifications.unread'));
      toggle.setAttribute('aria-expanded', String(!members.hidden));
      toggle.setAttribute('aria-controls', members.id);
      toggle.append(h('span', {class: 'notification-row-title'}, chevron, `${notificationTitle(first)} · ${t('notifications.nodeCount', {count: nodes.size})}`),
        h('span', {class: 'notification-row-meta'}, readState, when(first.created_at)));
      const mark = button(t('notifications.markGroupRead'), {id: `notification-read-${first.id}`, small: true, disabled: isRead, onClick: () => markGroup(group, readState)});
      return h('section', {class: 'notification-group'}, toggle, mark, members);
    }));
    if (!entries.length) list.append(h('p', {class: 'empty'}, loaded ? (unread ? t('notifications.noUnread') : t('notifications.noNotifications')) : t('notifications.notificationsLoading')));
  }
  async function markGroup(group, status) {
    if (!alive() || groupRequests.has(group[0].id) || group.every((item) => item.read)) return;
    const controller = new AbortController();
    const id = group[0].id;
    groupRequests.set(id, controller);
    try {
      // A group is a presentation of individual notifications; persist every member's read flag.
      for (const item of group.filter((item) => !item.read)) {
        await api(`/notifications/${encodeURIComponent(item.id)}/read`, requestOptions(controller.signal, {method: 'PATCH', body: {read: true}}));
        if (!alive() || groupRequests.get(id) !== controller) return;
        item.read = true;
      }
      const focus = list.contains(document.activeElement) ? document.activeElement?.id : null;
      renderList();
      if (focus) {
        const target = document.getElementById(focus);
        (target?.disabled ? document.getElementById(`notification-toggle-${id}`) : target)?.focus();
      }
      refreshSummary();
    } catch (error) {
      if (alive() && groupRequests.get(id) === controller && error.name !== 'AbortError') {
        status.textContent = t('notifications.readSaveFailed', {error: errorText(error)});
        refreshSummary();
      }
    } finally {
      if (groupRequests.get(id) === controller) groupRequests.delete(id);
      if (alive() && !selected && !groupRequests.size && refreshPending) loadList();
    }
  }

  async function loadList(append = false) {
    if (!alive() || selected || append && (!hasMore || !nextCursor || listRequest)) return;
    listRequest?.abort();
    const controller = new AbortController();
    listRequest = controller; actionError.hidden = true; if (!append) refreshPending = false; renderList();
    const params = new URLSearchParams({limit: '50'});
    if (unread) params.set('unread', 'true');
    if (append) params.set('before', nextCursor);
    try {
      const page = await api(`/notifications?${params}`, requestOptions(controller.signal));
      if (!alive() || listRequest !== controller) return;
      const cursor = pageCursor(page, append ? nextCursor : null);
      entries = append ? [...entries, ...page.notifications.filter((item) => !entries.some((old) => old.id === item.id))] : page.notifications;
      nextCursor = cursor; hasMore = page.has_more; readThrough = page.read_through; loaded = true;
    } catch (error) {
      if (!alive() || listRequest !== controller || error.name === 'AbortError') return;
      showError(t('notifications.notificationsLoadFailed', {error: errorText(error)}));
      if (!loaded) list.replaceChildren(h('p', {class: 'empty'}, t('notifications.notificationsUnavailable')));
    } finally {
      if (listRequest === controller) { listRequest = null; if (loaded) renderList(); else { moreButton.hidden = true; markAllButton.disabled = true; } }
    }
  }
  async function markAll() {
    if (!alive() || !readThrough || readAllRequest) return;
    const controller = new AbortController();
    readAllRequest = controller; markAllButton.disabled = true; actionError.hidden = true;
    try {
      await api('/notifications/read-all', requestOptions(controller.signal, {method: 'POST', body: {through: readThrough}}));
      if (!alive() || readAllRequest !== controller) return;
      readAllRequest = null; await loadList(); refreshSummary();
    } catch (error) {
      if (alive() && readAllRequest === controller && error.name !== 'AbortError') showError(t('notifications.markReadFailed', {error: errorText(error)}));
    } finally { if (readAllRequest === controller) { readAllRequest = null; markAllButton.disabled = !readThrough; } }
  }
  async function showDetail(id) {
    if (!alive()) return;
    selected = id; listRequest?.abort(); listRequest = null; detailRequest?.abort(); clearComponent();
    const controller = new AbortController();
    detailRequest = controller; toolbar.hidden = true; list.hidden = true; moreButton.hidden = true; detail.hidden = false; actionError.hidden = true;
    const back = button(t('notifications.backNotifications'), {onClick: () => {
      detailRequest?.abort(); detailRequest = null; clearComponent(); selected = null; detail.hidden = true; toolbar.hidden = false; list.hidden = false; actionError.hidden = true;
      renderList(); if (unread || refreshPending) loadList(); allButton.focus();
    }});
    const body = h('div', {class: 'notification-detail-content'}, h('p', {role: 'status'}, t('notifications.notificationLoading')));
    detail.replaceChildren(back, body); back.focus();
    const current = () => alive() && selected === id && detailRequest === controller;
    try {
      const notification = await api(`/notifications/${encodeURIComponent(id)}`, requestOptions(controller.signal));
      if (!current()) return;
      const readStatus = h('p', {class: 'text-note', role: 'status'}, notification.read ? t('notifications.read') : t('notifications.unread'));
      const source = h('section', {class: 'notification-source'}, h('p', null, t('notifications.sourceLoading')));
      body.replaceChildren(h('h3', null, notificationTitle(notification)), readStatus, snapshot(notification), source);
      if (!notification.read) {
        try {
          await api(`/notifications/${encodeURIComponent(id)}/read`, requestOptions(controller.signal, {method: 'PATCH', body: {read: true}}));
          if (!current()) return;
          notification.read = true; const cached = entries.find((item) => item.id === id); if (cached) cached.read = true;
          readStatus.textContent = t('notifications.read'); refreshSummary();
        } catch (error) { if (!current()) return; readStatus.textContent = t('notifications.readSaveFailed', {error: errorText(error)}); }
      }
      if (!current()) return;
      if (notification.kind === 'service') {
        source.replaceChildren(h('a', {class: 'btn', href: '/services', dataset: {link: ''}}, t('services.title')));
        return;
      }
      const task = notification.kind === 'task';
      try {
        const value = await api(`/${task ? 'tasks' : 'alerts'}/${encodeURIComponent(notification.source_id)}`, requestOptions(controller.signal));
        if (!current()) return;
        const href = task ? `/settings?task=${encodeURIComponent(notification.source_id)}#data` : `/alerts?event=${encodeURIComponent(notification.source_id)}#history`;
        const link = h('a', {class: 'btn', href, dataset: {link: ''}}, task ? t('notifications.viewTask') : t('notifications.viewAlert'));
        source.replaceChildren(h('h3', null, task ? t('notifications.currentTask') : t('notifications.alertDetails')), link);
        if (task) { component = openTaskDetails(value, {signal: controller.signal}); source.append(component.element); }
      } catch (error) {
        if (!current()) return;
        source.replaceChildren(h('p', {class: 'text-note'}, error.status === 404 || error.status === 410
          ? t(task ? 'notifications.taskUnavailable' : 'notifications.alertUnavailable') : t('notifications.sourceLoadFailed', {error: errorText(error)})));
      }
    } catch (error) {
      if (!current() || error.name === 'AbortError') return;
      body.replaceChildren(h('p', {class: 'status-message error', role: 'alert'}, error.status === 404 ? t('notifications.notificationMissing') : t('notifications.notificationLoadFailed', {error: errorText(error)})));
    }
  }
  renderList(); loadList();
  return handle;
}
