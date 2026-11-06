import {reducedMotion} from './dom.js';
import {motionTiming} from './motion.js';

// 一个页面共享一个 RAF。WeakMap 保留显示值；只有补间中的元素进入活动表。
export function createNumberTween({signal, now = () => performance.now(),
  requestFrame = (callback) => requestAnimationFrame(callback), cancelFrame = (id) => cancelAnimationFrame(id),
  timing = () => motionTiming('--motion-normal', '--ease-linear')} = {}) {
  const values = new WeakMap();
  const active = new Map();
  let frame;
  let duration;
  let listening = false;
  const write = (element, state, value) => {
    state.current = value;
    const text = state.format(value);
    if (element.textContent !== text) element.textContent = text;
  };
  const unlisten = () => {
    if (!listening) return;
    listening = false;
    document.removeEventListener('visibilitychange', visibility);
    window.removeEventListener('pagehide', finishAll);
    signal?.removeEventListener('abort', finishAll);
  };
  const idle = () => {
    if (active.size) return;
    if (frame !== undefined) cancelFrame(frame);
    frame = undefined;
    unlisten();
  };
  const finish = (element) => {
    const state = active.get(element);
    if (state) { write(element, state, state.target); active.delete(element); }
    idle();
  };
  function finishAll() {
    for (const [element, state] of active) write(element, state, state.target);
    active.clear();
    idle();
  }
  function visibility() { if (document.hidden) finishAll(); }
  function tick(time) {
    frame = undefined;
    if (document.hidden || reducedMotion.matches || signal?.aborted) { finishAll(); return; }
    for (const [element, state] of active) {
      if (!element.isConnected) { write(element, state, state.target); active.delete(element); continue; }
      // RAF 时间戳可能早于 set() 时读到的 now()。
      const progress = Math.max(0, Math.min(1, (time - state.started) / duration));
      write(element, state, progress === 1 ? state.target : state.from + (state.target - state.from) * progress);
      if (progress === 1) active.delete(element);
    }
    if (active.size) frame = requestFrame(tick);
    else unlisten();
  }
  return {finish, set(element, value, format) {
    value = Number.isFinite(value) ? value : null;
    let state = values.get(element);
    // 同一快照的 400 个指标无需格式化、读取样式或重启计时。
    if (state?.target === value && !active.has(element)) return;
    const instant = !element.isConnected || document.hidden || signal?.aborted || reducedMotion.matches;
    if (state?.target === value && !instant) return;
    const previous = state?.current;
    if (!state) { state = {}; values.set(element, state); }
    state.target = value;
    state.format = format;
    if (instant || !Number.isFinite(previous) || value === null) {
      write(element, state, value);
      active.delete(element);
      idle();
      return;
    }
    duration ??= timing().duration;
    state.from = previous;
    state.started = now();
    active.set(element, state);
    if (!listening) {
      listening = true;
      document.addEventListener('visibilitychange', visibility);
      window.addEventListener('pagehide', finishAll);
      signal?.addEventListener('abort', finishAll, {once: true});
    }
    if (frame === undefined) frame = requestFrame(tick);
  }};
}
