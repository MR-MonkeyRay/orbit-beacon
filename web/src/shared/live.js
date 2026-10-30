import {timestampMillis} from './freshness.js';
import {openSSE} from './sse.js';
import {responseErrorCode} from './http-error.js';

const FIRST_SNAPSHOT_MS = 8000;
const WATCHDOG_MS = 30000;
const PREFER_WS_MS = 300000;
const DEFAULT_INTERVAL_SECONDS = 10;
const knownTypes = new Set(['snapshot', 'heartbeat', 'close', 'error']);
const owns = (value, key) => Object.prototype.hasOwnProperty.call(value, key);
const discard = (response) => { if (response?.body && !response.body.locked) void response.body.cancel().catch(() => {}); };

function retryAfter(response) {
  if (![429, 503].includes(response.status)) return 0;
  const value = response.headers.get('Retry-After')?.trim();
  if (!value) return 0;
  if (/^\d+$/.test(value)) return Number(value) * 1000;
  const date = Date.parse(value);
  const serverDate = Date.parse(response.headers.get('Date'));
  return Number.isFinite(date) ? Math.max(0, date - (Number.isFinite(serverDate) ? serverDate : Date.now())) : 0;
}

// One read-only channel per consumer. Transport and retryAt are internal UI hints,
// not user-facing transport choices. retryAt is an epoch-ms HTTP countdown target.
export function connectLive({urls, signal, credentials = 'same-origin', validateSnapshot, onSnapshot, onState = () => {}, onClock = () => {}, onUnauthorized, onPublicPageDisabled}) {
  let generation = 0;
  let channel;
  let closed = false;
  let pageHidden = false;
  let intervalSeconds = DEFAULT_INTERVAL_SECONDS;
  let previousState;
  let httpNotBefore = 0;
  const timers = new Set();
  const available = () => !closed && !signal?.aborted && !pageHidden && !document.hidden;
  const current = (active) => channel === active && active.generation === generation && !active.controller.signal.aborted && available();
  const later = (callback, delay) => {
    const id = setTimeout(() => { timers.delete(id); callback(); }, delay);
    timers.add(id);
    return id;
  };
  const clear = (id) => { clearTimeout(id); timers.delete(id); };
  const state = (value, transport, retryAt = null, force = false) => {
    const next = {state: value, transport, intervalSeconds, retryAt};
    if (force || !previousState || Object.keys(next).some((key) => next[key] !== previousState[key])) {
      previousState = next;
      onState(next);
    }
  };
  const stop = () => {
    generation += 1;
    for (const timer of timers) clearTimeout(timer);
    timers.clear();
    const old = channel;
    channel = undefined;
    if (!old) return;
    old.controller.abort();
    old.socket?.close();
    old.detach?.();
  };
  const close = () => {
    if (closed) return;
    closed = true;
    stop();
    document.removeEventListener('visibilitychange', visibility);
    window.removeEventListener('pagehide', hide);
    window.removeEventListener('pageshow', show);
    signal?.removeEventListener('abort', close);
  };
  const unauthenticated = (active) => {
    if (!current(active)) return;
    state('stale', active.transport);
    close();
    // Known authentication failure is terminal even if the hook cannot reach /me.
    try { Promise.resolve(onUnauthorized?.('unauthenticated')).catch(() => {}); } catch { /* terminal */ }
  };
  const publicPageDisabled = (active) => {
    if (!current(active)) return;
    close();
    // Close/invalidate before application code can navigate or throw.
    try { Promise.resolve(onPublicPageDisabled?.()).catch(() => {}); } catch { /* terminal */ }
  };
  const fail = async (active) => {
    if (!current(active)) return;
    const {transport, opened} = active;
    stop();
    const token = generation;
    state('stale', transport);
    if (token !== generation || !available()) return;
    if (transport === 'ws' && !opened && onUnauthorized) {
      try {
        const allowed = await onUnauthorized('verify');
        if (token !== generation || !available()) return;
        if (allowed === false) { close(); return; }
      } catch {
        // An unknown network failure may fall back; a known 401 returns false.
        if (token !== generation || !available()) return;
      }
    }
    if (token === generation && available()) start(transport === 'ws' ? 'sse' : 'http');
  };
  const watchdog = (active) => {
    clear(active.watchdog);
    active.watchdog = later(() => { void fail(active); }, WATCHDOG_MS);
  };
  const control = (active, code) => {
    const transport = active.transport;
    stop();
    const token = generation;
    state('stale', transport);
    if (token !== generation || !available()) return;
    if (code === 'renew') start(transport);
    else {
      state('stale', transport, Date.now() + intervalSeconds * 1000);
      if (token !== generation || !available()) return;
      later(() => { if (token === generation && available()) start(transport); }, intervalSeconds * 1000);
    }
  };
  const message = (active, frame) => {
    if (!current(active)) return false;
    if (!frame || typeof frame !== 'object' || Array.isArray(frame) || typeof frame.type !== 'string') throw new Error('Invalid live frame');
    // Unknown future messages are ignored and never renew liveness.
    if (!knownTypes.has(frame.type)) {
      if (active.transport === 'http') throw new Error('Expected HTTP snapshot');
      return true;
    }
    if (!Number.isFinite(timestampMillis(frame.server_time)) || !frame.server_time.endsWith('Z')
      || !Number.isInteger(frame.update_interval_seconds) || frame.update_interval_seconds < 2 || frame.update_interval_seconds > 60) throw new Error('Invalid live metadata');
    if (frame.type === 'snapshot') {
      if (!owns(frame, 'data') || owns(frame, 'code') || validateSnapshot(frame.data) !== true) throw new Error('Invalid live snapshot');
    } else if (owns(frame, 'data') || (frame.type === 'heartbeat' ? owns(frame, 'code')
      : frame.type === 'close' ? !['renew', 'shutdown'].includes(frame.code) : !['unauthenticated', 'public_page_disabled'].includes(frame.code))) throw new Error('Invalid live control');
    if (active.transport === 'http' && frame.type === 'heartbeat') throw new Error('Expected HTTP snapshot');
    if (!current(active)) return false;
    intervalSeconds = frame.update_interval_seconds;
    onClock(frame.server_time);
    if (!current(active)) return false;
    if (frame.type === 'error') {
      if (frame.code === 'public_page_disabled') publicPageDisabled(active); else unauthenticated(active);
      return false;
    }
    if (frame.type === 'close') { control(active, frame.code); return false; }
    if (frame.type === 'snapshot') {
      active.received = true;
      clear(active.first);
      if (active.transport !== 'http') watchdog(active);
      onSnapshot(frame.data, frame);
      if (current(active)) state('live', active.transport);
    } else if (active.received) watchdog(active);
    return current(active);
  };
  const websocket = (active) => {
    try {
      const url = new URL(urls.ws, globalThis.location?.href);
      if (url.protocol === 'https:') url.protocol = 'wss:';
      if (url.protocol === 'http:') url.protocol = 'ws:';
      const socket = active.socket = new WebSocket(url.href);
      const opened = () => { if (current(active)) active.opened = true; };
      const received = (event) => {
        if (!current(active)) return;
        try {
          if (typeof event.data !== 'string') throw new Error('Invalid live encoding');
          message(active, JSON.parse(event.data));
        } catch { void fail(active); }
      };
      const broken = () => { void fail(active); };
      socket.addEventListener('open', opened);
      socket.addEventListener('message', received);
      socket.addEventListener('error', broken);
      socket.addEventListener('close', broken);
      active.detach = () => {
        socket.removeEventListener('open', opened);
        socket.removeEventListener('message', received);
        socket.removeEventListener('error', broken);
        socket.removeEventListener('close', broken);
      };
    } catch { void fail(active); }
  };
  const sse = async (active) => {
    try {
      await openSSE({url: urls.sse, credentials, signal: active.controller.signal, onFrame: (event) => {
        if (!current(active)) return false;
        if (event.event !== 'message') return true;
        return message(active, JSON.parse(event.data));
      }});
      if (current(active)) void fail(active);
    } catch (error) {
      if (!current(active)) return;
      if (error.response?.status === 401) unauthenticated(active);
      else if (error.response?.status === 403 && error.errorCode === 'public_page_disabled') publicPageDisabled(active);
      else void fail(active);
    }
  };
  const poll = (active) => {
    if (!current(active)) return;
    const remaining = httpNotBefore - performance.now();
    if (remaining > 0) {
      state(previousState?.state === 'live' ? 'live' : 'stale', 'http', Date.now() + remaining);
      if (current(active)) active.poll = later(() => poll(active), remaining);
      return;
    }
    const request = new AbortController();
    active.request = request;
    const cancel = () => request.abort();
    active.controller.signal.addEventListener('abort', cancel, {once: true});
    const valid = () => current(active) && active.request === request && !request.signal.aborted;
    const finish = (delay) => {
      clear(timeout);
      active.controller.signal.removeEventListener('abort', cancel);
      active.request = undefined;
      if (!current(active)) return;
      httpNotBefore = performance.now() + delay;
      const retryAt = Date.now() + delay;
      state(previousState?.state === 'live' ? 'live' : 'stale', 'http', retryAt);
      if (!current(active)) return;
      active.poll = later(() => poll(active), delay);
    };
    const timeout = later(() => {
      if (!valid()) return;
      request.abort();
      state('stale', 'http');
      finish(intervalSeconds * 1000);
    }, FIRST_SNAPSHOT_MS);
    void (async () => {
      let delay = intervalSeconds * 1000;
      try {
        const response = await fetch(urls.http, {credentials, cache: 'no-store', signal: request.signal});
        if (!valid()) { discard(response); return; }
        if (response.status === 401) { discard(response); unauthenticated(active); return; }
        if (response.status === 403) {
          const code = await responseErrorCode(response, request.signal);
          if (!valid()) return;
          if (code === 'public_page_disabled') { publicPageDisabled(active); return; }
        }
        delay = Math.max(delay, retryAfter(response));
        if (!response.ok) { discard(response); throw new Error('Live request failed'); }
        const frame = await response.json();
        if (!valid()) return;
        message(active, frame);
        delay = intervalSeconds * 1000;
      } catch {
        if (valid()) state('stale', 'http');
      } finally {
        // A timed-out request may resolve after a newer request has started.
        if (valid()) finish(delay);
      }
    })();
  };
  function start(transport = 'ws') {
    stop();
    if (!available()) return;
    const active = channel = {generation, transport, controller: new AbortController(), opened: false, received: false};
    state('connecting', transport, null, true);
    if (!current(active)) return;
    if (transport !== 'ws') later(() => { if (current(active)) start('ws'); }, PREFER_WS_MS);
    if (transport === 'http') poll(active);
    else {
      active.first = later(() => { void fail(active); }, FIRST_SNAPSHOT_MS);
      if (transport === 'ws') websocket(active); else void sse(active);
    }
  }
  const refresh = () => { if (!closed) start('ws'); };
  const pause = () => {
    const transport = channel?.transport || previousState?.transport || 'ws';
    stop();
    state('stale', transport, null, true);
  };
  const visibility = () => { if (document.hidden) pause(); else if (!pageHidden) refresh(); };
  const hide = () => { pageHidden = true; pause(); };
  const show = (event) => { if (event.persisted && pageHidden) { pageHidden = false; refresh(); } };
  document.addEventListener('visibilitychange', visibility);
  window.addEventListener('pagehide', hide);
  window.addEventListener('pageshow', show);
  signal?.addEventListener('abort', close, {once: true});
  if (signal?.aborted) close(); else if (!document.hidden) start();
  return {refresh, close};
}
