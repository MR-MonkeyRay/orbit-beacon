import test from 'node:test';
import assert from 'node:assert/strict';
import {formatProbeLatency, formatProbeUptime, probeErrorText, probesPanel} from '../src/console/pages/node-probes.js';
import {setNavigationSignal} from '../src/console/api.js';
import {elements, flush, installDOM} from './console-dom.js';

const probe = (values = {}) => ({target: 'tcp://db:5432', status: 'pending', latency_ms: null,
  error: '', status_code: null, checked_at: null, uptime_24h: null, checks_24h: 0, ...values});

function setup(onConfigure) {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = (path, options) => new Promise((resolve) => requests.push({path, signal: options.signal, resolve}));
  const page = new AbortController();
  setNavigationSignal(page.signal);
  const root = probesPanel('node/id', page.signal, onConfigure);
  dom.body.append(root);
  dom.mutations();
  return {...dom, page, root, requests,
    respond: (request, body, status = 200) => request.resolve(new Response(JSON.stringify(body), {status})),
    cleanup: () => { page.abort(); setNavigationSignal(null); globalThis.fetch = originalFetch; dom.restore(); }};
}

test('probe error kinds have human-readable text and safe HTTP status fallbacks', () => {
  const messages = {timeout: 'Timed out', refused: 'Connection refused', dns: 'DNS lookup failed',
    tls: 'TLS handshake failed', unreachable: 'Network unreachable', unsupported: 'Not supported on this agent', error: 'Probe failed'};
  for (const [kind, text] of Object.entries(messages)) assert.equal(probeErrorText(kind), text);
  assert.equal(probeErrorText(''), '');
  assert.equal(probeErrorText('http_status', 503), 'HTTP 503');
  for (const code of [null, undefined, '500', 0, 600, 200.5]) assert.equal(probeErrorText('http_status', code), 'HTTP request failed');
  assert.equal(probeErrorText('unexpected'), 'Probe failed');
});

test('probe latency handles zero, decimals and unavailable values', () => {
  assert.equal(formatProbeLatency(12.5), '12.5 ms');
  assert.equal(formatProbeLatency(12.56), '12.6 ms');
  assert.equal(formatProbeLatency(0), '0 ms');
  for (const value of [null, undefined, '', '12.5', -1, NaN, Infinity]) assert.equal(formatProbeLatency(value), 'Unavailable');
});

test('probe uptime includes check count and never treats absent checks as 0% uptime', () => {
  assert.equal(formatProbeUptime(99.5, 1440), '99.5% · 1440 checks');
  assert.equal(formatProbeUptime(100, 1), '100% · 1 check');
  assert.equal(formatProbeUptime(0, 24), '0% · 24 checks');
  assert.equal(formatProbeUptime(null, 0), 'Unavailable · 0 checks');
  assert.equal(formatProbeUptime(100, 0), 'Unavailable · 0 checks');
  for (const value of [null, undefined, '99', -1, 101, NaN, Infinity]) assert.equal(formatProbeUptime(value, 2), 'Unavailable · 2 checks');
});

test('probe pending rows explain the first-check requirement and render targets as text', async () => {
  const env = setup();
  try {
    assert.equal(env.requests.length, 1);
    assert.equal(env.requests[0].path, '/api/v1/nodes/node%2Fid/probes');
    const target = 'https://example.com/<img onerror=bad>';
    env.respond(env.requests[0], {probes: [probe({target})]});
    await flush();
    assert.match(env.root.textContent, /Pending/);
    assert.match(env.root.textContent, /Not checked yet/);
    assert.match(env.root.textContent, /Unavailable · 0 checks/);
    assert.match(env.root.textContent, /Results appear after the agent runs its first check \(requires an agent version with probe support\)\./);
    assert.equal(env.root.querySelector('.probe-target').textContent, target);
    assert.equal(env.root.querySelectorAll('a').length, 0);
    assert.equal(env.root.querySelectorAll('img').length, 0);
    assert.ok(elements(env.root).every((element) => !element.hasAttribute('style')));
  } finally { env.cleanup(); }
});

