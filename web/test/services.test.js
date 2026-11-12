import test from 'node:test';
import assert from 'node:assert/strict';
import {setNavigationSignal, setSession} from '../src/console/api.js';
import {copy, elements, flush, installDOM} from './console-dom.js';
import {validResponse, validService} from '../src/public/validate.js';

const id = '00000000-0000-4000-8000-000000000001';
const service = (extra = {}) => ({id, name: 'Health', type: 'http', target: 'https://internal.example/health', interval_seconds: 60, timeout_seconds: 10,
  latency_threshold_ms: 0, display_order: 0, public: true, notify: true, enabled: true, latest: {status: 'up', latency_ms: 12}, availability_24h: 99.9, ...extra});
const admin = () => setSession({csrf_token: 'csrf', user: {id, username: 'admin', must_change_password: false}, expires_at: ''});
const control = (root, name) => elements(root).find((element) => element.name === name);

test('service editor switches target placeholders and submits typed payloads including false and zero', async () => {
  const dom = installDOM(); const originalFetch = globalThis.fetch; setNavigationSignal(null); admin();
  const sent = []; let saved = 0;
  globalThis.fetch = async (url, options) => { sent.push({url: String(url), method: options.method, body: JSON.parse(options.body)}); return new Response(JSON.stringify(service())); };
  try {
    const {serviceEditor, servicePayload} = await import('../src/console/pages/services.js');
    const dialog = serviceEditor(null, {signal: new AbortController().signal, saved: async () => { saved++; }});
    const root = dom.body;
    assert.equal(control(root, 'target').placeholder, 'https://example.com/health');
    const type = control(root, 'type'); type.value = 'tcp'; type.dispatchEvent(new Event('change'));
    assert.equal(control(root, 'target').placeholder, 'example.com:443');
    type.value = 'icmp'; type.dispatchEvent(new Event('change'));
    assert.equal(control(root, 'target').placeholder, 'example.com');
    control(root, 'name').value = ' Ping '; control(root, 'target').value = ' example.com ';
    const fields = Object.fromEntries(['name', 'type', 'target', 'interval_seconds', 'timeout_seconds', 'latency_threshold_ms', 'display_order', 'public', 'notify', 'enabled'].map((name) => [name, control(root, name)]));
    assert.deepEqual(servicePayload(fields), {name: 'Ping', type: 'icmp', target: 'example.com', interval_seconds: 60, timeout_seconds: 10, latency_threshold_ms: 0, display_order: 0, public: false, notify: false, enabled: true});
    for (const name of ['public', 'notify', 'enabled']) assert.equal(control(root, name).getAttribute('role'), 'switch');
    root.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(sent[0].url, '/api/v1/services'); assert.equal(sent[0].method, 'POST'); assert.equal(saved, 1);
    dialog.close('done'); await flush();
    const edit = serviceEditor(service(), {saved: async () => { saved++; }});
    control(root, 'enabled').checked = false; control(root, 'display_order').value = '-3';
    root.querySelector('form').dispatchEvent(new Event('submit', {cancelable: true})); await flush();
    assert.equal(sent[1].method, 'PATCH'); assert.equal(sent[1].body.enabled, false); assert.equal(sent[1].body.display_order, -3);
    edit.close('done');
  } finally { setSession({user: null, csrf_token: '', expires_at: ''}); dom.restore(); globalThis.fetch = originalFetch; }
});

test('services table, permission hint, history ranges and empty add action render', async () => {
  const dom = installDOM(); const originalFetch = globalThis.fetch; const originalMatch = globalThis.matchMedia;
  globalThis.matchMedia = () => ({matches: true}); const navigation = new AbortController(); setNavigationSignal(navigation.signal); admin();
  const ranges = []; let empty = false;
  globalThis.fetch = async (url) => {
    if (String(url).includes('/history')) { ranges.push(new URL(String(url), 'https://example.com').searchParams.get('range')); return new Response(JSON.stringify({range: ranges.at(-1), step_seconds: 300, buckets: []})); }
    return new Response(JSON.stringify({services: empty ? [] : [service({type: 'icmp', latest: {status: 'down', error: 'permission_denied', latency_ms: null}})]}));
  };
  try {
    const {servicesPage, serviceDetail} = await import('../src/console/pages/services.js');
    const page = await servicesPage(); dom.body.append(page);
    assert.ok(page.querySelector('table')); assert.ok(page.textContent.includes('net.ipv4.ping_group_range')); assert.ok(page.textContent.includes('Health'));
    const dialog = serviceDetail(service(), navigation.signal); await flush();
    assert.deepEqual(ranges, ['24h']);
    const range = elements(dom.body).find((e) => e.tagName === 'button' && e.textContent === copy('services.range.7d'));
    range.click(); await flush(); assert.deepEqual(ranges, ['24h', '7d']);
    assert.ok(dom.body.textContent.includes(copy('services.noHistory'))); dialog.close('done'); await flush();
    navigation.abort(); page.remove();
    empty = true; const next = new AbortController(); setNavigationSignal(next.signal); const blank = await servicesPage(); dom.body.append(blank);
    assert.ok(blank.textContent.includes(copy('services.empty'))); assert.equal(blank.querySelector('table'), null);
    elements(blank).find((e) => e.tagName === 'button' && e.textContent === copy('services.add')).click();
    assert.ok(dom.body.querySelector('form')); next.abort();
    const {closeDialogs} = await import('../src/console/ui/dialogs.js'); closeDialogs();
  } finally { navigation.abort(); setNavigationSignal(null); setSession({user: null, csrf_token: '', expires_at: ''}); dom.restore(); globalThis.fetch = originalFetch; globalThis.matchMedia = originalMatch; }
});

