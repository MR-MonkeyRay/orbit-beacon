import {h, reducedMotion} from '../shared/dom.js';
import {onSession, pauseSessionVerification, session, setNavigationSignal, verifySession} from './api.js';
import {closeMenu, setActiveNav} from './shell.js';
import {button, emptyState, linkButton, pageHead, resetIDs, skeleton} from './ui/components.js';
import {closeDialogs, confirmDiscard, dialogActionStale} from './ui/dialogs.js';
import {progress} from './ui/feedback.js';

// History-API router. A page is an async function returning an element (or a
// path string to redirect). Data loads before the swap; a skeleton appears only
// when loading is slow, and the swap uses a view transition where supported.
const view = document.getElementById('view');
let routes = [];
let navigation = 0;
let controller;
let currentURL = location.pathname + location.search + location.hash;
let intent = 0;
let transition;

class NavigationCancelled extends DOMException {
  constructor() { super('The navigation was cancelled.', 'AbortError'); }
}

// Event-driven navigation has no awaiting action. Consume only cancellation
// created by this router; genuine loading/transition errors stay observable.
export function ignoreNavigationCancellation(error) {
  if (!(error instanceof NavigationCancelled)) throw error;
}

const hasUnsavedChanges = () => Boolean(view.querySelector('form[data-dirty="true"]'));

function skipTransition(hidden = false) {
  if (!transition) return;
  transition.skipped = true;
  transition.hidden ||= hidden;
  transition.value.skipTransition();
}

export async function go(target, options = {}) {
  const attempt = ++intent;
  if (!options.force && hasUnsavedChanges() && !(await confirmDiscard())) throw new NavigationCancelled();
  if (attempt !== intent) throw new NavigationCancelled();
  if (target !== location.pathname + location.search + location.hash) {
    history.replaceState({...history.state, scroll: window.scrollY}, '');
    history[options.replace ? 'replaceState' : 'pushState']({scroll: 0}, '', target);
  }
  await render({scroll: 0});
}

// Re-renders the current page in place after a change, keeping scroll and focus.
export const refresh = () => dialogActionStale() ? Promise.reject(new NavigationCancelled()) : render({quiet: true});

async function render(options = {}) {
  const token = ++navigation;
  controller?.abort();
  skipTransition();
  controller = new AbortController();
  setNavigationSignal(controller.signal);
  const openerID = closeDialogs();
  currentURL = location.pathname + location.search + location.hash;
  const route = routes.find((item) => item.match(location.pathname)) || routes.find((item) => item.fallback);
  const focusID = options.quiet ? openerID || document.activeElement?.id : '';
  progress.start();
  let skeletonShown = false;
  const skeletonTimer = options.quiet ? 0 : setTimeout(() => {
    if (token !== navigation) return;
    skeletonShown = true;
    swap(skeleton(), route, 'fade', token);
  }, 160);
  const loadingController = controller;
  const cancelSkeleton = () => clearTimeout(skeletonTimer);
  loadingController.signal.addEventListener('abort', cancelSkeleton, {once: true});
  let content;
  try {
    resetIDs();
    content = await route.page();
  } catch (error) {
    if (token !== navigation) throw new NavigationCancelled();
    content = errorPage(error);
  } finally {
    clearTimeout(skeletonTimer);
    loadingController.signal.removeEventListener('abort', cancelSkeleton);
    if (token === navigation) progress.done();
  }
  if (token !== navigation) throw new NavigationCancelled();
  if (typeof content === 'string') return go(content, {replace: true, force: true});
  await swap(content, route, options.quiet ? 'none' : skeletonShown ? 'fade' : 'transition', token);
  if (token !== navigation) throw new NavigationCancelled();
  if (options.quiet) {
    const previous = focusID && document.getElementById(focusID);
    const target = previous && !previous.disabled ? previous : view.querySelector('h1');
    if (target) target.focus({preventScroll: true});
    if (document.activeElement !== target) view.querySelector('h1')?.focus({preventScroll: true});
    return;
  }
  if (options.scroll !== undefined) window.scrollTo(0, options.scroll);
  const target = view.querySelector('[data-autofocus]') || view.querySelector('h1');
  if (target) target.focus({preventScroll: true});
}

