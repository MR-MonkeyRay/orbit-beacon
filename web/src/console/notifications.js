import {errorText} from '../shared/errors.js';
import {t, getLocale} from '../shared/i18n.js';
import {h} from '../shared/dom.js';
import {api, ApiError, getSessionEpoch, onSession, pauseSessionVerification, session, sessionIsVerified, verifySession} from './api.js';
import {formatBytes, when} from './format.js';
import {button, facts} from './ui/components.js';
import {modal} from './ui/dialogs.js';
import {formatMetricValue, metricLabel, severityLabel} from './pages/alert-utils.js';
import {taskDetails} from './pages/backup-task.js';

const tickListeners = new Set();
const stateLabels = {queued: t('shell.queued'), running: t('shell.running'), succeeded: t('shell.succeeded'), failed: t('shell.failed'), firing: t('shell.firing'), resolved: t('shell.resolved')};
const categoryLabels = {configuration: t('shell.configuration'), monitoring: t('shell.monitoring'), events: t('shell.events')};
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

const visibleSession = () => pageActive && !document.hidden && Boolean(session.user && !session.user.must_change_password && session.csrf) && location.pathname !== '/login';
const foreground = () => visibleSession() && sessionIsVerified();
const requestOptions = (signal, extra = {}) => ({navigation: false, signal, ...extra});
const categories = (values) => (values || []).map((value) => categoryLabels[value] || value).join(', ');
const sourceName = (details) => details.destination?.plan_name || details.destination?.actor_name || (details.kind === 'backup_export' ? t('shell.manualExport') : t('shell.deletedPlan'));
const failureText = () => navigator.onLine === false ? t('shell.notificationOffline') : t('shell.notificationStale');