test('public services strictly validate 30 nullable percentages and reject target/error leakage', () => {
  const entry = {name: 'Health', type: 'tcp', status: 'up', latency_ms: 3, availability: Array(30).fill(null)};
  const response = {services: [entry], nodes: [], has_more: false, next_offset: null, generated_at: '2026-11-11T12:00:00Z'};
  assert.equal(validResponse(response), true);
  for (const bad of [{...entry, target: 'secret'}, {...entry, error: 'timeout'}, {...entry, latency_ms: Infinity}, {...entry, latency_ms: -1}, {...entry, status: 'disabled'}, {...entry, type: 'udp'}, {...entry, availability: Array(29).fill(100)}, {...entry, availability: Array(30)}, {...entry, availability: Array(30).fill(101)}, {...entry, name: ''}, {...entry, name: 'Bad\nname'}]) assert.equal(validService(bad), false);
  assert.equal(validResponse({...response, services: undefined}), false);
});

test('public services reuse daily bars, update statuses, and hide when empty', async () => {
  const dom = installDOM();
  try {
    const {serviceSection} = await import('../src/public/services.js'); const section = serviceSection(); dom.body.append(section);
    assert.equal(section.hidden, true);
    section.update([{name: '<Health>', type: 'http', status: 'up', latency_ms: 4.5, availability: Array(30).fill(100)}], '2026-11-11T12:00:00Z');
    assert.equal(section.hidden, false); assert.equal(section.querySelector('strong').textContent, '<Health>'); assert.equal(section.querySelectorAll('.availability-day').length, 30);
    assert.ok(section.textContent.includes('4.5 ms')); assert.ok(section.querySelector('.availability').getAttribute('aria-label').includes('100%')); assert.ok(!section.querySelector('.availability').getAttribute('aria-label').includes(copy('public.availabilityNoData'))); assert.equal(section.querySelector('.dot').getAttribute('aria-label'), copy('public.serviceStatus.up'));
    section.update([{name: 'Health', type: 'http', status: 'down', latency_ms: null, availability: Array(30).fill(null)}], '2026-11-11T12:00:00Z');
    assert.equal(section.querySelector('.dot').getAttribute('aria-label'), copy('public.serviceStatus.down'));
    section.update([], '2026-11-11T12:00:00Z'); assert.equal(section.hidden, true);
  } finally { dom.restore(); }
});

test('service inbox details render the immutable event and link services without an alert lookup', async () => {
  const dom = installDOM(); setNavigationSignal(null);
  const record = {id, source_id: id, kind: 'service', transition: 'certificate_expiring', read: true, created_at: '2026-11-11T12:00:00Z',
    details: {name: 'Health', target: 'https://internal.example', error: '', latency_ms: 12, tls_expires_at: '2026-11-20T00:00:00Z'}};
  const calls = [];
  const {createNotificationInbox} = await import('../src/console/ui/notification-inbox.js');
  const inbox = createNotificationInbox({api: async (path) => {
    calls.push(path);
    if (path.startsWith('/notifications?')) return {notifications: [record], has_more: false, read_through: id};
    return record;
  }, isActive: () => true, isStale: () => false, failureText: () => '', refreshSummary: () => {}, openTaskDetails: () => { throw new Error('service is not a task'); }});
  try {
    await flush();
    const row = document.querySelector('.notification-row');
    elements(row).find((e) => e.tagName === 'button' && e.textContent.includes('Health')).click(); await flush();
    assert.ok(dom.body.textContent.includes(copy('services.certificateExpiry')));
    assert.ok(dom.body.textContent.includes(copy('services.event.certificate_expiring')));
    assert.equal(document.querySelector('.notification-source').querySelector('a').getAttribute('href'), '/services');
    assert.equal(calls.some((path) => path.startsWith('/alerts/')), false);
  } finally { inbox.close(); await inbox.closed; dom.restore(); }
});
