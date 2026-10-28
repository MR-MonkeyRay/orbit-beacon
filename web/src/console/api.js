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

export function onSession(listener) { sessionListeners.add(listener); }

export function setSession(value) {
  session.csrf = value.csrf_token;
  session.user = value.user;
  session.expires = value.expires_at;
  for (const listener of sessionListeners) listener(session);
}

export function updateUser(patch) {
  session.user = {...session.user, ...patch};
  for (const listener of sessionListeners) listener(session);
}

export const isAdmin = () => Boolean(session.user && session.user.role === 'admin' && session.csrf);

const cache = new Map();
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
  const timeout = typeof AbortSignal.timeout === 'function' ? AbortSignal.timeout(15000) : null;
  const timeoutController = timeout ? null : new AbortController();
  let timer = null;
  if (timeoutController) timer = setTimeout(() => timeoutController.abort(new DOMException('Request timed out.', 'TimeoutError')), 15000);
  const signals = [callerSignal, navigation ? navigationSignal : null, timeout || timeoutController.signal].filter(Boolean);
  if (typeof AbortSignal.any === 'function') {
    return {signal: AbortSignal.any(signals), cleanup: () => { if (timer) clearTimeout(timer); }};
  }
  const controller = new AbortController();
  const abort = (event) => controller.abort(event.target.reason);
  for (const signal of signals) {
    if (signal.aborted) { controller.abort(signal.reason); break; }
    signal.addEventListener('abort', abort, {once: true});
  }
  return {signal: controller.signal, cleanup: () => {
    if (timer) clearTimeout(timer);
    for (const signal of signals) signal.removeEventListener('abort', abort);
  }};
}

function assertActive(signal) {
  if (!signal.aborted) return;
  if (signal.reason && signal.reason.name === 'TimeoutError') throw new Error('The request timed out after 15 seconds.');
  throw signal.reason || new DOMException('The request was cancelled.', 'AbortError');
}

// Writes carry the session CSRF token; the browser adds Origin. A 401 sends the
// browser to the sign-in page unless the caller handles it (noRedirect).
export async function api(path, options = {}) {
  const method = options.method || 'GET';
  const headers = {'Content-Type': 'application/json'};
  if (method !== 'GET') headers['X-CSRF-Token'] = session.csrf;
  const request = requestSignal(options.signal, options.navigation !== false);
  try {
    const response = await fetch(`/api/v1${path}`, {
      method, headers, credentials: 'same-origin', cache: 'no-store', signal: request.signal,
      body: options.body === undefined ? undefined : JSON.stringify(options.body)
    });
    assertActive(request.signal);
    if (response.status === 204) return null;
    let value = null;
    try { value = await response.json(); } catch { assertActive(request.signal); }
    assertActive(request.signal);
    if (!response.ok) {
      if (response.status === 401 && !options.noRedirect && location.pathname !== '/login') {
        const next = `${location.pathname}${location.search}${location.hash}`;
        location.assign(`/login?next=${encodeURIComponent(next)}`);
      }
      throw new ApiError(response.status, value);
    }
    if (value === null) throw new ApiError(response.status, {message: 'The server returned an unreadable response.'});
    return value;
  } catch (error) {
    assertActive(request.signal);
    throw error;
  } finally {
    request.cleanup();
  }
}

export async function signOut() {
  await api('/session', {method: 'DELETE'});
  setSession({csrf_token: '', user: null, expires_at: ''});
  location.assign('/login');
}

export async function ensureSession() {
  if (!session.user) setSession(await api('/me'));
  return session.user;
}

export function cached(key, ttl, fetcher) {
  const hit = cache.get(key);
  if (hit && Date.now() - hit.at < ttl) return hit.promise;
  const entry = {at: Date.now(), settled: false};
  entry.promise = fetcher().then((value) => { entry.settled = true; return value; },
    (error) => { if (cache.get(key) === entry) cache.delete(key); throw error; });
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
    for (let offset = 0; offset < 10000;) {
      const page = await api(`/nodes?limit=100&offset=${offset}`);
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
