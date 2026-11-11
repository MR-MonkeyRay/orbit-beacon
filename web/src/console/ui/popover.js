// 非模态浮层共用的关闭逻辑；打开与焦点规则由各自调用方负责。
export function dismissPopover(root, {trigger, panel, signal, closeOnTab = false}) {
  const close = () => {
    panel.hidden = true;
    trigger.setAttribute('aria-expanded', 'false');
  };
  const outside = (event) => { if (!root.contains(event.target)) close(); };
  const keydown = (event) => {
    if (panel.hidden) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      close();
      trigger.focus();
    } else if (closeOnTab && event.key === 'Tab') { trigger.focus(); close(); }
  };
  const focusout = (event) => { if (!root.contains(event.relatedTarget)) close(); };
  root.addEventListener('keydown', keydown);
  root.addEventListener('focusout', focusout);
  document.addEventListener('pointerdown', outside);
  let mounted = root.isConnected;
  const observer = new MutationObserver(() => {
    if (root.isConnected) mounted = true;
    else if (mounted) dispose();
  });
  const dispose = () => {
    close();
    observer.disconnect();
    document.removeEventListener('pointerdown', outside);
    root.removeEventListener('keydown', keydown);
    root.removeEventListener('focusout', focusout);
    signal?.removeEventListener('abort', dispose);
  };
  observer.observe(document.body, {childList: true, subtree: true});
  signal?.addEventListener('abort', dispose, {once: true});
  if (signal?.aborted) dispose();
  return close;
}
