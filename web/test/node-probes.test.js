import test from 'node:test';
import assert from 'node:assert/strict';
import {formatProbeLatency, formatProbeUptime, probeErrorText, probesPanel} from '../src/console/pages/node-probes.js';
import {setNavigationSignal} from '../src/console/api.js';
import {copy, copyPattern, elements, flush, installDOM} from './console-dom.js';

const probe = (values = {}) => ({target: 'tcp://db:5432', status: 'pending', latency_ms: null,
  error: '', status_code: null, checked_at: null, uptime_24h: null, checks_24h: 0, ...values});

function setup() {
  const dom = installDOM();
  const originalFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = (path, options) => new Promise((resolve) => requests.push({path, signal: options.signal, resolve}));
  const page = new AbortController();
  setNavigationSignal(page.signal);
  const root = probesPanel('node/id', page.signal);
  dom.body.append(root);
  dom.mutations();
  return {...dom, page, root, requests,
    respond: (request, body, status = 200) => request.resolve(new Response(JSON.stringify(body), {status})),
    cleanup: () => { page.abort(); setNavigationSignal(null); globalThis.fetch = originalFetch; dom.restore(); }};
}

test('probe errors describe the outcome', () => {
  const messages = {timeout: copy('nodes.probe.timeout'), refused: copy('nodes.probe.refused'), dns: copy('nodes.probe.dns'),
    tls: copy('nodes.probe.tls'), unreachable: copy('nodes.probe.unreachable'), unsupported: copy('nodes.probe.unsupported'), error: copy('nodes.probe.error')};
  for (const [kind, text] of Object.entries(messages)) assert.equal(probeErrorText(kind), text);
  assert.equal(probeErrorText(''), '');
  for (const code of [503, null, undefined, '500', 0, 600, 200.5]) assert.equal(probeErrorText('http_status', code), copy('nodes.probe.http'));
  assert.equal(probeErrorText('unexpected'), copy('nodes.probe.error'));
});

test('probe latency handles zero, decimals and unavailable values', () => {
  assert.equal(formatProbeLatency(12.5), copy('nodes.probe.latencyValue', {value: '12.5'}));
  assert.equal(formatProbeLatency(12.56), copy('nodes.probe.latencyValue', {value: '12.6'}));
  assert.equal(formatProbeLatency(0), copy('nodes.probe.latencyValue', {value: '0'}));
  for (const value of [null, undefined, '', '12.5', -1, NaN, Infinity]) assert.equal(formatProbeLatency(value), copy('common.unavailable'));
});

test('probe uptime includes check count and never treats absent checks as 0% uptime', () => {
  assert.equal(formatProbeUptime(99.5, 1440), copy('nodes.probe.checks', {percent: '99.5%', count: 1440}));
  assert.equal(formatProbeUptime(100, 1), copy('nodes.probe.checks', {percent: '100%', count: 1}));
  assert.equal(formatProbeUptime(0, 24), copy('nodes.probe.checks', {percent: '0%', count: 24}));
  assert.equal(formatProbeUptime(null, 0), copy('nodes.probe.checks', {percent: copy('common.unavailable'), count: 0}));
  assert.equal(formatProbeUptime(100, 0), copy('nodes.probe.checks', {percent: copy('common.unavailable'), count: 0}));
  for (const value of [null, undefined, '99', -1, 101, NaN, Infinity]) assert.equal(formatProbeUptime(value, 2), copy('nodes.probe.checks', {percent: copy('common.unavailable'), count: 2}));
});

