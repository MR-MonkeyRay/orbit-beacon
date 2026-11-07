const MAX_ERROR_BYTES = 64 * 1024;

// 错误响应体不可信，也可能一直无法读完。传输层负责超时控制，
// 取消操作也必须中断尚未完成的响应体读取。
export async function responseErrorCode(response, signal) {
  if (response.status !== 403 || !response.body) return '';
  const reader = response.body.getReader();
  const cancel = () => { void reader.cancel().catch(() => {}); };
  signal?.addEventListener('abort', cancel, {once: true});
  try {
    const decoder = new TextDecoder('utf-8', {fatal: true});
    let size = 0;
    let text = '';
    for (;;) {
      signal?.throwIfAborted();
      const {done, value} = await reader.read();
      signal?.throwIfAborted();
      if (done) break;
      size += value.byteLength;
      if (size > MAX_ERROR_BYTES) return '';
      text += decoder.decode(value, {stream: true});
    }
    const payload = JSON.parse(text + decoder.decode());
    return payload && !Array.isArray(payload) && typeof payload.code === 'string' ? payload.code : '';
  } catch {
    signal?.throwIfAborted();
    return '';
  } finally {
    signal?.removeEventListener('abort', cancel);
    cancel();
    reader.releaseLock();
  }
}
