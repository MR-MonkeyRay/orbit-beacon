import './public.css';
import {h} from '../shared/dom.js';
import {themeButton} from '../shared/theme.js';
import {createCard, skeletonCards, STATUS} from './cards.js';
import {createConstellation} from './constellation.js';
import {validResponse} from './validate.js';
import {connectLive} from '../shared/live.js';

const PAGE = 50;
const $ = (id) => document.getElementById(id);
const list = $('nodes');
const empty = $('empty');
const emptyText = $('empty-text');
const retry = $('retry');
const headline = $('headline');
const summary = $('summary');
const updated = $('updated');
const updateTime = h('span', {class: 'update-time'}, updated.textContent);
const updateMode = h('span', {class: 'update-mode'});
updated.replaceChildren(updateTime, updateMode);
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
let connection;
let displayTimer;
let lastGenerated;
let announcedState;
let cards = [];
let keys = '';
let filter = '';
let current = [];
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
    headline.textContent = partial ? '本页暂无公开的节点。' : '暂无公开的节点。';
    summary.textContent = '管理员公开节点后，会在这里显示它们的在线状态和资源使用情况。';
  } else {
    headline.textContent = online === nodes.length ? `${nodes.length} 个节点全部在线` : `${online} / ${nodes.length} 个节点在线`;
    summary.textContent = `${partial ? '本页' : '共'} ${nodes.length} 个公开节点${offline ? `，${offline} 个离线` : ''}${unknown ? `，${unknown} 个状态未知` : ''}。`;
  }
  lastGenerated = new Date(result.generated_at);
  updateText(`更新于 ${timeFormat.format(lastGenerated)}`, '自动更新');
  updated.title = `${fullFormat.format(lastGenerated)}（本地时间）`;
  nextOffset = result.next_offset;
  next.disabled = nextOffset === null;
  pagination.hidden = offset === 0 && nextOffset === null;
  empty.hidden = nodes.length !== 0;
  emptyText.textContent = partial ? '本页暂无公开的节点。' : '暂无公开的节点。';
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
  updateText('尚未成功更新');
  updated.removeAttribute('title');
  nextOffset = null;
  next.disabled = true;
  empty.hidden = true;
  filtersBar.hidden = true;
  delete countdown.dataset.state;
}

function restartCountdown(running, milliseconds = 0) {
  countdown.classList.remove('running');
  if (!running) return;
  countdown.style.setProperty('--refresh-duration', `${milliseconds}ms`);
  void countdown.getBoundingClientRect();
  countdown.classList.add('running');
}

// ------------------------------------------------------------------ updates

function begin() {
  refresh.classList.add('loading');
  refresh.setAttribute('aria-busy', 'true');
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

function updateText(time, mode = '') {
  updateTime.textContent = mode ? `${time} ·` : time;
  updateMode.textContent = mode ? ` ${mode}` : '';
}

function paintConnection(state) {
  clearTimeout(displayTimer);
  restartCountdown(false);
  if (state.state === 'live') {
    finish();
    countdown.dataset.state = state.transport === 'http' ? 'ok' : 'live';
    const deadline = state.retryAt === null ? null : performance.now() + Math.max(0, state.retryAt - Date.now());
    const paint = () => {
      const remaining = deadline === null ? 0 : Math.max(0, deadline - performance.now());
      const seconds = Math.ceil(remaining / 1000);
      updateText(`更新于 ${timeFormat.format(lastGenerated)}`, state.transport === 'http' && state.retryAt !== null ? `${seconds} 秒后刷新` : '自动更新');
      if (seconds > 0) displayTimer = setTimeout(paint, Math.min(1000, remaining));
    };
    paint();
    if (state.transport === 'http' && deadline !== null) restartCountdown(true, Math.max(0, deadline - performance.now()));
  } else if (state.state === 'stale') {
    finish();
    countdown.dataset.state = 'stale';
    if (hasSnapshot) {
      cards.forEach((card) => card.setStale());
      summary.textContent = document.hidden ? '更新已暂停；当前显示最后一次数据。' : '连接中断，正在重连；当前显示最后一次数据。';
      updateText(`更新于 ${timeFormat.format(lastGenerated)}`, '数据可能已过期');
    } else {
      list.replaceChildren();
      headline.textContent = '暂时无法获取节点信息';
      summary.textContent = '加载失败，请检查网络后重试。';
      empty.hidden = false;
      emptyText.textContent = '加载失败，请检查网络后重试。';
      retry.hidden = false;
      updateText('尚未成功更新');
    }
  } else {
    countdown.dataset.state = 'connecting';
  }
  // Transport changes, heartbeats and HTTP countdown ticks are not announcements.
  if (announcedState !== state.state) {
    announcedState = state.state;
    announcement.textContent = state.state === 'live' ? '节点数据已更新，正在自动更新。'
      : state.state === 'connecting' ? '正在加载节点…'
        : hasSnapshot ? '连接中断，当前显示最后一次数据。' : '加载失败，请检查网络后重试。';
  }
}

function start() {
  connection?.close();
  clearTimeout(displayTimer);
  restartCountdown(false);
  begin();
  const pageOffset = offset;
  const query = `?limit=${PAGE}&offset=${pageOffset}`;
  connection = connectLive({
    urls: {ws: `/api/public/v1/ws${query}`, sse: `/api/public/v1/stream${query}`, http: `/api/public/v1/nodes${query}`},
    credentials: 'omit',
    validateSnapshot: (data) => validResponse(data, pageOffset),
    onSnapshot(data) {
      hasSnapshot = true;
      render(data);
      finish();
    },
    onState: paintConnection,
    onPublicPageDisabled() {
      connection?.close();
      clearTimeout(displayTimer);
      restartCountdown(false);
      clearData();
      finish();
      location.replace('/');
    }
  });
}

function turnPage(target) {
  connection?.close();
  clearTimeout(displayTimer);
  offset = target;
  filter = '';
  refresh.focus();
  clearData();
  lastGenerated = undefined;
  start();
}

function refreshConnection() {
  begin();
  connection.refresh();
}

refresh.addEventListener('click', refreshConnection);
retry.addEventListener('click', () => { refresh.focus(); refreshConnection(); });
previous.addEventListener('click', () => turnPage(Math.max(0, offset - PAGE)));
next.addEventListener('click', () => { if (nextOffset !== null) turnPage(nextOffset); });
start();
