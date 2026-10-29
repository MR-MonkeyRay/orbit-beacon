import {getLocale, t} from './i18n.js';

export function translateHTML(document = globalThis.document) {
  document.documentElement.lang = getLocale();
  for (const [attribute, target] of [['data-i18n', null], ['data-i18n-aria-label', 'aria-label'], ['data-i18n-content', 'content']]) {
    for (const element of document.querySelectorAll(`[${attribute}]`)) {
      const text = t(element.getAttribute(attribute));
      if (target) element.setAttribute(target, text);
      else element.textContent = text;
    }
  }
}
