// shortcut: 节点页(上一页/下一页)、数据页(较新/较旧)与通知(加载更多)各建分页控件；
// 形态互不相同，提取共享组件需设计师先统一交互，暂不归并。
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
