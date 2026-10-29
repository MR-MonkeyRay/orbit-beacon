import {timestampMillis} from '../shared/freshness.js';
import {connectLive} from '../shared/live.js';
import {api, ApiError, calibrateSessionClock, getNavigationSignal, getSessionEpoch, getSessionSignal, invalidateSession, pauseSessionVerification, session, verifySession} from './api.js';

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const record = (value) => value !== null && typeof value === 'object' && !Array.isArray(value);
const time = (value) => Number.isFinite(timestampMillis(value));
const stringList = (value, max = Infinity) => Array.isArray(value) && value.length <= max && value.every((item) => typeof item === 'string');
const number = (value) => typeof value === 'number' && Number.isFinite(value) && value >= 0;
const nullableNumber = (value) => value === null || number(value);
const status = (value) => record(value) && uuid.test(value.id) && ['pending', 'online', 'offline', 'unknown', 'disabled'].includes(value.status)
  && ['pending', 'never', 'online', 'offline', 'disabled'].includes(value.presence)
  && (value.last_seen === null || time(value.last_seen)) && typeof value.agent_version === 'string';
const runtime = (value) => value === null || record(value) && Number.isInteger(value.report_interval_seconds)
  && value.report_interval_seconds >= 5 && value.report_interval_seconds <= 60
  && Number.isInteger(value.disk_interval_seconds) && value.disk_interval_seconds >= 60 && value.disk_interval_seconds <= 3600
  && stringList(value.interfaces, 8) && stringList(value.disk_paths, 8) && stringList(value.probe_targets, 5);
const node = (value) => status(value) && ['name', 'notes', 'region', 'provider'].every((key) => typeof value[key] === 'string')
  && (value.group_id === null || typeof value.group_id === 'string' && uuid.test(value.group_id)) && stringList(value.tags) && time(value.created_at);
const metrics = (value) => {
  if (!record(value) || !record(value.metric_status) || !record(value.sources)) return false;
  for (const key of ['cpu_percent', 'load1', 'load5', 'load15', 'memory_total_bytes', 'memory_available_bytes', 'swap_total_bytes', 'swap_used_bytes', 'uptime_seconds']) {
    if (key in value && !nullableNumber(value[key])) return false;
  }
  if (value.cpu_percent != null && value.cpu_percent > 100) return false;
  if (!Object.values(value.metric_status).every((item) => ['ok', 'stale', 'unsupported', 'permission_denied', 'disabled', 'error'].includes(item))) return false;
  if (!Object.values(value.sources).every((source) => record(source) && number(source.age_ms) && number(source.source_interval_ms) && source.source_interval_ms > 0)) return false;
  if (value.disks != null && (!Array.isArray(value.disks) || !value.disks.every((disk) => record(disk) && typeof disk.path === 'string'
    && ['total_bytes', 'used_bytes'].every((key) => nullableNumber(disk[key])) && time(disk.sampled_at)))) return false;
  return value.networks == null || Array.isArray(value.networks) && value.networks.every((network) => record(network) && typeof network.name === 'string'
    && nullableNumber(network.rx_bytes_per_sec) && nullableNumber(network.tx_bytes_per_sec));
};
const latest = (value) => value === null || record(value) && time(value.received_at) && time(value.sampled_at)
  && Number.isInteger(value.seq) && value.seq > 0 && (value.system === null || record(value.system)) && metrics(value.metrics)
  && record(value.runtime) && runtime(value.runtime);
function validSnapshot(data, nodeID) {
  if (!record(data)) return false;
  if (!nodeID) return Array.isArray(data.nodes) && data.nodes.length <= 10000 && data.nodes.every(status)
    && new Set(data.nodes.map((item) => item.id)).size === data.nodes.length;
  if (data.node === null) return data.latest === null && data.runtime === null;
  return node(data.node) && data.node.id === nodeID && latest(data.latest) && runtime(data.runtime);
}

// Explicit metadata reloads confirm membership with a causally later projection.
// The caller pauses its old channel so delayed frames cannot supersede this read.
export async function readConsoleFleet(signal) {
  const epoch = getSessionEpoch();
  const frame = await api('/live', {signal});
  if (!record(frame) || frame.type !== 'snapshot' || !time(frame.server_time)
    || !Number.isInteger(frame.update_interval_seconds) || frame.update_interval_seconds < 2 || frame.update_interval_seconds > 60
    || !validSnapshot(frame.data)) throw new ApiError(200, {code: 'unreadable_response'});
  calibrateSessionClock(frame.server_time);
  if (epoch !== getSessionEpoch() || signal?.aborted) throw new DOMException('The session has changed.', 'AbortError');
  return frame.data;
}

