import test from 'node:test';
import assert from 'node:assert/strict';

import {changedConfig, quotaToBytes, validateProbeTarget, validateProbeTargets, validateSelection} from '../src/console/pages/node-config-helpers.js';
import {installDOM} from './console-dom.js';

test('quota conversion uses exact decimal BigInt arithmetic and uint64 bounds', () => {
  assert.deepEqual(quotaToBytes('9', 'GB'), {ok: true, value: '9000000000'});
  assert.deepEqual(quotaToBytes('0', 'TB'), {ok: true, value: '0'});
  assert.equal(quotaToBytes('18446744073709551', 'KB').ok, true);
  assert.equal(quotaToBytes('18446744073709552', 'KB').ok, false);
  assert.equal(quotaToBytes('9007199254740993', 'bytes').value, '9007199254740993');
  assert.equal(quotaToBytes(Number.MAX_SAFE_INTEGER + 1, 'bytes').ok, false);
  assert.equal(quotaToBytes('01', 'bytes').ok, true);
  assert.equal(quotaToBytes('1', 'constructor').ok, false);
});

test('selection validation enforces server-compatible limits', () => {
  assert.equal(validateSelection([], 'interfaces'), '');
  assert.equal(validateSelection(['eth0', 'eth0'], 'interfaces'), 'Interfaces must be unique.');
  assert.match(validateSelection(['\u0000'], 'interfaces'), /NUL/);
  assert.match(validateSelection(['\n'], 'interfaces'), /NUL/);
  assert.match(validateSelection(['\u00a0'], 'interfaces'), /blank/);
  assert.match(validateSelection(['é'.repeat(128)], 'interfaces'), /255/);
  assert.match(validateSelection(['relative'], 'disk_paths'), /absolute/);
  assert.equal(validateSelection(['/var/lib/orbit'], 'disk_paths'), '');
  assert.match(validateSelection(['/'.repeat(1025)], 'disk_paths'), /1024/);
  assert.match(validateSelection(Array.from({length: 9}, (_, i) => `eth${i}`), 'interfaces'), /8/);
});

test('changedConfig emits only changed fields', () => {
  const config = {report_interval_seconds: 30, disk_interval_seconds: 60, billing_day: 1, interfaces: ['eth0'], disk_paths: ['/'], probe_targets: ['tcp://db:5432'], quota_bytes: '1000'};
  assert.deepEqual(changedConfig(config, {...config}), {});
  assert.deepEqual(changedConfig(config, {report_interval_seconds: '45', disk_interval_seconds: '60', billing_day: '1', interfaces: ['eth0'], disk_paths: ['/'], quota_bytes: '1000'}), {report_interval_seconds: 45});
  assert.deepEqual(changedConfig(config, {interfaces: [], disk_paths: ['/'], quota_bytes: '1000'}), {interfaces: []});
  assert.deepEqual(changedConfig(config, {probe_targets: ['https://example.com/health']}), {probe_targets: ['https://example.com/health']});
});

test('probe target validation accepts supported target forms', () => {
  for (const target of [
    'tcp://db:5432', 'tcp://example.com.:1', 'tcp://192.0.2.10:65535', 'tcp://[2001:db8::1]:443',
    'tcp://[2001:0DB8:0:0:0:0:0:1]:443', 'http://example.com', 'https://example.com/health,ready?value=1', 'icmp://router',
    'icmp://192.0.2.1', 'icmp://[2001:db8::2]'
  ]) assert.equal(validateProbeTarget(target), '', target);
});

test('probe target validation rejects invalid schemes, whitespace, hosts, ports, and URL parts', () => {
  for (const target of [
    'udp://db:53', 'tcp://db', 'tcp://db:0', 'tcp://db:65536', 'tcp://db:abc',
    'tcp://999.2.3.4:80', 'tcp://bad_host:80', 'tcp://2001:db8::1:443',
    'https://', 'https:///host', 'https://host\\evil', 'https://@host', 'https://user:pass@example.com/',
    'https://example.com/#section', 'https://example.com/#',
    'icmp://host:7', 'icmp://host/path', 'icmp://bad_host', 'icmp://',
    'tcp://db :5432', 'https://example.com/a b', 'icmp://host\u0085',
    'http://999.1.1.1/', 'http://example.test/%zz',
    // Parity with ValidateProbeTarget in internal/protocol/v1/protocol.go.
    'http://127.1/', 'http://127.0.0.1./', 'http://0177.0.0.1/', 'http://2130706433/', 'http://0x100000000/',
    'http://%65xample.com/', 'http://%25/', 'http://例え.テスト/', 'http://\u200d.com/', 'http://h^x/', 'http://0x7f.1/', 'tcp://0x7f.1:80',
    'http://h/\u0000x', 'http://h/\u007fx', 'http://h?x=\u0001', 'http://h:99999/', 'http://h:abc/', 'http://[::1/', 'http://[fe80::1%25eth0]/', 'http://[::1]evil/', 'http://[::1]evil:80/', 'http://[::1].example/'
  ]) assert.notEqual(validateProbeTarget(target), '', target);
  for (const target of ['http://h?x=%zz', 'http://h:/', 'https://[2001:db8::1]:8443/x', 'http://127.0.0.1:8080/health', 'https://example.com./',
    'http://xn--a/', 'http://h:0/', 'https://xn--bcher-kva.example/', 'https://example.com:443/a?b=c&d=%41']) {
    assert.equal(validateProbeTarget(target), '', target);
  }
});

test('probe target list validation enforces uniqueness, count, and UTF-8 byte limits', () => {
  assert.equal(validateProbeTargets([]), '');
  assert.match(validateProbeTargets(['tcp://db:5432', 'tcp://db:5432']), /unique/);
  assert.match(validateProbeTargets(Array.from({length: 6}, (_, index) => `icmp://host${index}`)), /5/);
  assert.equal(new TextEncoder().encode(`https://example.com/${'é'.repeat(502)}`).length, 1024);
  assert.equal(validateProbeTargets([`https://example.com/${'é'.repeat(502)}`]), '');
  assert.match(validateProbeTargets([`https://example.com/${'é'.repeat(503)}`]), /1024/);
  assert.match(validateProbeTargets('tcp://db:5432'), /list/);
});

test('node configuration renders service probes read-only and in the editable tag editor', async () => {
  const dom = installDOM();
  try {
    globalThis.window = new EventTarget();
    globalThis.matchMedia = () => ({matches: false, addEventListener() {}});
    const {configTab} = await import('../src/console/pages/node-config.js');
    const config = {revision: 3, report_interval_seconds: 30, disk_interval_seconds: 60, billing_day: 1,
      interfaces: [], disk_paths: [], probe_targets: ['https://example.com/health,ready'], quota_bytes: '0'};
    const node = {id: 'node-1', config: {desired_revision: 3, applied_revision: 3}};
    const readOnly = configTab(node, config, false);
    assert.match(readOnly.textContent, /Service probes/);
    assert.match(readOnly.textContent, /https:\/\/example\.com\/health,ready/);
    const editable = configTab(node, config, true);
    const probes = editable.querySelector('[data-field="probe_targets"]');
    assert.deepEqual(probes.getValue(), ['https://example.com/health,ready']);
    assert.match(editable.textContent, /Up to 5 targets/);
  } finally {
    delete globalThis.window;
    delete globalThis.matchMedia;
    dom.restore();
  }
});
