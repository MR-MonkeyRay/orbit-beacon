import {t, getLocale} from './i18n.js';
import {api, getSessionEpoch, onSession, pauseSessionVerification, session, sessionIsVerified, verifySession} from './api.js';
import {taskDetails} from './pages/backup-task.js';
import {createNotificationInbox} from './ui/notification-inbox.js';

const tickListeners = new Set();
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
let alertSummary = null;
let alertsStale = false;
let stale = false;
let inbox = null;

export function onNotificationTick(listener) {
  tickListeners.add(listener);
  return () => tickListeners.delete(listener);
}

const visibleSession = () => pageActive && !document.hidden && Boolean(session.user && !session.user.must_change_password && session.csrf) && location.pathname !== '/login';
const foreground = () => visibleSession() && sessionIsVerified();
const requestOptions = (signal, extra = {}) => ({navigation: false, signal, ...extra});
const failureText = () => navigator.onLine === false ? t('notifications.notificationOffline') : t('notifications.notificationStale');

function renderEntries() {
  for (const entry of document.querySelectorAll('.alert-entry')) {
    const total = alertSummary?.firing.total || 0;
    const badge = entry.querySelector('.alert-count');
    if (badge) {
      badge.hidden = !total || !session.user || session.user.must_change_password;
      badge.textContent = new Intl.NumberFormat(getLocale()).format(total);
      badge.classList.toggle('critical', Boolean(alertSummary?.firing.critical > 0));
    }
    const description = alertSummary ? t('shell.firingCount', {count: total}) : t('shell.alerts');
    entry.setAttribute('aria-label', alertsStale ? t('shell.alertEntryStale', {description}) : description);
    entry.classList.toggle('stale', alertsStale);
    entry.title = alertsStale ? failureText() : description;
  }
  for (const entry of document.querySelectorAll('.notification-entry')) {
    entry.hidden = !session.user || session.user.must_change_password;
    const count = entry.querySelector('.notification-count');
    // 图标栏以小徽章显示数量，大数量会截断。
    count.textContent = summary ? (summary.unread_count > 99 ? '99+' : new Intl.NumberFormat(getLocale()).format(summary.unread_count)) : stale ? '?' : '…';
    const status = stale ? (navigator.onLine === false ? t('common.status.offline') : t('notifications.stale')) : '';
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

function pollNotifications() {
  if (foreground()) refreshNotifications();
  else resumeNotifications();
}

export function syncNotifications() {
  if (!notificationSignal || notificationSignal.aborted) return;
  if (!foreground()) { stopPolling(); return; }
  if (!timer) {
    timer = setInterval(pollNotifications, 15000);
    inbox?.resume();
    refreshNotifications();
  }
}

function resumeNotifications() {
  if (!visibleSession() || resumeRequest) return;
  stale = true;
  alertsStale = true;
  renderEntries();
  inbox?.setStale(true);
  // 前台鉴权检查离线期间，保持同一个 15 秒控制器。
  // 摘要本身须等待 /me，但失败的检查可以重试。
  if (!timer) timer = setInterval(pollNotifications, 15000);
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
    const results = await Promise.allSettled([
      api('/notifications/summary', requestOptions(controller.signal)),
      api('/alerts/summary', requestOptions(controller.signal))
    ]);
    if (request !== controller || epoch !== currentEpoch || !foreground()) return;
    const [notifications, alerts] = results;
    if (alerts.status === 'fulfilled') { alertSummary = alerts.value; alertsStale = false; }
    else alertsStale = true;
    if (notifications.status === 'fulfilled') {
      const value = notifications.value;
      const changed = summary && (summary.latest_id !== value.latest_id || summary.unread_count !== value.unread_count);
      summary = value;
      stale = false;
      inbox?.setStale(false);
      if (changed) inbox?.refresh();
      for (const listener of tickListeners) listener({stale: false, summary: value});
    } else {
      stale = true;
      inbox?.setStale(true);
      for (const listener of tickListeners) listener({stale: true, error: notifications.reason, summary});
    }
    renderEntries();
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
      alertSummary = null;
      alertsStale = false;
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
  listen(window, 'offline', () => { if (foreground()) { stale = true; alertsStale = true; renderEntries(); inbox?.setStale(true); } });
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
    alertSummary = null;
    alertsStale = false;
    stale = false;
    if (disposeNotifications === dispose) disposeNotifications = undefined;
  };
  disposeNotifications = dispose;
  return dispose;
}

function openInbox() {
  if (inbox || !foreground()) return;
  const openedIdentity = identity;
  const handle = createNotificationInbox({
    api, isStale: () => stale, failureText, refreshSummary: refreshNotifications, openTaskDetails: taskDetails,
    isActive: () => foreground() && identity === openedIdentity
  });
  inbox = handle;
  handle.closed.then(() => { if (inbox === handle) inbox = null; });
}
