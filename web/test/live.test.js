import test from 'node:test';
import assert from 'node:assert/strict';
import {connectLive} from '../src/shared/live.js';
import {flush} from './console-dom.js';
import {frame, liveHarness, response, stream} from './live-harness.js';

const urls = {ws: 'wss://example.test/ws', sse: '/stream', http: '/nodes'};
function consumer(options = {}) {
  const snapshots = [], states = [], clocks = [];
  let validations = 0;
  const live = connectLive({urls, credentials: 'omit', validateSnapshot(data) { validations += 1; return Number.isFinite(data?.value); },
    onSnapshot: (data, meta) => snapshots.push({data, meta}), onState: (state) => states.push(state), onClock: (time) => clocks.push(time), ...options});
  return {live, snapshots, states, clocks, get validations() { return validations; }};
}

test('WS delivers one validated snapshot; quiet heartbeats calibrate only the clock', async (t) => {
  const env = liveHarness(t);
  const ui = consumer();
  const socket = env.sockets[0];
  socket.open(); socket.message(frame());
  assert.equal(ui.validations, 1);
  assert.equal(ui.snapshots.length, 1);
  const transitions = ui.states.length;
  for (let i = 0; i < 4; i++) { await env.advance(10000); socket.message(frame('heartbeat')); }
  assert.equal(ui.clocks.length, 5);
  assert.equal(ui.snapshots.length, 1);
  assert.equal(ui.states.length, transitions);
  assert.equal(env.requests.length, 0);
  assert.equal(ui.states.at(-1).state, 'live');
  ui.live.close();
});

for (const failure of ['handshake', 'first snapshot', 'watchdog', 'invalid JSON', 'invalid snapshot', 'invalid heartbeat']) {
  test(`WS ${failure} closes the old socket before SSE fallback and preserves accepted data`, async (t) => {
    const sse = stream();
    const env = liveHarness(t, () => sse.response);
    const ui = consumer();
    const socket = env.sockets[0];
    if (failure !== 'handshake') socket.open();
    if (!['handshake', 'first snapshot'].includes(failure)) socket.message(frame());
    if (failure === 'handshake') socket.fail();
    else if (failure === 'first snapshot') { socket.message(frame('heartbeat')); await env.advance(8000); }
    else if (failure === 'watchdog') await env.advance(30000);
    else if (failure === 'invalid JSON') socket.message('{');
    else if (failure === 'invalid snapshot') socket.message(frame('snapshot', {data: {value: 'bad'}}));
    else socket.message(frame('heartbeat', {data: {value: 9}}));
    await flush();
    assert.equal(socket.closed, true);
    assert.ok(env.order.indexOf('ws:close') < env.order.indexOf('sse:start'));
    assert.equal(env.requests[0].signal.aborted, false);
    socket.message(frame('snapshot', {data: {value: 99}}));
    sse.frame(frame('snapshot', {data: {value: 2}}));
    await flush();
    assert.equal(ui.snapshots.at(-1).data.value, 2);
    assert.equal(ui.snapshots.some(({data}) => data.value === 99), false);
    ui.live.close();
    await flush();
    assert.equal(sse.cancelled, true);
  });
}

for (const failure of ['first snapshot', 'watchdog', 'EOF', 'invalid JSON', 'invalid UTF-8']) {
  test(`SSE ${failure} cancels the reader before falling back to HTTP`, async (t) => {
    const sse = stream();
    const env = liveHarness(t, (request) => request.url === '/stream' ? sse.response : response(frame()));
    const ui = consumer();
    env.sockets[0].fail(); await flush();
    if (failure === 'first snapshot') { sse.text(': comment\n\n'); await env.advance(8000); }
    else if (failure === 'watchdog') { sse.frame(frame()); await flush(); await env.advance(30000); }
    else if (failure === 'EOF') sse.end();
    else if (failure === 'invalid JSON') sse.text('event: message\ndata: {\n\n');
    else sse.bytes(Uint8Array.of(0xff));
    await flush();
    assert.equal(sse.cancelled || failure === 'EOF', true);
    assert.equal(env.requests[0].signal.aborted, true);
    assert.equal(env.requests.at(-1).url, '/nodes');
    assert.equal(ui.states.at(-1).transport, 'http');
    ui.live.close();
  });
}

