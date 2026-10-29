import test from 'node:test';
import assert from 'node:assert/strict';
import {buildChannelPayload, durationSeconds, formatMetricValue, formatRuleDuration, metricUnit, scopeLabel, validateChannel, validateRule, validHost} from '../src/console/lib/alert-utils.js';

const id = '00000000-0000-4000-8000-000000000001';
const valid = () => ({name: 'High CPU', enabled: true, scope_kind: 'all', scope_id: null, metric: 'cpu_percent', comparator: '>', threshold: 80, for_seconds: 60, severity: 'warning', channel_ids: []});
test('rule validation covers bounds, offline semantics, UUIDs and UTF-8 names', () => {
  assert.deepEqual(validateRule(valid()), {});
  for (const threshold of [-1, 101, NaN, Infinity, null, '80']) assert.ok(validateRule({...valid(), threshold}).threshold);
  for (const threshold of [0, 100]) assert.deepEqual(validateRule({...valid(), threshold}), {});
  assert.deepEqual(validateRule({...valid(), metric: 'load1', threshold: 1000000}), {});
  assert.ok(validateRule({...valid(), metric: 'load1', threshold: 1000001}).threshold);
  assert.deepEqual(validateRule({...valid(), metric: 'probes_down', threshold: 5}), {});
  assert.ok(validateRule({...valid(), metric: 'probes_down', threshold: 6}).threshold);
  const offline = {...valid(), metric: 'offline', comparator: null, threshold: null, for_seconds: 180};
  assert.deepEqual(validateRule(offline), {});
  assert.ok(validateRule({...offline, for_seconds: 179}).for_seconds);
  assert.ok(validateRule({...offline, comparator: '>'}).comparator);
  assert.ok(validateRule({...offline, threshold: 1}).threshold);
  for (const for_seconds of [-1, .5, 86401, NaN]) assert.ok(validateRule({...valid(), for_seconds}).for_seconds);
  assert.deepEqual(validateRule({...valid(), for_seconds: 86400}), {});
  assert.ok(validateRule({...valid(), name: '   '}).name);
  assert.ok(validateRule({...valid(), name: '界'.repeat(34)}).name);
  assert.deepEqual(validateRule({...valid(), name: '界'.repeat(33)}), {});
  assert.ok(validateRule({...valid(), scope_kind: 'node', scope_id: ''}).scope_id);
  assert.deepEqual(validateRule({...valid(), scope_kind: 'group', scope_id: id}), {});
  assert.ok(validateRule({...valid(), scope_id: id}).scope_id);
  assert.ok(validateRule({...valid(), channel_ids: [id, id]}).channel_ids);
  assert.ok(validateRule({...valid(), severity: ''}).severity);
});
test('metric values preserve precision, durations preserve components and scope labels preserve user content', () => {
  assert.equal(metricUnit('cpu_percent'), '%');
  assert.equal(formatMetricValue('memory_used_percent', 80.25), '80.3%');
  assert.match(formatMetricValue('memory_used_percent', 1000), /1,000%/);
  assert.equal(parseFloat(formatMetricValue('load1', 1.125)), 1.13);
  assert.match(formatMetricValue('load1', .001), /^< 0\.01\b/);
  assert.match(formatMetricValue('load1', 1000), /1,000\.00/);
  assert.equal(parseFloat(formatMetricValue('probes_down', 1)), 1);
  assert.equal(parseFloat(formatMetricValue('probes_down', 2)), 2);
  assert.equal(parseFloat(formatMetricValue('', 12)), 12);
  assert.deepEqual(formatRuleDuration(0).match(/\d+/g), ['0']);
  assert.deepEqual(formatRuleDuration(65).match(/\d+/g), ['1', '5']);
  assert.deepEqual(formatRuleDuration(180).match(/\d+/g), ['3']);
  assert.deepEqual(formatRuleDuration(86400).match(/\d+/g), ['1440']);
  assert.equal(durationSeconds('2', '59'), 179);
  assert.equal(durationSeconds('1440', '0'), 86400);
  for (const parts of [['', '0'], ['1', ''], ['0.5', '0'], ['0', '60'], ['-1', '0']]) assert.ok(Number.isNaN(durationSeconds(...parts)));
  assert.ok(scopeLabel({scope_kind: 'group', scope_id: id}, [{id, name: 'Production'}]).includes('Production'));
  assert.ok(scopeLabel({scope_kind: 'node', scope_id: id}, [], [{id, name: 'Edge'}]).includes('Edge'));
  assert.ok(scopeLabel({scope_kind: 'group', scope_id: id}).includes(id));
});

