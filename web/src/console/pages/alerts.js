import {alertTabs} from '../routes.js';
import {h} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {allNodes, api, getNavigationSignal, groupsList} from '../api.js';
import {refresh} from '../router.js';
import {lazyTab} from '../ui/lazy-tab.js';
import {pageHead} from '../ui/components.js';
import {tabSet} from '../ui/tabs.js';
import {eventsPanel} from './alert-events.js';
import {silencesPanel} from './alert-silences.js';
import {rulesPanel} from './alert-rules.js';
import {channelsPanel, deliveriesPanel} from './notification-channels.js';
import {showAlert} from './alert-detail.js';

export async function alertsPage() {
  const signal = getNavigationSignal();
  let loadedRules = [];
  let tabs;
  const openChannels = () => tabs.selectTab('channels');
  // 各标签页独立加载：某个接口故障只影响自己，失败处原地重试。
  const builds = {
    'active': () => {
      const panel = eventsPanel('firing', signal, {getRules: () => loadedRules});
      // 事件不含比较符与阈值，从规则列表补齐；失败时只显示当前值。
      api('/alert-rules', {signal}).then(({rules}) => {
        if (signal?.aborted || !Array.isArray(rules)) return;
        loadedRules = rules;
        panel.rerender?.();
      }, () => {});
      return panel;
    },
    'history': () => eventsPanel('all', signal),
    'rules': lazyTab(
      () => Promise.all([api('/alert-rules', {signal}), groupsList(), allNodes(), api('/notification-channels', {signal})]),
      ([{rules}, groups, nodes, {channels}]) => {
        loadedRules = rules;
        return rulesPanel({rules, groups, nodes, channels, signal, saved: refresh, openChannels});
      }
    ),
    'silences': lazyTab(
      () => Promise.all([api('/alert-silences', {signal}), groupsList(), allNodes()]),
      ([{silences}, groups, nodes]) => silencesPanel({silences, groups, nodes, signal, saved: refresh})
    ),
    'channels': lazyTab(
      () => api('/notification-channels', {signal}),
      ({channels}) => h('div', null, channelsPanel({channels, signal, saved: refresh}), deliveriesPanel(channels, signal))
    )
  };
  const event = new URLSearchParams(location.search).get('event');
  if (event !== null) requestAnimationFrame(() => { if (!signal?.aborted) showAlert(event, signal); });
  tabs = tabSet(alertTabs.map((tab) => ({...tab, label: t(tab.label), build: builds[tab.id]})), t('alerts.sections'));
  return h('div', null, pageHead(t('alerts.title')), tabs);
}
