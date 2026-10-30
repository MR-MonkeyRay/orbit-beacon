import {uuid} from '../shared/uuid.js';

export const consoleRoutes = [
  {path: '/login', page: 'login', bare: true},
  {path: '/change-password', page: 'changePassword', bare: true},
  {path: '/overview', page: 'overview', nav: '/overview', fallback: true},
  {path: '/nodes', page: 'nodes', nav: '/nodes'},
  {path: '/nodes/{id}', page: 'node', nav: '/nodes'},
  {path: '/settings', page: 'settings', nav: '/settings'},
  {path: '/alerts', page: 'alerts', nav: '/alerts'}
];

export const NODE_PATH = new RegExp(`^/nodes/(${uuid.source.slice(1, -1)})$`, 'i');
export const matchesConsoleRoute = (route, path) => route.path === '/nodes/{id}' ? NODE_PATH.test(path) : route.path === path;
