import {uuid} from '../shared/uuid.js';

export const consoleRoutes = [
  {path: '/login', page: 'login', bare: true},
  {path: '/change-password', page: 'changePassword', bare: true},
  {path: '/overview', page: 'overview', label: 'shell.overview', icon: 'overview', nav: '/overview', fallback: true},
  {path: '/nodes', page: 'nodes', label: 'shell.nodes', icon: 'server', nav: '/nodes'},
  {path: '/nodes/{id}', page: 'node', nav: '/nodes'},
  {path: '/settings', page: 'settings', label: 'shell.settings', icon: 'sliders', nav: '/settings'},
  {path: '/alerts', page: 'alerts', label: 'shell.alerts', icon: 'bell', nav: '/alerts'}
];

export const NODE_PATH = new RegExp(`^/nodes/(${uuid.source.slice(1, -1)})$`, 'i');
export const matchesConsoleRoute = (route, path) => route.path === '/nodes/{id}' ? NODE_PATH.test(path) : route.path === path;

// 页面与命令面板共用标签 ID、名称和图标。
export const alertTabs = [
  {id: 'active', label: 'alerts.tab.active', icon: 'alert'},
  {id: 'history', label: 'alerts.tab.history', icon: 'list'},
  {id: 'rules', label: 'alerts.tab.rules', icon: 'sliders'},
  {id: 'silences', label: 'alerts.silences.title', icon: 'moon'},
  {id: 'channels', label: 'alerts.tab.channels', icon: 'bell'}
];

export const settingsTabs = [
  {id: 'account', label: 'settings.account', icon: 'user'},
  {id: 'administrators', label: 'settings.administrators', icon: 'users'},
  {id: 'groups', label: 'settings.groups', icon: 'folder'},
  {id: 'audit', label: 'settings.audit', icon: 'list'},
  {id: 'data', label: 'settings.data', icon: 'cloud'},
  {id: 'public-page', label: 'settings.publicPage', icon: 'globe'}
];
