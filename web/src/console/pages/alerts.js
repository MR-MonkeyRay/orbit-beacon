import {h} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {allNodes, api, getNavigationSignal, groupsList} from '../api.js';
import {refresh} from '../router.js';
import {pageHead} from '../ui/components.js';
import {tabSet} from '../ui/tabs.js';
import {eventsPanel} from './alert-events.js';
import {rulesPanel} from './alert-rules.js';
import {channelsPanel, deliveriesPanel} from './notification-channels.js';
import {showAlert} from './alert-detail.js';

export async function alertsPage() {
  const signal = getNavigationSignal();
  const [{rules}, {channels}, groups, nodes] = await Promise.all([
    api('/alert-rules', {signal}), api('/notification-channels', {signal}), groupsList(), allNodes()
  ]);
  const context = {rules, channels, groups, nodes, signal, saved: refresh};
  const event = new URLSearchParams(location.search).get('event');
  if (event !== null) requestAnimationFrame(() => { if (!signal?.aborted) showAlert(event, signal); });
  return h('div', null,
    pageHead(t('alerts.title'), t('alerts.subtitle')),
    tabSet([
      {id: 'active', label: t('alerts.tab.active'), icon: 'alert', build: () => eventsPanel('firing', signal)},
      {id: 'history', label: t('alerts.tab.history'), icon: 'list', build: () => eventsPanel('all', signal)},
      {id: 'rules', label: t('alerts.tab.rules'), icon: 'sliders', build: () => rulesPanel(context)},
      {id: 'channels', label: t('alerts.tab.channels'), icon: 'bell', build: () => h('div', null, channelsPanel(context), deliveriesPanel(channels, signal))}
    ], t('alerts.sections')));
}
