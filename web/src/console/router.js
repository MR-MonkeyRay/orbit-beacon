import {h, reducedMotion} from '../shared/dom.js';
import {setNavigationSignal} from './api.js';
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

const hasUnsavedChanges = () => Boolean(view.querySelector('form[data-dirty="true"]'));

export async function go(target, options = {}) {
  const attempt = ++intent;
  if (!options.force && hasUnsavedChanges() && !(await confirmDiscard())) return;
  if (attempt !== intent) return;
  if (target !== location.pathname + location.search + location.hash) {
    history.replaceState({...history.state, scroll: window.scrollY}, '');
    history[options.replace ? 'replaceState' : 'pushState']({scroll: 0}, '', target);
  }
  await render({scroll: 0});
}

// Re-renders the current page in place after a change, keeping scroll and focus.
export const refresh = () => dialogActionStale() ? Promise.resolve() : render({quiet: true});

async function render(options = {}) {
  const token = ++navigation;
  controller?.abort();
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
  let content;
  try {
    resetIDs();
    content = await route.page();
  } catch (error) {
    content = errorPage(error);
  } finally {
    clearTimeout(skeletonTimer);
    if (token === navigation) progress.done();
  }
  if (token !== navigation) return;
  if (typeof content === 'string') { go(content, {replace: true, force: true}); return; }
  await swap(content, route, options.quiet ? 'none' : skeletonShown ? 'fade' : 'transition', token);
  if (token !== navigation) return;
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
  if (mode === 'transition' && document.startViewTransition && !reducedMotion.matches) {
    return document.startViewTransition(update).updateCallbackDone.catch(() => {});
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
      notFound ? linkButton('/nodes', 'Go to nodes') : button('Try again', {icon: 'refresh', variant: 'primary', onClick: () => render()}))));
}

export function startRouter(table) {
  routes = table;
  history.scrollRestoration = 'manual';
  document.addEventListener('click', (event) => {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const anchor = event.target.closest('a[data-link]');
    if (!anchor || anchor.target) return;
    const url = new URL(anchor.href);
    if (url.origin !== location.origin) return;
    event.preventDefault();
    closeMenu();
    go(url.pathname + url.search + url.hash);
  });
  window.addEventListener('popstate', async (event) => {
    const attempt = ++intent;
    const previousURL = currentURL;
    const scroll = window.scrollY;
    if (hasUnsavedChanges() && !(await confirmDiscard())) {
      if (attempt !== intent) return;
      history.pushState({scroll}, '', previousURL);
      window.scrollTo(0, scroll);
      return;
    }
    if (attempt === intent) render({scroll: (event.state && event.state.scroll) || 0});
  });
  // Tabs replace the hash in place; remember it when a form is edited.
  for (const type of ['input', 'change', 'tags']) document.addEventListener(type, () => {
    currentURL = location.pathname + location.search + location.hash;
  });
  window.addEventListener('beforeunload', (event) => { if (hasUnsavedChanges()) event.preventDefault(); });
  render();
}
