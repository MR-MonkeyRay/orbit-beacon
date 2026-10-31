import {errorText} from '../../shared/errors.js';
import {formatList, t, getLocale} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {formatBytes, when} from '../format.js';
import {formatMetricValue, metricLabel, severityLabel} from '../lib/alert-utils.js';
import {taskCategoryKey, taskStateKey} from '../lib/task-labels.js';
import {button, facts} from './components.js';
import {modal} from './dialogs.js';
import {nextCursor as pageCursor} from './pager.js';

const requestOptions = (signal, extra = {}) => ({navigation: false, signal, ...extra});

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
    return notification.kind === 'alert'
      ? t('notifications.alertSummary', {source: details.rule_name || t('notifications.alert'), state: stateLabel(notification.transition) || notification.transition})
      : t('notifications.taskSummary', {source: sourceName(details), state: stateLabel(details.state || notification.transition) || t('notifications.unknownState')});
  };
  const snapshot = (notification) => {
    const details = notification.details || {};
    const rows = [[t('notifications.received'), when(notification.created_at)], [t('notifications.status'), stateLabel(notification.transition) || notification.transition]];
    if (notification.kind === 'alert') rows.push(
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
  const status = h('p', {class: 'notification-status', role: 'status', hidden: !isStale()}, isStale() ? failureText() : '');
  const actionError = h('p', {class: 'notification-error', role: 'alert', hidden: true});
  const list = h('div', {class: 'notification-list'});
  const detail = h('div', {class: 'notification-detail', hidden: true});
  const allButton = button(t('notifications.all'), {onClick: () => changeFilter(false)});
  const unreadButton = button(t('notifications.unread'), {onClick: () => changeFilter(true)});
  const markAllButton = button(t('notifications.markAllRead'), {onClick: markAll});
  const refreshButton = button(t('common.refresh'), {icon: 'refresh', onClick: () => { loadList(); refreshSummary(); }});
  const moreButton = button(t('common.loadMore'), {onClick: () => loadList(true)});
  const toolbar = h('div', {class: 'notification-toolbar'}, h('div', {class: 'notification-filters', 'aria-label': t('notifications.notificationFilter')}, allButton, unreadButton), markAllButton, refreshButton);
  const content = h('div', {class: 'dialog-body notification-inbox'}, toolbar, status, actionError, list, moreButton, detail);
  const dialog = modal(t('shell.notifications'), () => content);
  content.closest('dialog').classList.add('notifications-dialog');
  function clearComponent() { component?.dispose(); component = null; }
  function stopRequests() {
    listRequest?.abort(); listRequest = null;
    detailRequest?.abort(); detailRequest = null;
    readAllRequest?.abort(); readAllRequest = null;
    markAllButton.disabled = false;
    clearComponent();
  }
  const handle = {
    close: () => { stopRequests(); dialog.close('session'); },
    pause: stopRequests,
    resume: () => { if (selected) showDetail(selected); else loadList(); },
    refresh: () => { if (selected || readAllRequest) refreshPending = true; else loadList(); },
    setStale: (value) => { status.hidden = !value; status.textContent = value ? failureText() : ''; }
  };
  handle.closed = dialog.closed;
  dialog.closed.then(() => { closed = true; stopRequests(); });

  function showError(message) { actionError.textContent = message; actionError.hidden = false; }
  function changeFilter(value) {
    if (unread === value && !selected) return;
    unread = value; entries = []; nextCursor = null; hasMore = false; readThrough = null; loaded = false; selected = null;
    detailRequest?.abort(); detailRequest = null; clearComponent(); detail.hidden = true; toolbar.hidden = false; list.hidden = false; loadList();
  }
  function renderList() {
    allButton.setAttribute('aria-pressed', String(!unread));
    unreadButton.setAttribute('aria-pressed', String(unread));
    markAllButton.disabled = !readThrough || Boolean(readAllRequest);
    moreButton.hidden = !hasMore || Boolean(selected);
    moreButton.disabled = Boolean(listRequest);
    list.replaceChildren(...entries.map((notification) => button('', {className: 'notification-row', onClick: () => showDetail(notification.id)})));
    for (let index = 0; index < entries.length; index += 1) {
      const notification = entries[index];
      list.children[index].append(h('span', {class: 'notification-row-title'}, notificationTitle(notification)),
        h('span', {class: 'notification-row-meta'}, h('span', {class: notification.read ? 'notification-read' : 'notification-unread'}, notification.read ? t('notifications.read') : t('notifications.unread')), when(notification.created_at)),
        notification.kind === 'alert' ? h('span', {class: 'notification-row-subject'}, notification.details?.node_name || notification.details?.node_id || t('notifications.unknownNode')) : h('span', {class: 'notification-row-subject'}, categories(notification.details?.categories)));
    }
    if (!entries.length) list.append(h('p', {class: 'notification-empty'}, loaded ? (unread ? t('notifications.noUnread') : t('notifications.noNotifications')) : t('notifications.notificationsLoading')));
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
      if (!loaded) list.replaceChildren(h('p', {class: 'notification-empty'}, t('notifications.notificationsUnavailable')));
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
      const readStatus = h('p', {class: 'notification-read-status', role: 'status'}, notification.read ? t('notifications.read') : t('notifications.unread'));
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
        source.replaceChildren(h('p', {class: 'notification-source-status'}, error.status === 404 || error.status === 410
          ? t(task ? 'notifications.taskUnavailable' : 'notifications.alertUnavailable') : t('notifications.sourceLoadFailed', {error: errorText(error)})));
      }
    } catch (error) {
      if (!current() || error.name === 'AbortError') return;
      body.replaceChildren(h('p', {class: 'notification-error', role: 'alert'}, error.status === 404 ? t('notifications.notificationMissing') : t('notifications.notificationLoadFailed', {error: errorText(error)})));
    }
  }
  renderList(); loadList();
  return handle;
}
