// Management API client, session state and a short-lived request cache.

export class ApiError extends Error {
  constructor(status, body) {
    super(body && body.message ? body.message : `Request failed with status ${status}`);
    this.status = status;
    this.code = body && body.code;
    this.fields = body && body.fields;
    this.requestID = body && body.request_id;
  }
}

export const session = {csrf: '', user: null, expires: ''};
const sessionListeners = new Set();
const cache = new Map();
const responseProofs = new WeakMap();
let sessionEpoch = 0;
let sessionController = new AbortController();
let expiryTimer;
let serverAt = NaN;
let monotonicAt = 0;
let rejectedProof = '';
let verification;
let authReady = true;

const monotonicNow = () => performance.now();
const proofKey = (value) => value.user ? `${value.user.id}:${value.csrf_token}:${value.expires_at}:${value.user.must_change_password}` : '';
const currentProof = () => proofKey({user: session.user, csrf_token: session.csrf, expires_at: session.expires});
const serverNow = () => serverAt + Math.max(0, monotonicNow() - monotonicAt);
const cancelled = () => new DOMException('The session has changed.', 'AbortError');
const notifySession = (detail = {}) => {
  for (const listener of sessionListeners) listener(session, {epoch: sessionEpoch, ...detail});
};

export function onSession(listener) { sessionListeners.add(listener); return () => sessionListeners.delete(listener); }
export const getSessionEpoch = () => sessionEpoch;
export const getSessionSignal = () => sessionController.signal;
export const sessionIsVerified = () => authReady;

function advanceSession() {
  sessionEpoch += 1;
  sessionController.abort(cancelled());
  sessionController = new AbortController();
  cache.clear();
  clearTimeout(expiryTimer);
  expiryTimer = undefined;
  verification?.controller.abort(cancelled());
  verification = undefined;
}

export function invalidateSession(reason = 'unauthenticated') {
  const previousUser = session.user;
  if (previousUser) rejectedProof = currentProof();
  advanceSession();
  session.csrf = '';
  session.user = null;
  session.expires = '';
  authReady = false;
  notifySession({boundary: true, previousUser, reason});
}

export function requirePasswordChange() {
  if (!session.user || session.user.must_change_password) return;
  const previousUser = session.user;
  advanceSession();
  session.user = {...session.user, must_change_password: true};
  scheduleExpiry();
  notifySession({boundary: true, previousUser, reason: 'password-change-required'});
}

function scheduleExpiry() {
  clearTimeout(expiryTimer);
  expiryTimer = undefined;
  const expires = Date.parse(session.expires);
  if (!session.user || !Number.isFinite(expires) || !Number.isFinite(serverAt)) return;
  const remaining = expires - serverNow();
  if (remaining <= 0) { invalidateSession('expired'); return; }
  const epoch = sessionEpoch;
  expiryTimer = setTimeout(() => { if (epoch === sessionEpoch) scheduleExpiry(); }, Math.min(2147483647, Math.ceil(remaining)));
}

// Server timestamps only move this clock forwards. HTTP Date is rounded down to
// seconds; its upper bound plus the measured request duration errs on privacy.
export function calibrateSessionClock(serverTime) {
  const value = Date.parse(serverTime);
  if (!Number.isFinite(value)) return;
  const current = serverNow();
  serverAt = Number.isFinite(current) ? Math.max(current, value) : value;
  monotonicAt = monotonicNow();
  scheduleExpiry();
}

export function setSession(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new ApiError(200, {message: 'The server returned an invalid session.'});
  const previousUser = session.user;
  const metadata = responseProofs.get(value);
  if (metadata && metadata.epoch !== sessionEpoch) throw cancelled();
  if (metadata?.callerSignal?.aborted || metadata?.pageSignal?.aborted) throw cancelled();
  if ((metadata && !value.user) || (value.user && (typeof value.user.id !== 'string' || typeof value.user.username !== 'string'
    || typeof value.user.must_change_password !== 'boolean'
    || metadata && (typeof value.csrf_token !== 'string' || !value.csrf_token || !Number.isFinite(Date.parse(value.expires_at)))))) {
    throw new ApiError(200, {message: 'The server returned an invalid session.'});
  }
  // Only a new login can restore management access after this credential was
  // restricted. A delayed or inconsistent /me must never reopen private data.
  if (!metadata?.login && session.user?.must_change_password && value.user?.id === session.user.id && !value.user.must_change_password) {
    value = {...value, user: {...value.user, must_change_password: true}};
  }
  const responseTime = metadata?.date == null ? undefined : metadata.date + Math.max(0, monotonicNow() - metadata.at);
  const nextProof = proofKey(value);
  if (value.user && nextProof === rejectedProof && metadata && !metadata.login) {
    if (session.user) invalidateSession('expired');
    throw new ApiError(401, {code: 'unauthenticated', message: 'Sign in again to continue.'});
  }
  const boundary = currentProof() !== nextProof || Boolean(metadata?.login);
  if (boundary) {
    advanceSession();
    serverAt = responseTime ?? Date.now();
    monotonicAt = monotonicNow();
  }
  if (metadata?.login) rejectedProof = '';
  session.csrf = value.csrf_token;
  session.user = value.user;
  session.expires = value.expires_at;
  authReady = Boolean(value.user);
  if (responseTime != null) calibrateSessionClock(new Date(responseTime).toISOString());
  else scheduleExpiry();
  if (session.user !== value.user) return;
  notifySession({boundary, previousUser, reason: metadata?.login ? 'login'
    : value.user?.must_change_password ? 'password-change-required' : 'session'});
}

