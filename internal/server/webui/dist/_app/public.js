'use strict';

const list = document.getElementById('nodes');
const empty = document.getElementById('empty');
const count = document.getElementById('count');
const updated = document.getElementById('updated');
const announcement = document.getElementById('announcement');
const refresh = document.getElementById('refresh');
const previous = document.getElementById('previous');
const next = document.getElementById('next');
const page = document.getElementById('page');
const timeFormat = new Intl.DateTimeFormat('zh-CN', {month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false, timeZoneName: 'short'});
let offset = 0;
let nextOffset = null;
let timer;
let controller;
let requestNumber = 0;
let stopped = false;

function text(tag, value, className) {
  const element = document.createElement(tag);
  element.textContent = value;
  if (className) element.className = className;
  return element;
}

function percent(label, value) {
  const section = document.createElement('div');
  section.className = 'resource';
  const line = document.createElement('div');
  line.className = 'metric-line';
  line.append(text('span', label), text('span', value === null ? '—' : `${value.toFixed(1)}%`, 'metric-value'));
  section.append(line);
  if (value === null) {
    section.append(text('p', '暂无有效数据', 'unavailable'));
  } else {
    const meter = document.createElement('meter');
    meter.min = 0;
    meter.max = 100;
    meter.value = value;
    meter.setAttribute('aria-label', label);
    section.append(meter);
  }
  return section;
}

function duration(seconds) {
  if (seconds === null) return '—';
  const minutes = Math.floor(seconds / 60);
  return `${Math.floor(minutes / 1440)}天 ${Math.floor(minutes % 1440 / 60)}小时 ${minutes % 60}分钟`;
}

function card(node) {
  const item = document.createElement('li');
  const article = document.createElement('article');
  article.className = 'card';
  const head = document.createElement('div');
  head.className = 'card-head';
  const heading = text('h2', node.name);
  const statuses = {online: '在线', offline: '离线', unknown: '状态未知'};
  const badge = text('span', statuses[node.status], `badge ${node.status}`);
  head.append(heading, badge);
  article.append(head, text('p', [node.region, node.provider].filter(Boolean).join(' · '), 'location'));
  article.append(percent('CPU', node.cpu_percent), percent('内存', node.memory_percent));
  const details = document.createElement('dl');
  details.className = 'details';
  for (const [label, value] of [['1分钟负载', node.load1 === null ? '—' : node.load1.toFixed(2)], ['运行时长', duration(node.uptime_seconds)]]) {
    const entry = document.createElement('div');
    entry.append(text('dt', label), text('dd', value));
    details.append(entry);
  }
  article.append(details);
  item.append(article);
  return item;
}

function clearData() {
  list.replaceChildren();
  count.textContent = '本页节点数：—';
  updated.textContent = '尚未成功更新';
  nextOffset = null;
  next.disabled = true;
  empty.hidden = true;
}

function cancel() {
  clearTimeout(timer);
  requestNumber += 1;
  if (controller) controller.abort();
  controller = null;
}

// A malformed response is a failure, never an invitation to display stale data.
function validResponse(result) {
  if (!result || !Array.isArray(result.nodes) || result.nodes.length > 100 || typeof result.has_more !== 'boolean' || !Number.isFinite(Date.parse(result.generated_at))) return false;
  if (result.has_more ? !Number.isSafeInteger(result.next_offset) || result.next_offset <= offset : result.next_offset !== null) return false;
  return result.nodes.every((node) => node && ['name', 'region', 'provider'].every((key) => typeof node[key] === 'string') && ['online', 'offline', 'unknown'].includes(node.status) && ['cpu_percent', 'memory_percent', 'load1', 'uptime_seconds'].every((key) => node[key] === null || typeof node[key] === 'number' && Number.isFinite(node[key]) && node[key] >= 0 && (!key.endsWith('_percent') || node[key] <= 100)));
}

async function load() {
  cancel();
  if (document.hidden || stopped) return;
  const number = requestNumber;
  const active = new AbortController();
  controller = active;
  const timeout = setTimeout(() => active.abort(), 8000);
  refresh.textContent = '刷新中…';
  announcement.textContent = '正在加载节点…';
  list.setAttribute('aria-busy', 'true');
  previous.disabled = offset === 0;
  page.textContent = `第 ${Math.floor(offset / 50) + 1} 页`;
  try {
    const response = await fetch(`/api/public/v1/nodes?limit=50&offset=${offset}`, {credentials: 'omit', cache: 'no-store', signal: active.signal});
    if (!response.ok) throw new Error('public request failed');
    const result = await response.json();
    if (number !== requestNumber || document.hidden || stopped) return;
    if (!validResponse(result)) throw new Error('invalid public response');
    const fragment = document.createDocumentFragment();
    for (const node of result.nodes) fragment.append(card(node));
    list.replaceChildren(fragment);
    count.textContent = `本页 ${result.nodes.length} 个节点`;
    updated.textContent = `最近更新：${timeFormat.format(new Date(result.generated_at))}（本地时间）`;
    nextOffset = result.next_offset;
    next.disabled = nextOffset === null;
    empty.hidden = result.nodes.length !== 0;
    empty.textContent = offset === 0 && !result.has_more ? '暂无公开节点' : '本页暂无公开节点';
    // Routine polling does not announce every changing resource value.
    announcement.textContent = '';
  } catch (error) {
    if (number !== requestNumber || document.hidden || stopped) return;
    clearData();
    empty.hidden = false;
    empty.textContent = '暂时无法获取节点信息';
    announcement.textContent = '更新失败，请点击刷新重试。';
    updated.textContent = '更新失败';
  } finally {
    clearTimeout(timeout);
    if (number === requestNumber) {
      controller = null;
      refresh.textContent = '刷新';
      list.removeAttribute('aria-busy');
      if (!document.hidden && !stopped) timer = setTimeout(load, 10000);
    }
  }
}

refresh.addEventListener('click', load);
previous.addEventListener('click', () => {
  offset = Math.max(0, offset - 50);
  refresh.focus();
  clearData();
  load();
});
next.addEventListener('click', () => {
  if (nextOffset !== null) {
    offset = nextOffset;
    refresh.focus();
    clearData();
    load();
  }
});
document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    cancel();
    clearData();
    announcement.textContent = '';
    list.removeAttribute('aria-busy');
  } else load();
});
window.addEventListener('pagehide', () => { stopped = true; cancel(); clearData(); });
window.addEventListener('pageshow', (event) => { if (event.persisted) { stopped = false; load(); } });
load();
