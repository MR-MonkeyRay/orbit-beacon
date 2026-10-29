import {h} from '../shared/dom.js';
import {api, getSessionEpoch, onSession, pauseSessionVerification, session, sessionIsVerified, verifySession} from './api.js';
import {formatBytes, when} from './format.js';
import {button, facts} from './ui/components.js';
import {modal} from './ui/dialogs.js';
import {formatMetricValue, metricLabel, severityLabel} from './pages/alert-utils.js';
import {taskDetails} from './pages/backup-task.js';

const tickListeners = new Set();
const stateLabels = {queued: 'Queued', running: 'Running', succeeded: 'Succeeded', failed: 'Failed', firing: 'Firing', resolved: 'Resolved'};
const categoryLabels = {configuration: 'Configuration', monitoring: 'Monitoring', events: 'Events'};
let disposeNotifications;
let notificationSignal;
let identity = '';
let epoch = 0;
let timer;
let request;
let resumeRequest;
let refreshAgain = false;
let pageActive = true;
let summary = null;
let stale = false;
let inbox = null;

export function onNotificationTick(listener) {
  tickListeners.add(listener);
  return () => tickListeners.delete(listener);
}

const visibleSession = () => pageActive && !document.hidden && Boolean(session.user && session.csrf) && location.pathname !== '/login';
const foreground = () => visibleSession() && sessionIsVerified();
const requestOptions = (signal, extra = {}) => ({navigation: false, signal, ...extra});
const categories = (values) => (values || []).map((value) => categoryLabels[value] || value).join(', ');
const sourceName = (details) => details.destination?.plan_name || details.destination?.actor_name || (details.kind === 'backup_export' ? 'Manual export' : 'Deleted plan');
const failureText = () => navigator.onLine === false ? 'You are offline. Reconnect to refresh notifications.' : 'Notifications could not be refreshed. Previously loaded information may be out of date.';

function renderEntries() {
  for (const entry of document.querySelectorAll('.notification-entry')) {
    entry.hidden = !session.user;
    const count = entry.querySelector('.notification-count');
    // The icon rail shows the count as a small badge, so large counts are capped.
    count.textContent = summary ? (summary.unread_count > 99 ? '99+' : String(summary.unread_count)) : stale ? '?' : '…';
    const status = stale ? (navigator.onLine === false ? 'Offline' : 'Stale') : '';
    const freshness = entry.querySelector('.notification-freshness');
    if (freshness) freshness.textContent = status;
    entry.classList.toggle('stale', stale);
    const description = summary ? `${summary.unread_count} unread` : 'unread count unavailable';
    entry.setAttribute('aria-label', `Notifications, ${description}${status ? `, ${status.toLowerCase()}` : ''}`);
    entry.title = stale ? failureText() : `Notifications, ${description}`;
  }
}

function stopPolling() {
  epoch += 1;
  clearInterval(timer);
  timer = undefined;
  request?.abort();
  request = null;
  resumeRequest = undefined;
  refreshAgain = false;
  inbox?.pause();
}

export function syncNotifications() {
  if (!notificationSignal || notificationSignal.aborted) return;
  if (!foreground()) { stopPolling(); return; }
  if (!timer) {
    timer = setInterval(() => { if (foreground()) refreshNotifications(); else resumeNotifications(); }, 15000);
    inbox?.resume();
    refreshNotifications();
  }
}

function resumeNotifications() {
  if (!visibleSession() || resumeRequest) return;
  stale = true;
  renderEntries();
  inbox?.setStale(true);
  // Keep the same single 15s controller while a foreground auth check is offline.
  // The summary itself must wait for /me, but a failed check can be retried.
  if (!timer) timer = setInterval(() => { if (foreground()) refreshNotifications(); else resumeNotifications(); }, 15000);
  inbox?.pause();
  const currentEpoch = epoch;
  const pending = resumeRequest = verifySession({signal: notificationSignal});
  pending.then(() => {
    if (pending !== resumeRequest || epoch !== currentEpoch || !foreground()) return;
    inbox?.resume();
    refreshNotifications();
  }, () => {}).finally(() => { if (resumeRequest === pending) resumeRequest = undefined; }).catch(() => {});
}

