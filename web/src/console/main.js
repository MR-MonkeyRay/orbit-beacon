import './styles/index.css';
import './i18n.js';
import {translateHTML} from '../shared/translate-html.js';
import {startRouter} from './router.js';
import {initShell} from './shell.js';
import {consoleRoutes, matchesConsoleRoute} from './routes.js';
import {alertsPage} from './pages/alerts.js';
import {loginPage} from './pages/login.js';
import {changePasswordPage} from './pages/change-password.js';
import {nodePage} from './pages/node.js';
import {nodesPage} from './pages/nodes.js';
import {overviewPage} from './pages/overview.js';
import {settingsPage} from './pages/settings.js';

translateHTML();

const pages = {login: loginPage, changePassword: changePasswordPage, overview: overviewPage, nodes: nodesPage, node: nodePage, settings: settingsPage, alerts: alertsPage};
const routes = consoleRoutes.map((route) => ({...route, match: (path) => matchesConsoleRoute(route, path), page: pages[route.page]}));

initShell();
startRouter(routes);

// "/" 跳转到节点搜索，与多数列表型工具一致。
document.addEventListener('keydown', (event) => {
  const editing = event.target instanceof Element && event.target.closest('input,textarea,select,[contenteditable]');
  if (event.key !== '/' || event.ctrlKey || event.metaKey || event.altKey || event.isComposing || editing || document.querySelector('dialog[open]')) return;
  const search = document.getElementById('node-search');
  if (!search) return;
  event.preventDefault();
  search.focus();
  search.select();
});
