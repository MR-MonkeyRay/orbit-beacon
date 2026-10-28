import {h, icon} from './dom.js';

// The HTML shells apply a stored theme before first paint; this module only
// changes it. "system" means no override, following prefers-color-scheme.
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
  } catch { /* storage can be unavailable; the choice still applies to this page */ }
  listeners.forEach((paint) => paint());
}

window.addEventListener('storage', (event) => {
  if (event.key !== KEY) return;
  applyTheme(ORDER.includes(event.newValue) ? event.newValue : 'system');
  listeners.forEach((paint) => paint());
});

// names: display names per theme; describe(current, next) builds the accessible label.
export function themeButton(names, describe, element = h('button', {type: 'button', class: 'icon-button'})) {
  const next = (theme) => ORDER[(ORDER.indexOf(theme) + 1) % ORDER.length];
  const paint = () => {
    const theme = currentTheme();
    element.replaceChildren(icon(ICON[theme]));
    element.setAttribute('aria-label', describe(names[theme], names[next(theme)]));
    element.title = element.getAttribute('aria-label');
  };
  element.addEventListener('click', () => setTheme(next(currentTheme())));
  listeners.add(paint);
  paint();
  return element;
}