export async function refreshNotifications() {
  if (!foreground()) return;
  if (request) { refreshAgain = true; return; }
  const currentEpoch = epoch;
  const controller = new AbortController();
  request = controller;
  try {
    const value = await api('/notifications/summary', requestOptions(controller.signal));
    if (request !== controller || epoch !== currentEpoch || !foreground()) return;
    const changed = summary && summary.latest_id !== value.latest_id;
    summary = value;
    stale = false;
    renderEntries();
    inbox?.setStale(false);
    if (changed) inbox?.refresh();
    for (const listener of tickListeners) listener({stale: false, summary: value});
  } catch (error) {
    if (request !== controller || epoch !== currentEpoch || !foreground() || error.name === 'AbortError') return;
    stale = true;
    renderEntries();
    inbox?.setStale(true);
    for (const listener of tickListeners) listener({stale: true, error, summary});
  } finally {
    if (request === controller) {
      request = null;
      if (refreshAgain) { refreshAgain = false; refreshNotifications(); }
    }
  }
}

export function initNotifications() {
  if (disposeNotifications) return disposeNotifications;
  const lifetime = new AbortController();
  notificationSignal = lifetime.signal;
  const listen = (target, type, listener) => target.addEventListener(type, listener, {signal: lifetime.signal});
  for (const entry of document.querySelectorAll('.notification-entry')) listen(entry, 'click', openInbox);
  const unsubscribe = onSession(() => {
    const next = session.user ? `${getSessionEpoch()}` : '';
    if (next !== identity) {
      stopPolling();
      inbox?.close();
      identity = next;
      summary = null;
      stale = false;
      renderEntries();
    }
    syncNotifications();
  });
  const resume = resumeNotifications;
  listen(document, 'visibilitychange', () => {
    if (document.hidden) { pauseSessionVerification(); stopPolling(); }
    else resume();
  });
  listen(window, 'pagehide', () => { pageActive = false; pauseSessionVerification(); stopPolling(); });
  listen(window, 'pageshow', () => { pageActive = true; resume(); });
  listen(window, 'online', () => { if (!sessionIsVerified()) resume(); else if (foreground()) refreshNotifications(); });
  listen(window, 'offline', () => { if (foreground()) { stale = true; renderEntries(); inbox?.setStale(true); } });
  pageActive = true;
  identity = session.user ? `${getSessionEpoch()}` : '';
  renderEntries();
  syncNotifications();
  const dispose = () => {
    lifetime.abort();
    unsubscribe();
    stopPolling();
    notificationSignal = undefined;
    inbox?.close();
    identity = '';
    summary = null;
    stale = false;
    if (disposeNotifications === dispose) disposeNotifications = undefined;
  };
  disposeNotifications = dispose;
  return dispose;
}

function notificationTitle(notification) {
  const details = notification.details || {};
  return notification.kind === 'alert'
    ? `${details.rule_name || 'Alert'} · ${stateLabels[notification.transition] || notification.transition}`
    : `${sourceName(details)} · ${stateLabels[details.state || notification.transition] || 'Unknown state'}`;
}

function snapshot(notification) {
  const details = notification.details || {};
  const rows = [['Received', when(notification.created_at)], ['Status', stateLabels[notification.transition] || notification.transition]];
  if (notification.kind === 'alert') rows.push(
    ['Rule', details.rule_name || details.rule_id], ['Node', details.node_name || details.node_id],
    ['Metric', metricLabel(details.metric)], ['Severity', severityLabel(details.severity || notification.severity)],
    ['Value', formatMetricValue(details.metric, details.value)], ['Started', details.started_at ? when(details.started_at) : null],
    ['Resolved', details.resolved_at ? when(details.resolved_at) : null]);
  else rows.push(
    ['Source', sourceName(details)], ['Categories', categories(details.categories)], ['Phase', details.phase || null],
    ['File', details.artifact_name], ['Size', details.artifact_bytes != null ? formatBytes(details.artifact_bytes) : null],
    ['SHA-256', details.artifact_sha256 ? h('span', {class: 'mono'}, details.artifact_sha256) : null],
    [details.kind === 'backup_remote' ? 'Object key' : 'Location', details.result_location], ['Failure code', details.error_code || null], ['Warning code', details.warning_code || null]);
  return h('section', {class: 'notification-snapshot'}, h('h3', null, 'Notification snapshot'), facts(rows),
    details.warning_code === 'storage_outcome_unknown' ? h('p', {class: 'callout'}, 'The storage outcome is unknown. A file or object may have been saved; this is not a confirmed success.') : null,
    details.warning_code === 'retention_cleanup_failed' ? h('p', {class: 'callout'}, 'The backup succeeded, but older local archives could not all be removed.') : null);
}

