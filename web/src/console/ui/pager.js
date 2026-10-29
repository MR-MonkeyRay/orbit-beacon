export function nextCursor(page, current = null, used = null) {
  if (!page.has_more) return null;
  if (!page.next_cursor || page.next_cursor === current || used?.has(page.next_cursor)) {
    throw Object.assign(new Error('Invalid pagination cursor'), {code: 'unreadable_response'});
  }
  return page.next_cursor;
}

export function cursorPath(path, limit, cursor = null) {
  return `${path}${path.includes('?') ? '&' : '?'}limit=${limit}${cursor ? `&before=${encodeURIComponent(cursor)}` : ''}`;
}
