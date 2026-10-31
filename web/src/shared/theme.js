import {h, icon} from './dom.js';
import {t} from './i18n.js';

// HTML 外壳在首次绘制前应用已存储的主题；此模块只负责切换。
// "system" 表示不覆盖，跟随 prefers-color-scheme。
const KEY = 'orbitbeacon-theme';
const ORDER = ['system', 'light', 'dark'];
const ICON = {system: 'monitor', light: 'sun', dark: 'moon'};
const listeners = new Set();

export function currentTheme() { return document.documentElement.dataset.theme || 'system'; }

function applyTheme(theme) {
  if (theme === 'system') delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = theme;
}

export function setTheme(theme) {
  applyTheme(theme);
  try {
    if (theme === 'system') localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, theme);
  } catch { /* 存储可能不可用；选择仍应用于当前页面 */ }
  listeners.forEach((paint) => paint());
}

window.addEventListener('storage', (event) => {
  if (event.key !== KEY) return;
  applyTheme(ORDER.includes(event.newValue) ? event.newValue : 'system');
  listeners.forEach((paint) => paint());
});

export function themeButton(element = h('button', {type: 'button', class: 'icon-button'})) {
  const next = (theme) => ORDER[(ORDER.indexOf(theme) + 1) % ORDER.length];
  const paint = () => {
    const theme = currentTheme();
    element.replaceChildren(icon(ICON[theme]));
    element.setAttribute('aria-label', t('controls.theme.switch', {
      current: t(`controls.theme.${theme}`), next: t(`controls.theme.${next(theme)}`)
    }));
    element.title = element.getAttribute('aria-label');
  };
  element.addEventListener('click', () => setTheme(next(currentTheme())));
  listeners.add(paint);
  paint();
  return element;
}