test('HTTP waits the full server 60 seconds without a stream watchdog and reports its countdown', async (t) => {
  const env = liveHarness(t, (request) => request.url === '/stream' ? response({}, {status: 503}) : response(frame('snapshot', {update_interval_seconds: 60})));
  const ui = consumer(); env.sockets[0].fail(); await flush();
  assert.equal(ui.states.at(-1).intervalSeconds, 60);
  assert.equal(ui.states.at(-1).retryAt, Date.now() + 60000);
  await env.advance(59999);
  assert.equal(env.requests.length, 2);
  assert.equal(ui.states.at(-1).state, 'live');
  await env.advance(1);
  assert.equal(env.requests.length, 3);
  ui.live.close();
});

for (const retry of ['90', 'Wed, 28 Oct 2026 00:01:30 GMT']) {
  test(`HTTP respects larger Retry-After ${retry}`, async (t) => {
    const env = liveHarness(t, (request) => request.url === '/stream' ? response({}, {status: 503}) : response({}, {status: 429, headers: {'Retry-After': retry}}));
    const ui = consumer(); env.sockets[0].fail(); await flush();
    await env.advance(89999); assert.equal(env.requests.length, 2);
    await env.advance(1); assert.equal(env.requests.length, 3);
    ui.live.close();
  });
}

test('HTTP eight-second timeout aborts fetch and rejects its late ignored-abort result', async (t) => {
  let resolve;
  const env = liveHarness(t, (request) => request.url === '/stream' ? response({}, {status: 503}) : new Promise((done) => { resolve = done; }));
  const ui = consumer(); env.sockets[0].fail(); await flush();
  await env.advance(8000);
  assert.equal(env.requests[1].signal.aborted, true);
  assert.equal(ui.states.at(-1).state, 'stale');
  resolve(response(frame())); await flush();
  assert.equal(ui.snapshots.length, 0);
  await env.advance(10000); assert.equal(env.requests.length, 3);
  ui.live.close();
});

for (const transport of ['sse', 'http']) {
  test(`${transport} re-prefers WS after exactly 300 seconds with only one active channel`, async (t) => {
    const sse = stream();
    const env = liveHarness(t, (request) => request.url === '/stream'
      ? transport === 'sse' ? sse.response : response({}, {status: 503}) : response(frame('snapshot', {update_interval_seconds: 60})));
    const ui = consumer(); env.sockets[0].fail(); await flush();
    for (let i = 0; i < 29; i++) { if (transport === 'sse') sse.frame(frame(i ? 'heartbeat' : 'snapshot')); await flush(); await env.advance(10000); }
    if (transport === 'sse') sse.frame(frame('heartbeat'));
    await flush(); await env.advance(9999);
    assert.equal(env.sockets.length, 1);
    await env.advance(1);
    assert.equal(env.sockets.length, 2);
    if (transport === 'sse') {
      assert.equal(env.requests[0].signal.aborted, true);
      assert.equal(sse.cancelled, true);
    }
    env.sockets[1].open(); env.sockets[1].message(frame());
    assert.equal(ui.states.at(-1).transport, 'ws');
    ui.live.close(); assert.equal(env.timers.size, 0);
  });
}

test('refresh, visibility and BFCache stop timers and reject old callbacks; abort is final', async (t) => {
  const env = liveHarness(t);
  const scope = new AbortController();
  const ui = consumer({signal: scope.signal});
  env.sockets[0].open(); env.sockets[0].message(frame());
  ui.live.refresh();
  env.sockets[0].message(frame('snapshot', {data: {value: 99}}));
  assert.equal(env.sockets[0].closed, true);
  env.document.hidden = true; env.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(env.sockets[1].closed, true); assert.equal(env.timers.size, 0);
  env.document.hidden = false; env.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(env.sockets.length, 3);
  env.window.dispatchEvent(new Event('pagehide')); assert.equal(env.timers.size, 0);
  env.window.dispatchEvent(Object.assign(new Event('pageshow'), {persisted: true}));
  assert.equal(env.sockets.length, 4);
  scope.abort(); env.sockets[3].message(frame()); ui.live.refresh(); await env.advance(600000);
  assert.equal(env.sockets.length, 4); assert.equal(ui.snapshots.length, 1); assert.equal(env.timers.size, 0);
});

test('renew reconnects the same adapter immediately; shutdown retains data and backs off', async (t) => {
  const env = liveHarness(t);
  const ui = consumer(); env.sockets[0].open(); env.sockets[0].message(frame());
  env.sockets[0].message(frame('close', {code: 'renew'}));
  assert.equal(env.sockets[0].closed, true); assert.equal(env.sockets.length, 2);
  env.sockets[1].open(); env.sockets[1].message(frame('close', {code: 'shutdown'}));
  assert.equal(ui.states.at(-1).state, 'stale'); assert.equal(env.sockets.length, 2);
  await env.advance(10000); assert.equal(env.sockets.length, 3);
  assert.equal(ui.snapshots.length, 1); ui.live.close();
});

