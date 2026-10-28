import './public.css';
import {h} from '../shared/dom.js';
import {themeButton} from '../shared/theme.js';
import {createCard, skeletonCards, STATUS} from './cards.js';
import {createConstellation} from './constellation.js';
import {validResponse} from './validate.js';
import {openStream, StreamSetupError} from './stream.js';

const PAGE = 50;
const POLL_MS = 10000;
const TIMEOUT_MS = 8000;
const $ = (id) => document.getElementById(id);
const list = $('nodes');
const empty = $('empty');
const emptyText = $('empty-text');
const retry = $('retry');
const headline = $('headline');
const summary = $('summary');
const updated = $('updated');
const announcement = $('announcement');
const refresh = $('refresh');
const countdown = $('countdown');
const filtersBar = $('filters');
const pagination = $('pagination');
const previous = $('previous');
const next = $('next');
const page = $('page');
const timeFormat = new Intl.DateTimeFormat('zh-CN', {hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false});
const fullFormat = new Intl.DateTimeFormat('zh-CN', {year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false, timeZoneName: 'short'});
const constellation = createConstellation($('constellation'));

let offset = 0;
let nextOffset = null;
let timer;
let controller;
let requestNumber = 0;
let stopped = false;
let cards = [];
let keys = '';
let filter = '';
let current = [];
let loadState = 'initial';
let hasSnapshot = false;

themeButton({system: '跟随系统', light: '浅色', dark: '深色'}, (now, then) => `主题：${now}，切换为${then}`, $('theme'));

// ------------------------------------------------------------------ filters

const filterButtons = new Map(['', 'online', 'offline', 'unknown'].map((status) => {
  const button = h('button', {type: 'button', 'aria-pressed': String(status === ''), onclick: () => { filter = status; applyFilter(); }},
    status ? h('i', {class: `dot ${status}`, 'aria-hidden': 'true'}) : null, status ? STATUS[status] : '全部', h('span', {class: 'num'}));
  filtersBar.append(button);
  return [status, button];
}));

function applyFilter() {
  for (const [status, button] of filterButtons) {
    const count = status ? current.filter((node) => node.status === status).length : current.length;
    button.lastChild.textContent = count;
    button.setAttribute('aria-pressed', String(filter === status));
    button.hidden = Boolean(status) && count === 0 && filter !== status;
  }
  let shown = 0;
  cards.forEach((card, index) => {
    card.hidden = Boolean(filter) && current[index].status !== filter;
    if (!card.hidden) shown += 1;
  });
  filtersBar.hidden = current.length === 0;
  if (current.length && !shown) {
    empty.hidden = false;
    emptyText.textContent = `本页没有${STATUS[filter]}的节点`;
    retry.hidden = true;
  } else if (current.length) empty.hidden = true;
}

// ------------------------------------------------------------------ rendering

function render(result) {
  const nodes = result.nodes;
  const nextKeys = nodes.map((node) => `${node.name}\u0000${node.region}\u0000${node.provider}`).join('\u0001');
  if (nextKeys !== keys || cards.length !== nodes.length) {
    cards = nodes.map(() => createCard());
    cards.forEach((card, index) => {
      card.classList.add('enter');
      card.style.setProperty('--i', String(Math.min(index, 12)));
      card.addEventListener('animationend', () => card.classList.remove('enter'), {once: true});
    });
    list.replaceChildren(...cards);
    keys = nextKeys;
  }
  nodes.forEach((node, index) => cards[index].update(node));
  current = nodes;
  constellation.update(nodes);

  const online = nodes.filter((node) => node.status === 'online').length;
  const offline = nodes.filter((node) => node.status === 'offline').length;
  const unknown = nodes.length - online - offline;
  const partial = offset > 0 || result.has_more;
  if (nodes.length === 0) {
    headline.textContent = partial ? '本页暂无公开节点' : '暂无公开节点';
    summary.textContent = '管理员公开节点后，会在这里显示它们的在线状态和资源使用情况。';
  } else {
    headline.textContent = online === nodes.length ? `${nodes.length} 个节点全部在线` : `${online} / ${nodes.length} 个节点在线`;
    summary.textContent = `${partial ? '本页' : '共'} ${nodes.length} 个公开节点${offline ? `，${offline} 个离线` : ''}${unknown ? `，${unknown} 个状态未知` : ''}。`;
  }
  const generated = new Date(result.generated_at);
  updated.textContent = `最近更新 ${timeFormat.format(generated)}`;
  updated.title = `${fullFormat.format(generated)}（本地时间）`;
  nextOffset = result.next_offset;
  next.disabled = nextOffset === null;
  pagination.hidden = offset === 0 && nextOffset === null;
  empty.hidden = nodes.length !== 0;
  emptyText.textContent = partial ? '本页暂无公开节点' : '暂无公开节点';
  retry.hidden = true;
  applyFilter();
}

function clearData() {
  hasSnapshot = false;
  list.replaceChildren();
  cards = [];
  keys = '';
  current = [];
  constellation.clear();
  headline.textContent = '节点状态';
  summary.textContent = '—';
  updated.textContent = '尚未成功更新';
  updated.removeAttribute('title');
  nextOffset = null;
  next.disabled = true;
  empty.hidden = true;
  filtersBar.hidden = true;
  delete countdown.dataset.state;
}

function restartCountdown(running) {
  countdown.classList.remove('running');
  if (!running) return;
  void countdown.getBoundingClientRect();
  countdown.classList.add('running');
}

// ------------------------------------------------------------------ updates

function cancel() {
  clearTimeout(timer);
  requestNumber += 1;
  if (controller) controller.abort();
  controller = null;
}