function openInbox() {
  if (inbox || !foreground()) return;
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
  const alive = () => !closed && foreground() && identity === openedIdentity;
  const openedIdentity = identity;
  const status = h('p', {class: 'notification-status', role: 'status', hidden: !stale}, stale ? failureText() : '');
  const actionError = h('p', {class: 'notification-error', role: 'alert', hidden: true});
  const list = h('div', {class: 'notification-list'});
  const detail = h('div', {class: 'notification-detail', hidden: true});
  const allButton = button('All', {onClick: () => changeFilter(false)});
  const unreadButton = button('Unread', {onClick: () => changeFilter(true)});
  const markAllButton = button('Mark all read', {onClick: markAll});
  const refreshButton = button('Refresh', {icon: 'refresh', onClick: () => { loadList(); refreshNotifications(); }});
  const moreButton = button('Load more', {onClick: () => loadList(true)});
  const toolbar = h('div', {class: 'notification-toolbar'}, h('div', {class: 'notification-filters', 'aria-label': 'Notification filter'}, allButton, unreadButton), markAllButton, refreshButton);
  const content = h('div', {class: 'dialog-body notification-inbox'}, toolbar, status, actionError, list, moreButton, detail);
  const dialog = modal('Notifications', () => content);
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
  inbox = handle;
  dialog.closed.then(() => {
    closed = true;
    stopRequests();
    if (inbox === handle) inbox = null;
  });

  function showError(message) { actionError.textContent = message; actionError.hidden = false; }
  function changeFilter(value) {
    if (unread === value && !selected) return;
    unread = value;
    entries = [];
    nextCursor = null;
    hasMore = false;
    readThrough = null;
    loaded = false;
    selected = null;
    detailRequest?.abort(); detailRequest = null;
    clearComponent();
    detail.hidden = true;
    toolbar.hidden = false;
    list.hidden = false;
    loadList();
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
      const row = list.children[index];
      row.append(h('span', {class: 'notification-row-title'}, notificationTitle(notification)),
        h('span', {class: 'notification-row-meta'}, h('span', {class: notification.read ? 'notification-read' : 'notification-unread'}, notification.read ? 'Read' : 'Unread'), when(notification.created_at)),
        notification.kind === 'alert' ? h('span', {class: 'notification-row-subject'}, notification.details?.node_name || notification.details?.node_id || 'Unknown node') : h('span', {class: 'notification-row-subject'}, categories(notification.details?.categories)));
    }
    if (!entries.length) list.append(h('p', {class: 'notification-empty'}, loaded ? (unread ? 'No unread notifications.' : 'No notifications yet.') : 'Loading notifications…'));
  }
  async function loadList(append = false) {
    if (!alive() || selected) return;
    if (append && (!hasMore || !nextCursor || listRequest)) return;
    listRequest?.abort();
    const controller = new AbortController();
    listRequest = controller;
    actionError.hidden = true;
    if (!append) { refreshPending = false; }
    renderList();
    const params = new URLSearchParams({limit: '50'});
    if (unread) params.set('unread', 'true');
    if (append) params.set('before', nextCursor);
    try {
      const page = await api(`/notifications?${params}`, requestOptions(controller.signal));
      if (!alive() || listRequest !== controller) return;
      if (page.has_more && (!page.next_cursor || (append && page.next_cursor === nextCursor))) throw new Error('Invalid notification cursor.');
      entries = append ? [...entries, ...page.notifications.filter((item) => !entries.some((old) => old.id === item.id))] : page.notifications;
      nextCursor = page.next_cursor || null;
      hasMore = page.has_more;
      readThrough = page.read_through;
      loaded = true;
    } catch (error) {
      if (!alive() || listRequest !== controller || error.name === 'AbortError') return;
      showError(navigator.onLine === false ? 'You are offline. Notifications have not been loaded.' : 'Could not load notifications. Use Refresh to try again.');
      if (!loaded) list.replaceChildren(h('p', {class: 'notification-empty'}, 'Notifications are unavailable.'));
    } finally {
      if (listRequest === controller) {
        listRequest = null;
        if (loaded) renderList();
        else { moreButton.hidden = true; markAllButton.disabled = true; }
      }
    }
  }
  async function markAll() {
    if (!alive() || !readThrough || readAllRequest) return;
    const controller = new AbortController();
    readAllRequest = controller;
    markAllButton.disabled = true;
    actionError.hidden = true;
    const through = readThrough;
    try {
      await api('/notifications/read-all', requestOptions(controller.signal, {method: 'POST', body: {through}}));
      if (!alive() || readAllRequest !== controller) return;
      readAllRequest = null;
      await loadList();
      refreshNotifications();
    } catch (error) {
      if (alive() && readAllRequest === controller && error.name !== 'AbortError') showError('Could not mark notifications read. No local read state was changed. Refresh and try again.');
    } finally {
      if (readAllRequest === controller) { readAllRequest = null; markAllButton.disabled = !readThrough; }
    }
  }
  async function showDetail(id) {
    if (!alive()) return;
    selected = id;
    listRequest?.abort(); listRequest = null;
    detailRequest?.abort();
    clearComponent();
    const controller = new AbortController();
    detailRequest = controller;
    toolbar.hidden = true;
    list.hidden = true;
    moreButton.hidden = true;
    detail.hidden = false;
    actionError.hidden = true;
    const back = button('Back to notifications', {onClick: () => {
      detailRequest?.abort(); detailRequest = null;
      clearComponent();
      selected = null;
      detail.hidden = true;
      toolbar.hidden = false;
      list.hidden = false;
      actionError.hidden = true;
      renderList();
      if (unread || refreshPending) loadList();
      allButton.focus();
    }});
    const body = h('div', {class: 'notification-detail-content'}, h('p', {role: 'status'}, 'Loading notification…'));
    detail.replaceChildren(back, body);
    back.focus();
    const current = () => alive() && selected === id && detailRequest === controller;
    try {
      const notification = await api(`/notifications/${encodeURIComponent(id)}`, requestOptions(controller.signal));
      if (!current()) return;
      const readStatus = h('p', {class: 'notification-read-status', role: 'status'}, notification.read ? 'Read' : 'Unread');
      const source = h('section', {class: 'notification-source'}, h('p', null, 'Loading current source…'));
      body.replaceChildren(h('h3', null, notificationTitle(notification)), readStatus, snapshot(notification), source);
      // The immutable snapshot is now rendered. Failed details never mark an item read.
      if (!notification.read) {
        try {
          await api(`/notifications/${encodeURIComponent(id)}/read`, requestOptions(controller.signal, {method: 'PATCH', body: {read: true}}));
          if (!current()) return;
          notification.read = true;
          const cached = entries.find((item) => item.id === id);
          if (cached) cached.read = true;
          readStatus.textContent = 'Read';
          refreshNotifications();
        } catch (error) {
          if (!current()) return;
          readStatus.textContent = 'Unread · Could not save read status. Reopen this notification to try again.';
        }
      }
      if (!current()) return;
      const task = notification.kind === 'task';
      try {
        const value = await api(`/${task ? 'tasks' : 'alerts'}/${encodeURIComponent(notification.source_id)}`, requestOptions(controller.signal));
        if (!current()) return;
        const href = task ? `/settings?task=${encodeURIComponent(notification.source_id)}#data` : `/alerts?event=${encodeURIComponent(notification.source_id)}#history`;
        const link = h('a', {class: 'btn', href, dataset: {link: ''}}, task ? 'View task' : 'View alert');
        source.replaceChildren(h('h3', null, task ? 'Current task' : 'Alert details'), link);
        if (task) {
          component = taskDetails(value, {signal: controller.signal});
          source.append(component.element);
        }
      } catch (error) {
        if (!current()) return;
        source.replaceChildren(h('p', {class: 'notification-source-status'}, error.status === 404 || error.status === 410
          ? `${task ? 'Task' : 'Alert'} details are no longer available. The notification snapshot above is retained.`
          : 'Current source details could not be loaded. The notification snapshot above remains available. Reopen the notification to try again.'));
      }
    } catch (error) {
      if (!current() || error.name === 'AbortError') return;
      body.replaceChildren(h('p', {class: 'notification-error', role: 'alert'}, error.status === 404 ? 'Notification not found. It may have expired.' : 'Could not load this notification. Its read status was not changed. Go back and try again.'));
    }
  }
  renderList();
  loadList();
}