test('alert metric values retain grouping and duration components distinct from sample formats', () => {
  assert.equal(formatMetricValue('cpu_percent', 1234.5), '1,234.5%');
  assert.equal(formatMetricValue('load1', 1234.5), '1,234.50 load');
  assert.equal(formatRuleDuration(3665), '61m 5s');
});
test('channel PATCH omits untouched secrets and full webhook URL, including redacted response fields', () => {
  const values = {name: 'Hook', enabled: true, kind: 'webhook', url: '', secret: '', url_redacted: 'https://example.com/…', secret_set: true};
  assert.deepEqual(buildChannelPayload(values, {editing: true}), {name: 'Hook', enabled: true, config: {}});
  assert.deepEqual(buildChannelPayload({...values, secret: 'new'}, {editing: true, replaceSecret: true}), {name: 'Hook', enabled: true, config: {}, secret: 'new'});
  assert.equal(buildChannelPayload(values, {editing: true, replaceSecret: true}).secret, '');
  assert.deepEqual(buildChannelPayload({...values, url: 'https://example.com/new'}, {editing: true, replaceURL: true}).config, {url: 'https://example.com/new'});
  assert.deepEqual(buildChannelPayload({...values, url: 'https://example.com/hook'}), {name: 'Hook', enabled: true, kind: 'webhook', config: {url: 'https://example.com/hook'}});
});
test('channel payloads and validation support all kinds without manufacturing credentials', () => {
  const telegram = buildChannelPayload({name: 'Bot', enabled: false, kind: 'telegram', chat_id: '-12345', secret: '123456:token'});
  assert.deepEqual(telegram.config, {chat_id: '-12345'});
  assert.deepEqual(validateChannel(telegram), {});
  assert.ok(validateChannel({...telegram, secret: ''}).secret);
  assert.ok(validateChannel({...telegram, config: {chat_id: '@bad'}})['config.chat_id']);
  const email = buildChannelPayload({name: 'Mail', enabled: true, kind: 'email', host: 'mail.example.com', port: '587', username: 'user', from: 'a@example.com', to: 'b@example.com, c@example.com\nd@example.com', secret: ''}, {editing: true});
  assert.deepEqual(email.config.to, ['b@example.com', 'c@example.com', 'd@example.com']);
  assert.equal(email.config.port, 587);
  assert.ok(!Object.hasOwn(email, 'secret'));
  assert.deepEqual(validateChannel(email, {kind: 'email', existing: {secret_set: true}}), {});
  assert.ok(validateChannel(email, {kind: 'email'}).secret);
  assert.ok(validateChannel({...email, config: {...email.config, port: 0}}, {kind: 'email'})['config.port']);
  const webhook = {name: 'Hook', kind: 'webhook', config: {url: 'https://example.com/path?key=abc'}};
  assert.deepEqual(validateChannel(webhook), {});
  for (const url of ['http://example.com', 'https://user:pass@example.com', 'https://example.com/#fragment', 'javascript:alert(1)']) assert.ok(validateChannel({...webhook, config: {url}})['config.url']);
  assert.deepEqual(validateChannel({...webhook, config: {url: 'http://127.0.0.1/hook'}}), {});
});

test('mail host validation mirrors the server rules', () => {
  for (const host of ['smtp.example.com', 'smtp.example.com.', 'mail-1.example', '192.0.2.10', '2001:db8::1']) assert.equal(validHost(host), true, host);
  for (const host of ['', 'smtp_server.example.com', '-bad.example', 'bad-.example', 'a..b', 'fe80::1%eth0', 'smtp host', 'x'.repeat(64) + '.example']) assert.equal(validHost(host), false, host);
});
