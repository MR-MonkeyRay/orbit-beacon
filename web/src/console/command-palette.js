import {h} from '../shared/dom.js';
import {errorText} from '../shared/errors.js';
import {t} from './i18n.js';
import {getNavigationSignal, onSession, readNodes, session} from './api.js';
import {alertTabs, consoleRoutes, matchesConsoleRoute, settingsTabs} from './routes.js';
import {go, ignoreNavigationCancellation} from './router.js';
import {createNodeDialog} from './lib/add-node-dialog.js';
import {input} from './ui/forms.js';
import {listbox} from './ui/listbox.js';
import {modal} from './ui/dialogs.js';

const available = () => Boolean(session.user && !session.user.must_change_password)
  && !consoleRoutes.some((route) => route.bare && matchesConsoleRoute(route, location.pathname));
const hasDialog = () => [...document.querySelectorAll('dialog')].some((dialog) => dialog.open);

export function openCommandPalette() {
  if (!available() || hasDialog()) return;
  const navigation = getNavigationSignal();
  if (navigation?.aborted) return;
  const commands = new Map();
  const choices = [];
  for (const route of consoleRoutes.filter((route) => route.label)) {
    commands.set(route.path, () => go(route.path).catch(ignoreNavigationCancellation));
    choices.push([route.path, t(route.label)]);
    const tabs = route.page === 'alerts' ? alertTabs : route.page === 'settings' ? settingsTabs : [];
    for (const tab of tabs) {
      const path = `${route.path}#${tab.id}`;
      commands.set(path, () => go(path).catch(ignoreNavigationCancellation));
      choices.push([path, `${t(route.label)} · ${t(tab.label)}`]);
    }
  }
  commands.set('add-node', createNodeDialog);
  choices.push(['add-node', t('nodes.add')]);
  let execute;
  const dialog = modal(t('shell.commandPalette'), (close, signal) => {
    const controller = new AbortController();
    const abort = () => controller.abort();
    signal.addEventListener('abort', abort, {once: true});
    navigation?.addEventListener('abort', abort, {once: true});
    controller.signal.addEventListener('abort', () => navigation?.removeEventListener('abort', abort), {once: true});
    const entry = input({'aria-label': t('shell.commandPalette'), dataset: {autofocus: '', transient: ''}});
    const error = h('div', {class: 'form-error', role: 'alert', hidden: true});
    const list = listbox(entry, {choices, emptyText: t('nodes.noMatching'), persistent: true, onChoose: ([id]) => {
      execute = commands.get(id);
      close('done');
    }});
    list.element.setAttribute('aria-label', t('shell.commandPalette'));
    list.open();
    void readNodes(controller.signal).then((nodes) => {
      if (controller.signal.aborted) return;
      const route = consoleRoutes.find((route) => route.page === 'node');
      for (const node of nodes) {
        const path = route.path.replace('{id}', encodeURIComponent(node.id));
        commands.set(path, () => go(path).catch(ignoreNavigationCancellation));
        choices.push([path, node.name]);
      }
      list.setChoices(choices);
    }).catch((failure) => {
      if (controller.signal.aborted) return;
      error.textContent = errorText(failure);
      error.hidden = false;
    });
    return h('div', {class: 'dialog-body notification-inbox'}, entry, error, list.element);
  });
  void dialog.closed.then((value) => { if (value === 'done' && available()) return execute?.(); });
  return dialog;
}

export function initCommandPalette() {
  const entry = document.getElementById('command-palette');
  const sync = () => { if (entry) entry.hidden = !session.user || session.user.must_change_password; };
  const unsubscribe = onSession(sync);
  sync();
  entry?.addEventListener('click', openCommandPalette);
  const keydown = (event) => {
    if (event.isComposing || !(event.ctrlKey || event.metaKey) || event.altKey || event.key.toLowerCase() !== 'k' || !available() || hasDialog()) return;
    event.preventDefault();
    openCommandPalette();
  };
  document.addEventListener('keydown', keydown);
  return () => {
    unsubscribe();
    document.removeEventListener('keydown', keydown);
    entry?.removeEventListener('click', openCommandPalette);
  };
}