function begin(announce) {
  refresh.classList.add('loading');
  refresh.setAttribute('aria-busy', 'true');
  restartCountdown(false);
  if (announce) announcement.textContent = '正在加载节点…';
  list.setAttribute('aria-busy', 'true');
  if (!cards.length) list.replaceChildren(...skeletonCards(6));
  previous.disabled = offset === 0;
  page.textContent = `第 ${Math.floor(offset / PAGE) + 1} 页`;
}

function finish() {
  refresh.classList.remove('loading');
  refresh.removeAttribute('aria-busy');
  list.removeAttribute('aria-busy');
  if (list.querySelector('.placeholder')) list.replaceChildren(...cards);
}

function accept(result, announce, state) {
  if (!validResponse(result, offset)) throw new Error('invalid public response');
  render(result);
  countdown.dataset.state = state;
  announcement.textContent = announce ? `${headline.textContent}。${summary.textContent}` : '';
  loadState = 'ok';
  hasSnapshot = true;
}

function markRetainedStale(message = '连接中断，正在重连；当前显示最后一次数据。') {
  if (!hasSnapshot) return;
  cards.forEach((card) => card.setStale());
  summary.textContent = message;
  countdown.dataset.state = 'stale';
}

function fail(announce) {
  if (hasSnapshot) {
    markRetainedStale();
  } else {
    clearData();
    headline.textContent = '暂时无法获取节点信息';
    summary.textContent = '加载失败，请检查网络后重试。将在 10 秒后自动重试。';
    empty.hidden = false;
    emptyText.textContent = '加载失败，请检查网络后重试。';
    retry.hidden = false;
    updated.textContent = '更新失败';
  }
  countdown.dataset.state = 'error';
  if (announce || loadState !== 'error') announcement.textContent = hasSnapshot ? '连接中断，当前显示最后一次数据。' : '更新失败，请点击刷新重试。';
  loadState = 'error';
}

async function load(announce = false) {
  cancel();
  if (document.hidden || stopped) return;
  const number = requestNumber;
  const active = new AbortController();
  controller = active;
  timer = setTimeout(() => active.abort(), TIMEOUT_MS);
  begin(announce);
  try {
    const response = await fetch(`/api/public/v1/nodes?limit=${PAGE}&offset=${offset}`, {credentials: 'omit', cache: 'no-store', signal: active.signal});
    if (!response.ok) throw new Error('public request failed');
    const result = await response.json();
    if (number !== requestNumber || document.hidden || stopped) return;
    active.signal.throwIfAborted();
    accept(result, announce, 'ok');
  } catch {
    if (number !== requestNumber || document.hidden || stopped) return;
    fail(announce);
  } finally {
    if (number === requestNumber) {
      clearTimeout(timer);
      controller = null;
      finish();
      if (!document.hidden && !stopped) {
        timer = setTimeout(() => load(), POLL_MS);
        restartCountdown(true);
      }
    }
  }
}

async function start(announce = false) {
  cancel();
  if (document.hidden || stopped) return;
  const number = requestNumber;
  const active = new AbortController();
  controller = active;
  let received = false;
  let closed = false;
  let fallback = false;
  let timedOut = false;
  const watch = (delay) => {
    clearTimeout(timer);
    timer = setTimeout(() => { timedOut = true; active.abort(); }, delay);
  };
  watch(TIMEOUT_MS);
  begin(announce);
  try {
    await openStream({offset, limit: PAGE, signal: active.signal, onFrame: ({event, data}) => {
      if (number !== requestNumber || document.hidden || stopped || active.signal.aborted) return;
      const result = JSON.parse(data);
      if (event === 'nodes') {
        accept(result, announce, 'live');
        received = true;
        announce = false;
        finish();
        watch(POLL_MS + TIMEOUT_MS);
      } else if (event === 'close' && result && typeof result === 'object' && !Array.isArray(result) && Object.keys(result).length === 0) closed = true;
      else throw new Error('invalid public frame');
    }});
  } catch (error) {
    if (number !== requestNumber || document.hidden || stopped) return;
    closed = false;
    // Setup failures hand over to polling, which validates and fails on its own.
    fallback = !timedOut && !received && error instanceof StreamSetupError;
    if (!fallback) fail(announce);
    else markRetainedStale();
  } finally {
    if (number === requestNumber) {
      clearTimeout(timer);
      active.abort();
      controller = null;
      finish();
      if (!document.hidden && !stopped) {
        if (fallback) load(announce);
        else if (closed) start();
        else {
          timer = setTimeout(() => start(), POLL_MS);
          restartCountdown(true);
        }
      }
    }
  }
}

function turnPage(target) {
  offset = target;
  filter = '';
  refresh.focus();
  clearData();
  start(true);
}

refresh.addEventListener('click', () => start(true));
retry.addEventListener('click', () => { refresh.focus(); start(true); });
previous.addEventListener('click', () => turnPage(Math.max(0, offset - PAGE)));
next.addEventListener('click', () => { if (nextOffset !== null) turnPage(nextOffset); });
document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    cancel();
    markRetainedStale('更新已暂停；当前显示最后一次数据。');
    restartCountdown(false);
    announcement.textContent = '';
    finish();
  } else { markRetainedStale(); start(); }
});
window.addEventListener('pagehide', () => { stopped = true; cancel(); markRetainedStale('更新已暂停；当前显示最后一次数据。'); restartCountdown(false); finish(); });
window.addEventListener('pageshow', (event) => { if (event.persisted) { stopped = false; markRetainedStale(); start(); } });
start(true);
