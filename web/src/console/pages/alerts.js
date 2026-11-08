import {alertTabs} from '../routes.js';
import {h} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {allNodes, api, getNavigationSignal, groupsList} from '../api.js';
import {refresh} from '../router.js';
import {pageHead} from '../ui/components.js';
import {tabSet} from '../ui/tabs.js';
import {eventsPanel} from './alert-events.js';
import {silencesPanel} from './alert-silences.js';
import {rulesPanel} from './alert-rules.js';
import {channelsPanel, deliveriesPanel} from './notification-channels.js';
import {showAlert} from './alert-detail.js';

export async function alertsPage() {
  const signal = getNavigationSignal();
  const [{rules}, {channels}, groups, nodes, {silences}] = await Promise.all([
    api('/alert-rules', {signal}), api('/notification-channels', {signal}), groupsList(), allNodes(), api('/alert-silences', {signal})
  ]);
  const context = {silences, rules, channels, groups, nodes, signal, saved: refresh};
  const event = new URLSearchParams(location.search).get('event');
  if (event !== null) requestAnimationFrame(() => { if (!signal?.aborted) showAlert(event, signal); });
  const builds = {
    'active': () => eventsPanel('firing', signal),
    'history': () => eventsPanel('all', signal),
    'rules': () => rulesPanel(context),
    'silences': () => silencesPanel(context),
    'channels': () => h('div', null, channelsPanel(context), deliveriesPanel(channels, signal))
  };
  return h('div', null,
    pageHead(t('alerts.title')),
    tabSet(alertTabs.map((tab) => ({...tab, label: t(tab.label), build: builds[tab.id]})), t('alerts.sections')));
}