// The inner controller owns transport fallback. This wrapper owns private auth
// and the page lifetime: it is removed on hide, so its auto-resume can never race
// ahead of the unconditional foreground /me gate.
export function connectConsoleLive({nodeID, signal = getNavigationSignal(), onSnapshot, onState = () => {}, onClock = () => {}}) {
  if (nodeID !== undefined && (typeof nodeID !== 'string' || !uuid.test(nodeID))) throw new Error('Invalid node ID');
  const epoch = getSessionEpoch();
  const sessionSignal = getSessionSignal();
  let inner;
  let gate;
  let retry;
  let generation = 0;
  let closed = false;
  let pageHidden = false;
  let seen = false;
  let state = {state: 'connecting', transport: 'ws', intervalSeconds: 10, retryAt: null};
  const active = () => !closed && !signal?.aborted && !sessionSignal.aborted && epoch === getSessionEpoch() && Boolean(session.user && !session.user.must_change_password);
  const visible = () => active() && !document.hidden && !pageHidden;
  const emit = (next) => { state = next; if (active()) onState(next); };
  const stop = () => {
    generation += 1;
    clearTimeout(retry); retry = undefined;
    gate?.abort(); gate = undefined;
    inner?.close(); inner = undefined;
  };
  const stale = () => emit({...state, state: 'stale', retryAt: null});
  const close = () => {
    if (closed) return;
    closed = true;
    stop();
    document.removeEventListener('visibilitychange', visibility);
    window.removeEventListener('pagehide', hide);
    window.removeEventListener('pageshow', show);
    window.removeEventListener('online', online);
    signal?.removeEventListener('abort', close);
    sessionSignal.removeEventListener('abort', close);
  };
  const unauthorized = async (reason) => {
    if (!active()) return false;
    if (reason === 'unauthenticated') { invalidateSession('unauthenticated'); return false; }
    // WebSocket deliberately hides handshake status. Never infer that an
    // unopened socket is only a transport failure; verify its cookie via HTTP.
    try { await verifySession({signal, fresh: true}); return active(); }
    catch (error) { return error.status === 401 || !active() ? false : true; }
  };
  const start = async () => {
    stop();
    if (!visible()) return;
    emit({...state, state: seen ? 'stale' : 'connecting', retryAt: null});
    const token = generation;
    if (!visible() || token !== generation) return;
    const controller = gate = new AbortController();
    try {
      await verifySession({signal: controller.signal});
      if (!visible() || token !== generation || controller.signal.aborted) return;
      gate = undefined;
      const query = nodeID ? `?node_id=${encodeURIComponent(nodeID)}` : '';
      inner = connectLive({
        urls: {ws: `/api/v1/live/ws${query}`, sse: `/api/v1/live/stream${query}`, http: `/api/v1/live${query}`},
        signal: sessionSignal, credentials: 'same-origin', validateSnapshot: (data) => validSnapshot(data, nodeID),
        onSnapshot(data, frame) {
          if (!visible() || token !== generation) return;
          seen = true;
          onSnapshot(data, frame);
        },
        onClock(serverTime) {
          if (!visible() || token !== generation) return;
          calibrateSessionClock(serverTime);
          if (active()) onClock(serverTime);
        },
        onState(next) { if (visible() && token === generation) emit(next); },
        onUnauthorized: unauthorized
      });
    } catch (error) {
      if (!visible() || token !== generation) return;
      stale();
      if (error.status !== 401) retry = setTimeout(start, state.intervalSeconds * 1000);
    }
  };
  const hide = () => { pageHidden = true; pauseSessionVerification(); stop(); stale(); };
  const visibility = () => {
    if (document.hidden) { pauseSessionVerification(); stop(); stale(); }
    else if (!pageHidden) void start();
  };
  const show = () => { pageHidden = false; if (!document.hidden) void start(); };
  const online = () => { if (visible() && !inner) void start(); };
  document.addEventListener('visibilitychange', visibility);
  window.addEventListener('pagehide', hide);
  window.addEventListener('pageshow', show);
  window.addEventListener('online', online);
  signal?.addEventListener('abort', close, {once: true});
  sessionSignal.addEventListener('abort', close, {once: true});
  if (!active()) close(); else if (!document.hidden) void start();
  return {refresh: () => { if (visible()) void start(); }, close};
}
