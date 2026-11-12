import {translateHTML} from '../shared/translate-html.js';
import {h} from '../shared/dom.js';
import {themeButton} from '../shared/theme.js';
import {validResponse} from './validate.js';
import {connectLive} from '../shared/live.js';
import {getLocale, t} from '../shared/i18n.js';
import {formatFullTimestamp} from '../shared/format.js';
import {localeToggle} from '../shared/locale-select.js';
import {errorText} from '../shared/errors.js';
import {statusLabels} from '../shared/status.js';

export function mountPublicPage({createCard, skeletonCards, createConstellation, createRegionHeading = (region) => h('li', {class: 'region-heading'}, h('h2', null, region))}) {
  const STATUS = statusLabels({unknown: 'public.status.unknown'});
  // 脚本已成功启动，移除脚本加载失败的回退提示。
  document.getElementById('script-error')?.remove();
  translateHTML();

  const PAGE = 50;
  const $ = (id) => document.getElementById(id);
  const list = $('nodes');
  const empty = $('empty');
  const emptyText = $('empty-text');
  const retry = $('retry');
  const headline = $('headline');
  // 标题固定为名词；空集合使用主区域空态。
  headline.textContent = t('public.heading');
  const summary = $('summary');
  summary.setAttribute('role', 'status');
  summary.setAttribute('aria-live', 'polite');
  summary.setAttribute('aria-atomic', 'true');
  const setSummary = (text, tone = '') => {
    summary.dataset.tone = tone;
    if (summary.textContent !== text) summary.textContent = text;
  };
  const updated = $('updated');
  const updateTime = h('span', {class: 'update-time'}, updated.textContent);
  updated.replaceChildren(updateTime);
  const shortTime = new Intl.DateTimeFormat(getLocale(), {hour: '2-digit', minute: '2-digit', hourCycle: 'h23'});
  const announcement = $('announcement');
  const refresh = $('refresh');
  refresh.title = t('common.refresh');
  const countdown = $('countdown');
  const filtersBar = $('filters');
  const pagination = $('pagination');
  const previous = $('previous');
  const next = $('next');
  const page = $('page');
  const constellationRoot = $('constellation');
  const constellation = createConstellation(constellationRoot);

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
  let query = '';
  const headings = new Map();
  const search = h('input', {type: 'search', class: 'node-search', hidden: true, 'aria-label': t('public.search'), placeholder: t('public.search'), oninput: () => { query = search.value.trim().toLocaleLowerCase(); applyFilter(); }});
  filtersBar.parentElement.insertBefore(search, filtersBar);

  themeButton($('theme'));
  const localeError = $('locale-error');
  $('top-actions').prepend(localeToggle({onError(error) {
    localeError.textContent = errorText(error);
    localeError.hidden = false;
  }}));

  // ------------------------------------------------------------------ 筛选

  const filterButtons = new Map(['', 'online', 'offline', 'unknown'].map((status) => {
    const button = h('button', {type: 'button', 'aria-pressed': String(status === ''), onclick: () => { filter = status; applyFilter(); }},
      status ? h('i', {class: `dot ${status}`, 'aria-hidden': 'true'}) : null, status ? STATUS[status] : t('public.filter.all'), h('span', {class: 'num'}));
    filtersBar.append(button);
    return [status, button];
  }));
  // 右缘渐隐只在筛选条真的可滚动时启用，否则最后一个按钮会被无谓遮暗。
  const syncFade = () => filtersBar.classList.toggle('fade', filtersBar.scrollWidth > filtersBar.clientWidth);
  if (typeof ResizeObserver === 'function') new ResizeObserver(syncFade).observe(filtersBar);

  function applyFilter() {
    for (const [status, button] of filterButtons) {
      const count = status ? current.filter((node) => node.status === status).length : current.length;
      button.lastChild.textContent = count;
      button.setAttribute('aria-pressed', String(filter === status));
      button.hidden = Boolean(status) && count === 0 && filter !== status;
    }
    let shown = 0;
    cards.forEach((card, index) => {
      const node = current[index];
      const matchesSearch = current.length <= 12 || !query || [node.name, node.region, node.provider].some((value) => value.toLocaleLowerCase().includes(query));
      card.hidden = Boolean(filter) && node.status !== filter || !matchesSearch;
      if (!card.hidden) shown += 1;
    });
    search.hidden = current.length <= 12;
    const ordered = [];
    if (current.length > 12) {
      const regions = new Map();
      current.forEach((node, index) => {
        const region = node.region || t('public.regionUnknown');
        if (!regions.has(region)) regions.set(region, []);
        regions.get(region).push(cards[index]);
      });
      for (const region of headings.keys()) if (!regions.has(region)) headings.delete(region);
      for (const [region, members] of regions) {
        if (!headings.has(region)) headings.set(region, createRegionHeading(region));
        const heading = headings.get(region);
        heading.hidden = members.every((card) => card.hidden);
        ordered.push(heading, ...members);
      }
    } else ordered.push(...cards);
    const retained = new Set(ordered);
    for (const child of [...list.children]) if (!retained.has(child)) child.remove();
    ordered.forEach((child, index) => { if (list.children[index] !== child) list.insertBefore(child, list.children[index] || null); });
    filtersBar.hidden = current.length === 0;
    syncFade();
    if (current.length && !shown) {
      empty.hidden = false;
      emptyText.textContent = t(current.length > 12 && query ? 'public.searchEmpty' : `public.filter.empty.${filter}`);
      retry.hidden = true;
    } else if (current.length) empty.hidden = true;
  }

  // ------------------------------------------------------------------ 渲染

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
    // 没有节点时星座图只剩空轨道，与居中空态重复。
    constellationRoot.hidden = nodes.length === 0;

    // 正常状态写摘要；空集合在剩余主区域显示。
    const online = nodes.filter((node) => node.status === 'online').length;
    const partial = offset > 0 || result.has_more;
    if (nodes.length === 0) {
      setSummary('');
      emptyText.textContent = t(partial ? 'public.empty.page' : 'public.empty.all');
    } else {
      const offline = nodes.filter((node) => node.status === 'offline').length;
      const unknown = nodes.length - online - offline;
      const status = online === nodes.length ? t('public.summary.allOnline', {count: nodes.length})
        : [offline ? t('public.summary.offline', {count: offline}) : '', unknown ? t('public.summary.unknown', {count: unknown}) : ''].filter(Boolean).join(' · ');
      setSummary(partial ? t('public.summary.joined', {status, page: t('public.summary.page', {count: nodes.length})}) : status,
        online === nodes.length ? 'ok' : offline === nodes.length ? 'bad' : 'warn');
    }
    lastGenerated = new Date(result.generated_at);
    updateText();
    nextOffset = result.next_offset;
    next.disabled = nextOffset === null;
    pagination.hidden = !nodes.length || offset === 0 && nextOffset === null;
    empty.hidden = nodes.length > 0;
    retry.hidden = true;
    applyFilter();
  }

  function clearData() {
    hasSnapshot = false;
    list.replaceChildren();
    cards = [];
    keys = '';
    current = [];
    headings.clear();
    query = ''; search.value = ''; search.hidden = true;
    constellation.clear();
    headline.textContent = t('public.heading');
    // 摘要区回到加载文案，避免显示未本地化的破折号。
    setSummary(t('public.loading'));
    lastGenerated = undefined;
    updateText();
    nextOffset = null;
    next.disabled = true;
    empty.hidden = true;
    filtersBar.hidden = true;
    pagination.hidden = true;
    delete countdown.dataset.state;
  }

  function restartCountdown(running, milliseconds = 0) {
    countdown.classList.remove('running');
    if (!running) return;
    countdown.style.setProperty('--refresh-duration', `${milliseconds}ms`);
    void countdown.getBoundingClientRect();
    countdown.classList.add('running');
  }

  // ------------------------------------------------------------------ 更新

  function begin() {
    refresh.classList.add('loading');
    refresh.setAttribute('aria-busy', 'true');
    list.setAttribute('aria-busy', 'true');
    if (!cards.length) list.replaceChildren(...skeletonCards(6));
    previous.disabled = offset === 0;
    const pageNumber = Math.floor(offset / PAGE) + 1;
    page.textContent = t('public.page', {page: pageNumber});
    // 翻页后浏览器标签也标明当前页，避免多个标签无法区分。
    document.title = pageNumber > 1 ? `${t('public.page', {page: pageNumber})} · ${t('public.title')}` : t('public.title');
  }

  function finish() {
    refresh.classList.remove('loading');
    refresh.removeAttribute('aria-busy');
    list.removeAttribute('aria-busy');
    if (list.querySelector('.placeholder')) list.replaceChildren(...cards);
  }

  function updateText({stale = false, seconds = null} = {}) {
    updateTime.textContent = stale ? t('public.stale') : lastGenerated
      ? t('public.updatedAt', {time: shortTime.format(lastGenerated)}) : t('public.notUpdated');
    const details = [lastGenerated ? t('public.localTime', {time: formatFullTimestamp(lastGenerated)}) : '',
      seconds !== null ? t('public.refreshIn', {count: seconds}) : ''].filter(Boolean).join(' — ');
    updated.title = details;
    countdown.parentElement.title = details;
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
        updateText({seconds: state.transport === 'http' && state.retryAt !== null ? seconds : null});
        if (seconds > 0) displayTimer = setTimeout(paint, Math.min(1000, remaining));
      };
      paint();
      if (state.transport === 'http' && deadline !== null) restartCountdown(true, Math.max(0, deadline - performance.now()));
    } else if (state.state === 'stale') {
      finish();
      countdown.dataset.state = 'stale';
      if (hasSnapshot) {
        cards.forEach((card) => card.setStale());
        setSummary(t(document.hidden ? 'public.paused' : 'public.reconnecting'));
        updateText({stale: true});
      } else {
        list.replaceChildren();
        setSummary(t('public.loadFailed'));
        empty.hidden = false;
        emptyText.textContent = t('public.loadFailed');
        retry.hidden = false;
        filtersBar.hidden = true;
        pagination.hidden = true;
        updateText({stale: true});
      }
    } else {
      countdown.dataset.state = 'connecting';
    }
    // 传输变化、心跳和 HTTP 倒计时不属于需要播报的事件。
    if (announcedState !== state.state) {
      announcedState = state.state;
      announcement.textContent = state.state === 'live' ? ''
        : state.state === 'connecting' ? t('public.loading')
          : t(hasSnapshot ? 'public.announce.stale' : 'public.loadFailed');
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
}