export function updateUser(patch) {
  if (!session.user) return;
  session.user = {...session.user, ...patch};
  notifySession();
}

// Visibility gates notifications and live subscriptions together. Retained DOM
// stays stale while /me is unavailable, but the known expiry timer keeps running.
export function pauseSessionVerification() {
  verification?.controller.abort();
  verification = undefined;
  if (!authReady) return;
  authReady = false;
  notifySession({reason: 'verification'});
}
let navigationSignal = null;

// Capture the current page lifetime before awaiting data or building lazy tabs.
export const getNavigationSignal = () => navigationSignal;

// Requests still pending were tied to the previous page's signal and are about
// to be aborted; drop them so the next page fetches afresh. Settled results stay.
export function setNavigationSignal(signal) {
  navigationSignal = signal;
  for (const [key, entry] of cache) if (!entry.settled) cache.delete(key);
}

function requestSignal(callerSignal, navigation = true) {
  const controller = new AbortController();
  const signals = [callerSignal, navigation ? navigationSignal : null, sessionController.signal].filter(Boolean);
  const timer = setTimeout(() => { cleanup(); controller.abort(new DOMException('Request timed out.', 'TimeoutError')); }, 15000);
  const cleanup = () => {
    clearTimeout(timer);
    for (const signal of signals) signal.removeEventListener('abort', abort);
  };
  const abort = (event) => { cleanup(); controller.abort(event.target.reason); };
  for (const signal of signals) {
    if (signal.aborted) { cleanup(); controller.abort(signal.reason); break; }
    signal.addEventListener('abort', abort, {once: true});
  }
  return {signal: controller.signal, cleanup};
}

function assertActive(signal) {
  if (!signal.aborted) return;
  if (signal.reason && signal.reason.name === 'TimeoutError') throw new Error('The request timed out after 15 seconds.');
  throw signal.reason || new DOMException('The request was cancelled.', 'AbortError');
}

// Writes carry the current CSRF proof. Management401s end the whole private
// lifetime; rejected credentials on the public login form remain form errors.
export async function api(path, options = {}) {
  if (session.user) {
    scheduleExpiry();
    if (!session.user) throw new ApiError(401, {code: 'unauthenticated', message: 'Sign in again to continue.'});
  }
  const method = options.method || 'GET';
  const started = monotonicNow();
  const epoch = sessionEpoch;
  const callerSignal = options.signal;
  const pageSignal = options.navigation === false ? null : navigationSignal;
  const headers = {'Content-Type': 'application/json'};
  if (method !== 'GET') headers['X-CSRF-Token'] = session.csrf;
  const request = requestSignal(options.signal, options.navigation !== false);
  try {
    assertActive(request.signal);
    const response = await fetch(`/api/v1${path}`, {
      method, headers, credentials: 'same-origin', cache: 'no-store', signal: request.signal,
      body: options.body === undefined ? undefined : JSON.stringify(options.body)
    });
    if (request.signal.aborted && response.body && !response.body.locked) void response.body.cancel().catch(() => {});
    assertActive(request.signal);
    if (response.status === 401 && !(path === '/session' && method === 'POST')) {
      if (response.body && !response.body.locked) void response.body.cancel().catch(() => {});
      invalidateSession('unauthenticated');
      throw new ApiError(401, {code: 'unauthenticated', message: 'Sign in again to continue.'});
    }
    if (response.status === 204) return null;
    let value = null;
    try { value = await response.json(); } catch { assertActive(request.signal); }
    assertActive(request.signal);
    if (!response.ok) {
      if (response.status === 403 && value?.code === 'password_change_required') requirePasswordChange();
      throw new ApiError(response.status, value);
    }
    if (value === null) throw new ApiError(response.status, {message: 'The server returned an unreadable response.'});
    if ((path === '/me' || (path === '/session' && method === 'POST')) && typeof value === 'object') {
      const date = Date.parse(response.headers.get('Date'));
      responseProofs.set(value, {date: Number.isFinite(date) ? date + 999 + Math.max(0, monotonicNow() - started) : undefined,
        at: monotonicNow(), epoch, callerSignal, pageSignal, login: path === '/session'});
    }
    return value;
  } catch (error) {
    if (error instanceof ApiError) throw error;
    assertActive(request.signal);
    throw error;
  } finally {
    request.cleanup();
  }
}