test('WS handshake verification awaits and false stops instead of falling back', async (t) => {
  const env = liveHarness(t);
  let resolve;
  const reasons = [];
  const ui = consumer({onUnauthorized: (reason) => { reasons.push(reason); return new Promise((done) => { resolve = done; }); }});
  env.sockets[0].fail(); await flush();
  assert.deepEqual(reasons, ['verify']); assert.equal(env.requests.length, 0);
  resolve(false); await flush(); await env.advance(600000);
  assert.equal(env.requests.length, 0); assert.equal(env.timers.size, 0); ui.live.refresh();
  assert.equal(env.sockets.length, 1);
});

for (const kind of ['ws', 'sse', 'http']) {
  test(`${kind} known unauthenticated is terminal without ordinary fallback`, async (t) => {
    const sse = stream(); const reasons = [];
    const env = liveHarness(t, (request) => request.url === '/stream'
      ? kind === 'http' ? response({}, {status: 503}) : kind === 'sse' ? response({}, {status: 401}) : sse.response
      : response({}, {status: 401}));
    const ui = consumer({onUnauthorized: (reason) => { reasons.push(reason); return true; }});
    if (kind === 'ws') { env.sockets[0].open(); env.sockets[0].message(frame('error', {code: 'unauthenticated'})); }
    else env.sockets[0].fail();
    await flush(); await env.advance(600000);
    assert.equal(reasons.at(-1), 'unauthenticated');
    assert.equal(env.requests.length, kind === 'ws' ? 0 : kind === 'sse' ? 1 : 2);
    assert.equal(env.timers.size, 0); assert.equal(ui.snapshots.length, 0);
  });
}

test('malformed control frames and clocks cannot update data or the validated server clock', async (t) => {
  const sse = stream();
  const env = liveHarness(t, () => sse.response);
  const ui = consumer(); env.sockets[0].open(); env.sockets[0].message(frame());
  env.sockets[0].message(frame('close', {code: 'unexpected'})); await flush();
  sse.frame(frame('snapshot', {server_time: 'not-a-clock', data: {value: 2}})); await flush();
  assert.equal(ui.clocks.length, 1); assert.equal(ui.snapshots.length, 1);
  ui.live.close();
});

test('late SSE setup and HTTP body results cannot write across refresh or scope replacement', async (t) => {
  const old = stream();
  let resolveSetup;
  const env = liveHarness(t, (request) => request.url === '/stream' ? new Promise((done) => { resolveSetup = done; }) : response(frame()));
  const ui = consumer(); env.sockets[0].fail(); await flush();
  ui.live.refresh();
  env.sockets[1].open(); env.sockets[1].message(frame('snapshot', {data: {value: 2}}));
  resolveSetup(old.response); await flush();
  assert.equal(old.cancelled, true); assert.equal(ui.snapshots.length, 1);
  let resolveBody;
  globalThis.fetch = async (url) => url === '/stream' ? response({}, {status: 503})
    : {ok: true, status: 200, headers: new Headers(), json: () => new Promise((done) => { resolveBody = done; })};
  ui.live.refresh(); env.sockets[2].fail(); await flush();
  const count = ui.snapshots.length;
  ui.live.close(); resolveBody(frame('snapshot', {data: {value: 99}})); await flush();
  assert.equal(ui.snapshots.length, count); assert.equal(env.timers.size, 0);
});

test('HTTP successful cadence is measured after the complete response body', async (t) => {
  let resolveBody;
  const env = liveHarness(t, (request) => request.url === '/stream' ? response({}, {status: 503})
    : {ok: true, status: 200, headers: new Headers(), json: () => new Promise((done) => { resolveBody = done; })});
  const ui = consumer(); env.sockets[0].fail(); await flush();
  await env.advance(5000); resolveBody(frame('snapshot', {update_interval_seconds: 2})); await flush();
  assert.equal(ui.states.at(-1).retryAt, Date.now() + 2000);
  await env.advance(1999); assert.equal(env.requests.length, 2);
  await env.advance(1); assert.equal(env.requests.length, 3); ui.live.close();
});

