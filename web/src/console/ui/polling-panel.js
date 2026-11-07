export function panelLifecycle({root, signal, load, cancel, poll = true, shouldPoll = () => true, resume = true, loadWhenHidden = false, interval = 60000}) {
  let disposed = false;
  let attached = false;
  let active = false;
  let timer;

  const stop = () => {
    cancel?.();
    clearInterval(timer);
    timer = undefined;
    active = false;
  };
  const start = (resuming = false) => {
    if (disposed || signal?.aborted) return;
    active = true;
    syncPolling();
    if (!resuming || loadWhenHidden || !document.hidden) load();
  };
  const syncPolling = () => {
    if (!active || disposed || signal?.aborted) return;
    if (!shouldPoll()) { clearInterval(timer); timer = undefined; return; }
    if (poll && timer === undefined) timer = setInterval(() => {
      if (!root.isConnected) { stop(); return; }
      if (!shouldPoll()) { clearInterval(timer); timer = undefined; return; }
      if (!document.hidden) load();
    }, interval);
  };
  const dispose = () => {
    if (disposed) return;
    disposed = true;
    stop();
    observer?.disconnect();
    signal?.removeEventListener('abort', dispose);
  };
  const observer = typeof MutationObserver === 'function' ? new MutationObserver(() => {
    if (disposed || signal?.aborted) return;
    if (root.isConnected) {
      attached = true;
      if (resume && !active) start(true);
    } else if (attached) {
      if (resume) stop();
      else dispose();
    }
  }) : null;
  observer?.observe(document.body, {childList: true, subtree: true});
  signal?.addEventListener('abort', dispose, {once: true});
  if (signal?.aborted) dispose();
  else start();
  return {dispose, syncPolling, get disposed() { return disposed; }};
}
