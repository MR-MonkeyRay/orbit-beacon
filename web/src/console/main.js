import './styles/index.css';
import {startRouter} from './router.js';
import {initShell} from './shell.js';
import {alertsPage} from './pages/alerts.js';
import {loginPage} from './pages/login.js';
import {NODE_PATH, nodePage} from './pages/node.js';
import {nodesPage} from './pages/nodes.js';
import {overviewPage} from './pages/overview.js';
import {settingsPage} from './pages/settings.js';

// Paths must stay in sync with the console routes the Go server answers with 200.html.
const routes = [
  {match: (path) => path === '/login', page: loginPage, bare: true},
  {match: (path) => path === '/overview', page: overviewPage, nav: '/overview', fallback: true},
  {match: (path) => path === '/nodes', page: nodesPage, nav: '/nodes'},
  {match: (path) => NODE_PATH.test(path), page: nodePage, nav: '/nodes'},
  {match: (path) => path === '/settings', page: settingsPage, nav: '/settings'},
  {match: (path) => path === '/alerts', page: alertsPage, nav: '/alerts'}
];

initShell();
startRouter(routes);

// "/" jumps to the node search, as in many list-heavy tools.
document.addEventListener('keydown', (event) => {
  if (event.key !== '/' || event.target.closest('input,textarea,select,[contenteditable]') || document.querySelector('dialog[open]')) return;
  const search = document.getElementById('node-search');
  if (!search) return;
  event.preventDefault();
  search.focus();
  search.select();
});