test('probe pending rows explain the first-check requirement and render targets as text', async () => {
  const env = setup();
  try {
    assert.equal(env.requests.length, 1);
    assert.equal(env.requests[0].path, '/api/v1/nodes/node%2Fid/probes');
    const target = 'https://example.com/<img onerror=bad>';
    env.respond(env.requests[0], {probes: [probe({target})]});
    await flush();
    assert.match(env.root.textContent, copyPattern('common.status.pending'));
    assert.match(env.root.textContent, copyPattern('nodes.probe.notChecked'));
    assert.match(env.root.textContent, copyPattern('nodes.probe.checks', {percent: copy('common.unavailable'), count: 0}));
    assert.match(env.root.textContent, copyPattern('nodes.probe.pendingHint'));
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
    assert.match(rows[0].textContent, new RegExp(`${copyPattern('nodes.probe.up').source}.*${copyPattern('nodes.probe.latencyValue', {value: '12.5'}).source}.*${copyPattern('nodes.probe.checks', {percent: '99.5%', count: 1440}).source}`));
    assert.equal(rows[0].querySelector('time').getAttribute('datetime'), checked);
    assert.equal(rows[1].querySelector('.badge').classList.contains('offline'), true);
    assert.match(rows[1].textContent, new RegExp(`${copyPattern('nodes.probe.down').source}.*${copyPattern('nodes.probe.http').source}.*${copyPattern('nodes.probe.checks', {percent: '0%', count: 1}).source}`));
    assert.match(rows[1].textContent, /503/);
    assert.doesNotMatch(env.root.textContent, copyPattern('nodes.probe.pendingHint'));
    assert.equal(env.root.querySelector('.probes-body').getAttribute('aria-busy'), 'false');
  } finally { env.cleanup(); }
});


test('probe request failures render a working retry', async () => {
  const env = setup();
  try {
    env.respond(env.requests[0], {message: 'Service unavailable'}, 503);
    await flush();
    assert.ok(env.root.querySelector('.history-error'));
    env.root.querySelector('#probes-retry').click();
    assert.equal(env.requests.length, 2);
    env.respond(env.requests[1], {probes: []});
    await flush();
    assert.match(env.root.textContent, copyPattern('nodes.probe.empty'));
    assert.doesNotMatch(env.root.textContent, copyPattern('nodes.probe.loadError', {error: ''}));
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
    assert.match(env.root.textContent, copyPattern('nodes.probe.up'));
    assert.doesNotMatch(env.root.textContent, copyPattern('nodes.probe.empty'));
    env.root.remove();
    env.mutations();
    assert.equal(env.timers.size, 0);
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
    assert.doesNotMatch(env.root.textContent, new RegExp(`${copyPattern('nodes.probe.up').source}|${copyPattern('nodes.probe.loadError', {error: ''}).source}`));
    env.root.remove(); env.mutations(); env.body.append(env.root); env.mutations();
    assert.equal(env.timers.size, 0);
    assert.equal(env.requests.length, 1);
  } finally { env.cleanup(); }
});


test('probe certificate expiry appears only on probes with a certificate', async () => {
  const env = setup();
  try {
    const expiry = '2027-01-02T03:04:05Z';
    env.respond(env.requests[0], {probes: [probe({target: 'https://example.com', status: 'down', error: 'http_status', status_code: 302, tls_expires_at: expiry}), probe({target: 'dns://example.com'})]});
    await flush();
    const rows = env.root.querySelectorAll('.probe-row');
    assert.ok(rows[0].textContent.includes(copy('nodes.probe.tlsExpires')));
    assert.ok(rows[0].textContent.includes(`${copy('nodes.probe.http')} (302)`));
    assert.equal(rows[0].querySelector('time').getAttribute('datetime'), expiry);
    assert.ok(!rows[1].textContent.includes(copy('nodes.probe.tlsExpires')));
  } finally { env.cleanup(); }
});

test('probe list without certificate expiry has no expiry label', async () => {
  const env = setup();
  try {
    env.respond(env.requests[0], {probes: [probe(), probe({target: 'https://example.com', tls_expires_at: null})]});
    await flush();
    assert.ok(!env.root.textContent.includes(copy('nodes.probe.tlsExpires')));
  } finally { env.cleanup(); }
});