test('SSE message heartbeat keeps liveness without replacing the previous snapshot', async (t) => {
  const sse = stream(); const env = liveHarness(t, () => sse.response);
  const ui = consumer(); env.sockets[0].fail(); await flush();
  sse.frame(frame()); await flush();
  for (let i = 0; i < 5; i++) { await env.advance(10000); sse.frame(frame('heartbeat')); await flush(); }
  assert.equal(ui.snapshots.length, 1); assert.equal(ui.validations, 1); assert.equal(ui.clocks.length, 6);
  assert.equal(env.requests.length, 1); ui.live.close();
});

test('unknown stream messages do not renew first-frame liveness or calibrate time', async (t) => {
  const sse = stream(); const env = liveHarness(t, () => sse.response);
  const ui = consumer(); env.sockets[0].open(); env.sockets[0].message(frame('future'));
  await env.advance(8000); assert.equal(env.sockets[0].closed, true);
  assert.equal(ui.snapshots.length, 0); assert.equal(ui.clocks.length, 0); ui.live.close();
});

test('scope abort inside state/snapshot callbacks leaves no newly scheduled channel or timers', async (t) => {
  const env = liveHarness(t);
  const stopped = new AbortController();
  connectLive({urls, signal: stopped.signal, validateSnapshot: () => true, onSnapshot() {}, onState() { stopped.abort(); }});
  assert.equal(env.sockets.length, 0); assert.equal(env.timers.size, 0);
  const scope = new AbortController();
  const ui = consumer({signal: scope.signal, onSnapshot() { scope.abort(); }});
  env.sockets[0].open(); env.sockets[0].message(frame());
  assert.equal(env.sockets[0].closed, true); assert.equal(env.timers.size, 0);
  ui.live.refresh(); assert.equal(env.sockets.length, 1);
});

test('cancelled asynchronous WS verification cannot fall back into a new scope', async (t) => {
  const env = liveHarness(t); let resolve;
  const ui = consumer({onUnauthorized: () => new Promise((done) => { resolve = done; })});
  env.sockets[0].fail(); await flush(); ui.live.refresh();
  resolve(true); await flush();
  assert.equal(env.sockets.length, 2); assert.equal(env.requests.length, 0); ui.live.close();
});

test('SSE renew closes its reader before a same-adapter reconnect and autherror remains terminal', async (t) => {
  const first = stream(), second = stream(); const reasons = [];
  const env = liveHarness(t, (_request, index) => index === 1 ? first.response : second.response);
  const ui = consumer({onUnauthorized: (reason) => { reasons.push(reason); return true; }});
  env.sockets[0].fail(); await flush(); first.frame(frame()); await flush();
  first.frame(frame('close', {code: 'renew'})); await flush();
  assert.equal(first.cancelled, true); assert.equal(env.requests.length, 2);
  assert.equal(env.requests[0].signal.aborted, true);
  second.frame(frame('error', {code: 'unauthenticated'})); await flush();
  assert.equal(reasons.at(-1), 'unauthenticated'); assert.equal(second.cancelled, true);
  await env.advance(600000); assert.equal(env.requests.length, 2); assert.equal(ui.snapshots.length, 1);
});

test('long Retry-After survives automatic WS re-preference with monotonic timing and server Date', async (t) => {
  let polls = 0;
  const env = liveHarness(t, (request) => request.url === '/stream' ? response({}, {status: 503})
    : (++polls, response({}, {status: 503, headers: {'Date': 'Thu, 01 Jan 2099 00:00:00 GMT', 'Retry-After': 'Thu, 01 Jan 2099 00:10:00 GMT'}})));
  const ui = consumer(); env.sockets[0].fail(); await flush();
  assert.equal(polls, 1);
  env.shiftWall(3600000);
  await env.advance(300000);
  assert.equal(env.sockets.length, 2); env.sockets[1].fail(); await flush();
  assert.equal(polls, 1);
  await env.advance(299999); assert.equal(polls, 1);
  await env.advance(1); env.sockets[2].fail(); await flush();
  assert.equal(polls, 2); ui.live.close();
});

test('HTTP-date Retry-After uses the response Date instead of a skewed browser clock', async (t) => {
  const env = liveHarness(t, (request) => request.url === '/stream' ? response({}, {status: 503})
    : response({}, {status: 429, headers: {'Date': 'Thu, 01 Jan 2099 00:00:00 GMT', 'Retry-After': 'Thu, 01 Jan 2099 00:01:30 GMT'}}));
  const ui = consumer(); env.sockets[0].fail(); await flush();
  assert.equal(ui.states.at(-1).retryAt, Date.now() + 90000);
  await env.advance(89999); assert.equal(env.requests.length, 2);
  await env.advance(1); assert.equal(env.requests.length, 3); ui.live.close();
});
