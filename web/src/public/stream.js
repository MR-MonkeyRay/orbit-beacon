const MAX_BUFFER = 1024 * 1024;
const encoder = new TextEncoder();

export function createSSEParser() {
  let line = '';
  let event = '';
  let data = [];
  let size = 0;
  let skipLF = false;
  return {
    feed(text) {
      const frames = [];
      const newline = /[\r\n]/g;
      for (let index = 0; index < text.length;) {
        if (skipLF) {
          skipLF = false;
          if (text[index] === '\n') { index += 1; continue; }
        }
        newline.lastIndex = index;
        const end = newline.exec(text)?.index ?? text.length;
        const part = text.slice(index, end);
        size += encoder.encode(part).length;
        if (size > MAX_BUFFER) throw new Error('public stream buffer exceeded');
        line += part;
        index = end;
        if (index === text.length) break;
        skipLF = text[index++] === '\r';
        if (!line) {
          if (data.length) frames.push({event: event || 'message', data: data.join('\n')});
          event = '';
          data = [];
          size = 0;
        } else {
          size += 1;
          if (size > MAX_BUFFER) throw new Error('public stream buffer exceeded');
          const colon = line.indexOf(':');
          const field = colon < 0 ? line : line.slice(0, colon);
          let value = colon < 0 ? '' : line.slice(colon + 1);
          if (value.startsWith(' ')) value = value.slice(1);
          if (field === 'event') event = value;
          else if (field === 'data') data.push(value);
        }
        line = '';
      }
      return frames;
    }
  };
}

export class StreamSetupError extends Error {}

export async function openStream({offset, limit, signal, onFrame}) {
  signal?.throwIfAborted();
  let response;
  try {
    response = await fetch(`/api/public/v1/stream?limit=${limit}&offset=${offset}`, {
      credentials: 'omit', cache: 'no-store', signal, headers: {Accept: 'text/event-stream'}
    });
  } catch (cause) {
    signal?.throwIfAborted();
    throw new StreamSetupError('public stream request failed', {cause});
  }
  const discard = () => { if (response.body) void response.body.cancel().catch(() => {}); };
  if (signal?.aborted) {
    discard();
    signal.throwIfAborted();
  }
  if (!response.ok || response.headers.get('Content-Type')?.split(';', 1)[0].trim().toLowerCase() !== 'text/event-stream' || !response.body) {
    discard();
    throw new StreamSetupError('public stream unavailable');
  }
  const reader = response.body.getReader();
  const parser = createSSEParser();
  const decoder = new TextDecoder('utf-8', {fatal: true});
  const cancel = () => { void reader.cancel().catch(() => {}); };
  signal?.addEventListener('abort', cancel, {once: true});
  const dispatch = (text) => {
    for (const frame of parser.feed(text)) {
      signal?.throwIfAborted();
      onFrame(frame);
      if (frame.event === 'close') return true;
    }
    return false;
  };
  try {
    while (true) {
      signal?.throwIfAborted();
      const {done, value} = await reader.read();
      signal?.throwIfAborted();
      if (dispatch(done ? decoder.decode() : decoder.decode(value, {stream: true}))) return;
      if (done) throw new Error('public stream ended without close');
    }
  } finally {
    signal?.removeEventListener('abort', cancel);
    cancel();
    reader.releaseLock();
  }
}