test('probe up and down rows retain server order, badges, latency, error, timestamp and uptime', async () => {
  const env = setup();
  try {
    const checked = '2026-10-27T12:00:00Z';
    env.respond(env.requests[0], {probes: [
      probe({status: 'up', latency_ms: 12.5, checked_at: checked, uptime_24h: 99.5, checks_24h: 1440}),
      probe({target: 'https://example.com', status: 'down', latency_ms: 20, error: 'http_status', status_code: 503,
        checked_at: checked, uptime_24h: 0, checks_24h: 1}), probe({target: 'icmp://router'})]});
    await flush();
    const rows = env.root.querySelectorAll('.probe-row');
    assert.equal(rows.length, 3);
    assert.equal(rows[0].querySelector('.probe-target').textContent, 'tcp://db:5432');
    assert.equal(rows[0].querySelector('.badge').classList.contains('online'), true);
    assert.match(rows[0].textContent, /Up.*12.5 ms.*99.5% · 1440 checks/);
    assert.equal(rows[0].querySelector('time').getAttribute('datetime'), checked);
    assert.equal(rows[1].querySelector('.badge').classList.contains('offline'), true);
    assert.match(rows[1].textContent, /Down.*HTTP 503.*0% · 1 check/);
    assert.doesNotMatch(env.root.textContent, /Results appear after/);
    assert.equal(env.root.querySelector('.probes-body').getAttribute('aria-busy'), 'false');
  } finally { env.cleanup(); }
});

test('empty probe panel offers configuration only when an admin action is provided', async () => {
  for (const admin of [false, true]) {
    let configured = false;
    const env = setup(admin ? () => { configured = true; } : null);
    try {
      env.respond(env.requests[0], {probes: []});
      await flush();
      assert.match(env.root.textContent, /No service probes configured\./);
      assert.doesNotMatch(env.root.textContent, /Results appear after/);
      const action = env.root.querySelector('button');
      assert.equal(Boolean(action), admin);
      if (action) { action.click(); assert.equal(configured, true); }
    } finally { env.cleanup(); }
  }
});

test('probe request failures render a working retry', async () => {
  const env = setup();
  try {
    env.respond(env.requests[0], {message: 'Service unavailable'}, 503);
    await flush();
    assert.match(env.root.textContent, /Could not load service probes: Service unavailable/);
    env.root.querySelector('#probes-retry').click();
    assert.equal(env.requests.length, 2);
    env.respond(env.requests[1], {probes: []});
    await flush();
    assert.match(env.root.textContent, /No service probes configured\./);
    assert.doesNotMatch(env.root.textContent, /Could not load/);
  } finally { env.cleanup(); }
});

test('probe polling skips hidden pages, pauses off-tab and resumes on reattachment', async () => {
  const env = setup();
  try {
    assert.equal(env.timers.size, 1);
    const tick = [...env.timers.values()][0];
    env.document.hidden = true;
    tick();
    assert.equal(env.requests.length, 1);
    env.document.hidden = false;
    tick();
    assert.equal(env.requests.length, 2);
    assert.equal(env.requests[0].signal.aborted, true);
    env.respond(env.requests[1], {probes: [probe({status: 'up'})]});
    await flush();
    env.respond(env.requests[0], {probes: []});
    await flush();
    assert.match(env.root.textContent, /Up/);
    assert.doesNotMatch(env.root.textContent, /No service probes/);
    env.root.remove();
    env.mutations();
    assert.equal(env.timers.size, 0);
    assert.equal(env.requests[1].signal.aborted, true);
    env.body.append(env.root);
    env.mutations();
    assert.equal(env.timers.size, 1);
    assert.equal(env.requests.length, 3);
  } finally { env.cleanup(); }
});

test('navigation cancels probe requests and permanently clears polling', async () => {
  const env = setup();
  try {
    env.page.abort();
    assert.equal(env.timers.size, 0);
    assert.equal(env.requests[0].signal.aborted, true);
    env.respond(env.requests[0], {probes: [probe({status: 'up'})]});
    await flush();
    assert.doesNotMatch(env.root.textContent, /Up|Could not load/);
    env.root.remove(); env.mutations(); env.body.append(env.root); env.mutations();
    assert.equal(env.timers.size, 0);
    assert.equal(env.requests.length, 1);
  } finally { env.cleanup(); }
});
