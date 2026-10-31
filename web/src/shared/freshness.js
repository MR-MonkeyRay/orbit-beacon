// 数据是否过期以响应中的服务端时钟为准，不依赖浏览器本地时钟。
// 数据源的 age_ms 属于已接受的上报，不因计时器触发而重置。
export function timestampMillis(value) {
  return typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value)
    ? Date.parse(value) : NaN;
}

export function sourceDeadline(receivedAt, source) {
  const received = timestampMillis(receivedAt);
  if (!Number.isFinite(received) || !Number.isFinite(source?.age_ms) || source.age_ms < 0
    || !Number.isFinite(source.source_interval_ms) || source.source_interval_ms <= 0) return NaN;
  return received + Math.max(3 * source.source_interval_ms, 45000) - source.age_ms;
}

// 每页一个到期计时器。onChange 只修补新鲜度，并返回
// 下一个服务端时钟到期时间；没有更多会过期的内容时返回 null。
export function createFreshnessController({signal, onChange}) {
  let serverAt = NaN;
  let monotonicAt = 0;
  let timer;
  let generation = 0;
  let closed = false;
  let paused = false;
  let pageHidden = false;
  const now = () => serverAt + Math.max(0, performance.now() - monotonicAt);
  const cancel = () => { generation += 1; clearTimeout(timer); timer = undefined; };
  const active = () => !closed && !signal?.aborted && !paused && !pageHidden && !document.hidden;
  const refresh = () => {
    cancel();
    if (!active()) return;
    const time = now();
    const deadline = onChange(time);
    if (!Number.isFinite(time) || !Number.isFinite(deadline) || deadline < time) return;
    const current = generation;
    // 相等时数据源仍算新鲜。至少超过一整毫秒才算越过。
    timer = setTimeout(() => { if (current === generation && active()) refresh(); }, Math.min(2147483647, Math.max(1, Math.ceil(deadline - time) + 1)));
  };
  const visibility = () => { if (document.hidden) cancel(); else refresh(); };
  const hide = () => { pageHidden = true; cancel(); };
  const show = () => { pageHidden = false; refresh(); };
  const close = () => {
    if (closed) return;
    closed = true;
    cancel();
    document.removeEventListener('visibilitychange', visibility);
    window.removeEventListener('pagehide', hide);
    window.removeEventListener('pageshow', show);
    signal?.removeEventListener('abort', close);
  };
  document.addEventListener('visibilitychange', visibility);
  window.addEventListener('pagehide', hide);
  window.addEventListener('pageshow', show);
  signal?.addEventListener('abort', close, {once: true});
  if (signal?.aborted) close();
  return {
    clock(serverTime) {
      if (closed || signal?.aborted) return;
      const value = timestampMillis(serverTime);
      if (!Number.isFinite(value)) return;
      const monotonicNow = performance.now();
      const current = serverAt + Math.max(0, monotonicNow - monotonicAt);
      serverAt = Number.isFinite(current) ? Math.max(value, current) : value;
      monotonicAt = monotonicNow;
      refresh();
      return true;
    },
    refresh,
    pause() { paused = true; cancel(); },
    resume() { paused = false; refresh(); },
    close
  };
}
