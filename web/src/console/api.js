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

// Writes carry the session CSRF token; the browser adds Origin. A 401 sends the
// browser to the sign-in page unless the caller handles it (noRedirect).
export async function api(path, options = {}) {
  const method = options.method || 'GET';
  const headers = {'Content-Type': 'application/json'};
  if (method !== 'GET') headers['X-CSRF-Token'] = session.csrf;
  const response = await fetch(`/api/v1${path}`, {
    method, headers, credentials: 'same-origin', cache: 'no-store', signal: options.signal,
    body: options.body === undefined ? undefined : JSON.stringify(options.body)
  });
  if (response.status === 204) return null;
  let value = null;
  try { value = await response.json(); } catch { value = null; }
  if (!response.ok) {
    if (response.status === 401 && !options.noRedirect && location.pathname !== '/login') location.assign('/login');
    throw new ApiError(response.status, value);
  }
  if (value === null) throw new ApiError(response.status, {message: 'The server returned an unreadable response.'});
  return value;
}

export async function signOut() {
  await api('/session', {method: 'DELETE'});
  session.csrf = '';
  location.assign('/login');
}

export async function ensureSession() {
  if (!session.user) setSession(await api('/me'));
  return session.user;
}

const cache = new Map();

export function cached(key, ttl, fetcher) {
  const hit = cache.get(key);
  if (hit && Date.now() - hit.at < ttl) return hit.promise;
  const promise = fetcher().catch((error) => { cache.delete(key); throw error; });
  cache.set(key, {at: Date.now(), promise});
  return promise;
}

export function invalidate(...keys) { for (const key of keys) cache.delete(key); }

// The console works with the whole inventory so search, filters and counts are
// instant; max_nodes keeps this bounded.
export function allNodes() {
  return cached('nodes', 15000, async () => {
    const nodes = [];
    for (let offset = 0; offset < 10000; offset += 100) {
      const page = await api(`/nodes?limit=100&offset=${offset}`);
      nodes.push(...page.nodes);
      if (page.nodes.length < 100) break;
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