function swap(content, route, mode, token) {
  const update = () => {
    if (token !== navigation) return;
    document.documentElement.classList.toggle('route-login', Boolean(route.bare));
    setActiveNav(route.nav);
    view.replaceChildren(content);
    const heading = content.querySelector('h1');
    document.title = heading ? `${heading.textContent} – OrbitBeacon` : 'OrbitBeacon console';
  };
  if (mode === 'transition' && !document.hidden && document.startViewTransition && !reducedMotion.matches) {
    const current = {value: document.startViewTransition(update), skipped: false, hidden: false};
    transition = current;
    const ready = current.value.ready.catch((error) => {
      // Explicit skips reject with AbortError; a document becoming hidden can
      // instead make the browser skip with InvalidStateError. Other failures
      // (including duplicate transition names) remain navigation errors.
      if (current.skipped && error.name === 'AbortError' || current.hidden && error.name === 'InvalidStateError') return;
      throw error;
    });
    return Promise.all([current.value.updateCallbackDone, ready]);
  }
  update();
  if (mode !== 'none') {
    content.classList.add('enter');
    content.addEventListener('animationend', () => content.classList.remove('enter'), {once: true});
  }
  return Promise.resolve();
}

function errorPage(error) {
  const notFound = error.status === 404;
  return h('div', null, pageHead(notFound ? 'Not found' : 'This page could not load'),
    h('div', {class: 'panel'}, emptyState(
      notFound ? 'This resource does not exist' : 'The console could not reach the server',
      notFound ? 'It may have been deleted, or the link is incorrect.' : `${error.message}${error.requestID ? ` (request ${error.requestID})` : ''}`,
      notFound ? linkButton('/nodes', 'Go to nodes') : button('Try again', {icon: 'refresh', variant: 'primary', onClick: () => { void render().catch(ignoreNavigationCancellation); }}))));
}

export function startRouter(table) {
  routes = table;
  const lifetime = new AbortController();
  const listen = (target, type, listener) => target.addEventListener(type, listener, {signal: lifetime.signal});
  const unsubscribe = onSession((current, detail) => {
    if (!detail.boundary || (!detail.previousUser && !['expired', 'unauthenticated', 'logout', 'password', 'deleted'].includes(detail.reason))) return;
    if (!detail.previousUser && location.pathname === '/login') return;
    // Security boundaries are synchronous, outside view transitions and dirty
    // prompts. A retained private view or body dialog must not survive expiry.
    navigation += 1;
    intent += 1;
    controller?.abort();
    skipTransition();
    closeDialogs();
    document.getElementById('toasts')?.replaceChildren();
    view.replaceChildren();
    const target = current.user ? location.pathname + location.search + location.hash
      : detail.reason === 'password' ? '/login?password-changed' : '/login';
    if (current.user && location.pathname === '/login') return;
    void go(target, {replace: true, force: true}).catch(ignoreNavigationCancellation);
  });
  history.scrollRestoration = 'manual';
  listen(document, 'click', (event) => {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const anchor = event.target.closest('a[data-link]');
    if (!anchor || anchor.target) return;
    const url = new URL(anchor.href);
    if (url.origin !== location.origin) return;
    event.preventDefault();
    closeMenu();
    void go(url.pathname + url.search + url.hash).catch(ignoreNavigationCancellation);
  });
  listen(window, 'popstate', async (event) => {
    const attempt = ++intent;
    const previousURL = currentURL;
    const scroll = window.scrollY;
    if (hasUnsavedChanges() && !(await confirmDiscard())) {
      if (attempt !== intent) return;
      history.pushState({scroll}, '', previousURL);
      window.scrollTo(0, scroll);
      return;
    }
    if (attempt === intent) void render({scroll: (event.state && event.state.scroll) || 0}).catch(ignoreNavigationCancellation);
  });
  // Tabs replace the hash in place; remember it when a form is edited.
  for (const type of ['input', 'change', 'tags']) listen(document, type, () => {
    currentURL = location.pathname + location.search + location.hash;
  });
  const revalidate = () => {
    if (location.pathname === '/login' || !session.user || document.hidden) return;
    void verifySession({signal: lifetime.signal}).catch(() => { /* network failure retains the known, expiring snapshot */ });
  };
  const hide = () => { skipTransition(true); pauseSessionVerification(); };
  listen(document, 'visibilitychange', () => { if (document.hidden) hide(); else revalidate(); });
  listen(window, 'pagehide', hide);
  listen(window, 'pageshow', revalidate);
  listen(window, 'beforeunload', (event) => { if (hasUnsavedChanges()) event.preventDefault(); });
  void render().catch(ignoreNavigationCancellation);
  return () => {
    lifetime.abort();
    unsubscribe();
    navigation += 1;
    intent += 1;
    controller?.abort();
    skipTransition();
    closeDialogs();
    progress.done();
  };
}