export async function signOut() {
  // Revoke with the current CSRF proof, but never leave private data visible
  // while that request waits for the network. Only this revoke owns its timer.
  const headers = {'Content-Type': 'application/json', 'X-CSRF-Token': session.csrf};
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 15000);
  let epoch;
  try {
    let request;
    try {
      request = fetch('/api/v1/session', {method: 'DELETE', headers, credentials: 'same-origin', cache: 'no-store', signal: controller.signal});
    } finally { invalidateSession('logout'); epoch = sessionEpoch; }
    const response = await request;
    if (epoch !== sessionEpoch) return;
    if (!response.ok && response.status !== 401) throw new ApiError(response.status, null);
  } catch (error) {
    if (epoch === sessionEpoch && error.name !== 'AbortError') throw error;
  } finally { clearTimeout(timer); }
}

export async function revalidateSession({signal} = {}) {
  const epoch = sessionEpoch;
  const value = await api('/me', {signal, noRedirect: true, navigation: false});
  if (epoch !== sessionEpoch || signal?.aborted) throw cancelled();
  setSession(value);
  if (!session.user) throw new ApiError(401, {code: 'unauthenticated', message: 'Sign in again to continue.'});
  return session.user;
}

// Multiple foreground consumers share one auth gate, not one live channel.
export function verifySession({signal, fresh = false} = {}) {
  if (signal?.aborted) return Promise.reject(signal.reason || cancelled());
  if (fresh && verification) {
    verification.controller.abort(cancelled());
    verification = undefined;
  }
  if (!verification || verification.controller.signal.aborted || verification.epoch !== sessionEpoch) {
    const controller = new AbortController();
    const abort = () => controller.abort(signal?.reason || cancelled());
    signal?.addEventListener('abort', abort, {once: true});
    const pending = revalidateSession({signal: controller.signal});
    const entry = verification = {promise: pending, controller, epoch: sessionEpoch};
    pending.finally(() => {
      signal?.removeEventListener('abort', abort);
      if (verification === entry) verification = undefined;
    }).catch(() => {});
  }
  return verification.promise;
}

export async function ensureSession() {
  scheduleExpiry();
  if (!session.user || !authReady) await verifySession({signal: navigationSignal});
  return session.user;
}

export function cached(key, ttl, fetcher) {
  const epoch = sessionEpoch;
  scheduleExpiry();
  if (epoch !== sessionEpoch) return Promise.reject(cancelled());
  const hit = cache.get(key);
  if (hit && Date.now() - hit.at < ttl) return hit.promise;
  const entry = {at: Date.now(), settled: false};
  entry.promise = fetcher().then((value) => {
    if (epoch !== sessionEpoch) throw cancelled();
    entry.settled = true;
    return value;
  }).catch((error) => { if (cache.get(key) === entry) cache.delete(key); throw error; });
  cache.set(key, entry);
  return entry.promise;
}

export function invalidate(...keys) { for (const key of keys) cache.delete(key); }

// A continuation must move forward, or a bad envelope would loop forever.
export function nextOffset(page, offset) {
  const next = page.next_offset;
  if (!Number.isSafeInteger(next) || next <= offset) throw new Error('The server returned an invalid page offset.');
  return next;
}

// The console works with the whole inventory so search, filters and counts are
// instant; max_nodes keeps this bounded.
export function allNodes() {
  return cached('nodes', 15000, async () => {
    const nodes = [];
    const epoch = sessionEpoch;
    const signal = navigationSignal;
    for (let offset = 0; offset < 10000;) {
      const page = await api(`/nodes?limit=100&offset=${offset}`);
      if (epoch !== sessionEpoch || signal?.aborted) throw cancelled();
      nodes.push(...page.nodes);
      if (!page.has_more) break;
      offset = nextOffset(page, offset);
    }
    return nodes;
  });
}

export const groupsList = () => cached('groups', 30000, () => api('/groups').then((result) => result.groups));
export const usersList = () => cached('users', 30000, () => api('/users').then((result) => result.users));

export function nameLookup(nodes, users, groups) {
  const names = new Map();
  for (const node of nodes) names.set(node.id, node.name);
  for (const user of users) names.set(user.id, user.username);
  for (const group of groups) names.set(group.id, group.name);
  return names;
}