function renderEntries() {
  for (const entry of document.querySelectorAll('.notification-entry')) {
    entry.hidden = !session.user || session.user.must_change_password;
    const count = entry.querySelector('.notification-count');
    // The icon rail shows the count as a small badge, so large counts are capped.
    count.textContent = summary ? (summary.unread_count > 99 ? '99+' : new Intl.NumberFormat(getLocale()).format(summary.unread_count)) : stale ? '?' : '…';
    const status = stale ? (navigator.onLine === false ? t('shell.offline') : t('shell.stale')) : '';
    const freshness = entry.querySelector('.notification-freshness');
    if (freshness) freshness.textContent = status;
    entry.classList.toggle('stale', stale);
    const description = summary ? t('shell.unreadCount', {count: summary.unread_count}) : t('shell.unreadUnavailable');
    entry.setAttribute('aria-label', t(status ? 'shell.notificationEntryStatus' : 'shell.notificationEntry', {description, status}));
    entry.title = stale ? failureText() : t('shell.notificationEntry', {description});
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
    ? `${details.rule_name || t('shell.alert')} · ${stateLabels[notification.transition] || notification.transition}`
    : `${sourceName(details)} · ${stateLabels[details.state || notification.transition] || t('shell.unknownState')}`;
}

function snapshot(notification) {
  const details = notification.details || {};
  const rows = [[t('shell.received'), when(notification.created_at)], [t('shell.status'), stateLabels[notification.transition] || notification.transition]];
  if (notification.kind === 'alert') rows.push(
    [t('shell.rule'), details.rule_name || details.rule_id], [t('shell.node'), details.node_name || details.node_id],
    [t('shell.metric'), metricLabel(details.metric)], [t('shell.severity'), severityLabel(details.severity || notification.severity)],
    [t('shell.value'), formatMetricValue(details.metric, details.value)], [t('shell.started'), details.started_at ? when(details.started_at) : null],
    [t('shell.resolved'), details.resolved_at ? when(details.resolved_at) : null]);
  else rows.push(
    [t('shell.source'), sourceName(details)], [t('shell.categories'), categories(details.categories)], [t('shell.phase'), details.phase ? t(({snapshot: 'data.phase.snapshot', packaging: 'data.phase.packaging', storing: 'data.phase.storing'})[details.phase] || 'data.phase.starting') : null],
    [t('shell.file'), details.artifact_name], [t('shell.size'), details.artifact_bytes != null ? formatBytes(details.artifact_bytes) : null],
    ['SHA-256', details.artifact_sha256 ? h('span', {class: 'mono'}, details.artifact_sha256) : null],
    [details.kind === 'backup_remote' ? t('shell.objectKey') : t('shell.location'), details.result_location], [t('shell.failureCode'), details.error_code || null], [t('shell.warningCode'), details.warning_code || null]);
  return h('section', {class: 'notification-snapshot'}, h('h3', null, t('shell.notificationSnapshot')), facts(rows),
    details.warning_code === 'storage_outcome_unknown' ? h('p', {class: 'callout'}, t('shell.storageUnknown')) : null,
    details.warning_code === 'retention_cleanup_failed' ? h('p', {class: 'callout'}, t('shell.retentionWarning')) : null);
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
  const allButton = button(t('shell.all'), {onClick: () => changeFilter(false)});
  const unreadButton = button(t('shell.unread'), {onClick: () => changeFilter(true)});
  const markAllButton = button(t('shell.markAllRead'), {onClick: markAll});
  const refreshButton = button(t('shell.refresh'), {icon: 'refresh', onClick: () => { loadList(); refreshNotifications(); }});
  const moreButton = button(t('shell.loadMore'), {onClick: () => loadList(true)});
  const toolbar = h('div', {class: 'notification-toolbar'}, h('div', {class: 'notification-filters', 'aria-label': t('shell.notificationFilter')}, allButton, unreadButton), markAllButton, refreshButton);
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
        h('span', {class: 'notification-row-meta'}, h('span', {class: notification.read ? 'notification-read' : 'notification-unread'}, notification.read ? t('shell.read') : t('shell.unread')), when(notification.created_at)),
        notification.kind === 'alert' ? h('span', {class: 'notification-row-subject'}, notification.details?.node_name || notification.details?.node_id || t('shell.unknownNode')) : h('span', {class: 'notification-row-subject'}, categories(notification.details?.categories)));
    }
    if (!entries.length) list.append(h('p', {class: 'notification-empty'}, loaded ? (unread ? t('shell.noUnread') : t('shell.noNotifications')) : t('shell.notificationsLoading')));
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
      if (page.has_more && (!page.next_cursor || (append && page.next_cursor === nextCursor))) throw new ApiError(200, {code: 'unreadable_response'});
      entries = append ? [...entries, ...page.notifications.filter((item) => !entries.some((old) => old.id === item.id))] : page.notifications;
      nextCursor = page.next_cursor || null;
      hasMore = page.has_more;
      readThrough = page.read_through;
      loaded = true;
    } catch (error) {
      if (!alive() || listRequest !== controller || error.name === 'AbortError') return;
      showError(t('shell.notificationsLoadFailed', {error: errorText(error)}));
      if (!loaded) list.replaceChildren(h('p', {class: 'notification-empty'}, t('shell.notificationsUnavailable')));
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
      if (alive() && readAllRequest === controller && error.name !== 'AbortError') showError(t('shell.markReadFailed', {error: errorText(error)}));
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
    const back = button(t('shell.backNotifications'), {onClick: () => {
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
    const body = h('div', {class: 'notification-detail-content'}, h('p', {role: 'status'}, t('shell.notificationLoading')));
    detail.replaceChildren(back, body);
    back.focus();
    const current = () => alive() && selected === id && detailRequest === controller;
    try {
      const notification = await api(`/notifications/${encodeURIComponent(id)}`, requestOptions(controller.signal));
      if (!current()) return;
      const readStatus = h('p', {class: 'notification-read-status', role: 'status'}, notification.read ? t('shell.read') : t('shell.unread'));
      const source = h('section', {class: 'notification-source'}, h('p', null, t('shell.sourceLoading')));
      body.replaceChildren(h('h3', null, notificationTitle(notification)), readStatus, snapshot(notification), source);
      // The immutable snapshot is now rendered. Failed details never mark an item read.
      if (!notification.read) {
        try {
          await api(`/notifications/${encodeURIComponent(id)}/read`, requestOptions(controller.signal, {method: 'PATCH', body: {read: true}}));
          if (!current()) return;
          notification.read = true;
          const cached = entries.find((item) => item.id === id);
          if (cached) cached.read = true;
          readStatus.textContent = t('shell.read');
          refreshNotifications();
        } catch (error) {
          if (!current()) return;
          readStatus.textContent = t('shell.readSaveFailed', {error: errorText(error)});
        }
      }
      if (!current()) return;
      const task = notification.kind === 'task';
      try {
        const value = await api(`/${task ? 'tasks' : 'alerts'}/${encodeURIComponent(notification.source_id)}`, requestOptions(controller.signal));
        if (!current()) return;
        const href = task ? `/settings?task=${encodeURIComponent(notification.source_id)}#data` : `/alerts?event=${encodeURIComponent(notification.source_id)}#history`;
        const link = h('a', {class: 'btn', href, dataset: {link: ''}}, task ? t('shell.viewTask') : t('shell.viewAlert'));
        source.replaceChildren(h('h3', null, task ? t('shell.currentTask') : t('shell.alertDetails')), link);
        if (task) {
          component = taskDetails(value, {signal: controller.signal});
          source.append(component.element);
        }
      } catch (error) {
        if (!current()) return;
        source.replaceChildren(h('p', {class: 'notification-source-status'}, error.status === 404 || error.status === 410
          ? t(task ? 'shell.taskUnavailable' : 'shell.alertUnavailable')
          : t('shell.sourceLoadFailed', {error: errorText(error)})));
      }
    } catch (error) {
      if (!current() || error.name === 'AbortError') return;
      body.replaceChildren(h('p', {class: 'notification-error', role: 'alert'}, error.status === 404 ? t('shell.notificationMissing') : t('shell.notificationLoadFailed', {error: errorText(error)})));
    }
  }
  renderList();
  loadList();
}
