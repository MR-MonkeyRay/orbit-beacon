import './styles/index.css';
import './i18n.js';
import {translateHTML} from '../shared/translate-html.js';
import {startRouter} from './router.js';
import {initShell} from './shell.js';
import {consoleRoutes, matchesConsoleRoute} from './routes.js';
import {servicesPage, servicesPlaceholder} from './pages/services.js';
import {alertsPage} from './pages/alerts.js';
import {loginPage} from './pages/login.js';
import {changePasswordPage} from './pages/change-password.js';
import {nodePage, nodePlaceholder} from './pages/node.js';
import {nodesPage, nodesPlaceholder} from './pages/nodes.js';
import {overviewPage, overviewPlaceholder} from './pages/overview.js';
import {settingsPage} from './pages/settings.js';

translateHTML();

const pages = {services: servicesPage, login: loginPage, changePassword: changePasswordPage, overview: overviewPage, nodes: nodesPage, node: nodePage, settings: settingsPage, alerts: alertsPage,
  dataManagement: () => consoleRoutes.find((route) => route.path === '/data').redirect};
// 有监控数据的页面先渲染同构占位，数据到达后原位填入。
const placeholders = {services: servicesPlaceholder, overview: overviewPlaceholder, nodes: nodesPlaceholder, node: nodePlaceholder};
const routes = consoleRoutes.map((route) => ({...route, match: (path) => matchesConsoleRoute(route, path), page: pages[route.page], placeholder: placeholders[route.page]}));

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
