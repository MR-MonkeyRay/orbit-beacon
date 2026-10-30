import en from './locales/public-catalog.en.js';
import zhCN from './locales/public-catalog.zh-CN.js';

const STORAGE_KEY = 'orbitbeacon-locale';
const preferences = new Set(['system', 'zh-CN', 'en']);
let cachedStorage;
let cachedNavigator;
let cachedLocale;
let catalogs = {en, 'zh-CN': zhCN};
const plurals = new Map();
const listFormats = new Map();

// The public entry uses the small synchronous catalogs above. The console
// installs its full catalogs before any console labels or formatters are built.
export function setCatalogs(value) { catalogs = value; }

export function resolveLocale(preference, languages) {
  if (preference === 'zh-CN' || preference === 'en') return preference;
  for (const language of Array.isArray(languages) ? languages : []) {
    if (typeof language !== 'string') continue;
    if (/^zh(?:-|$)/i.test(language)) return 'zh-CN';
    if (/^en(?:-|$)/i.test(language)) return 'en';
  }
  return 'en';
}

export function getLocalePreference() {
  try {
    const stored = globalThis.localStorage?.getItem(STORAGE_KEY);
    return preferences.has(stored) ? stored : 'system';
  } catch { return 'system'; }
}

export function getLocale() {
  let storage;
  try { storage = globalThis.localStorage; } catch {}
  const navigator = globalThis.navigator;
  // Switching language is an explicit, dirty-checked full reload. A storage or
  // browser-language change in another tab must not mix fresh labels with this
  // document's existing shell and module-scoped Intl formatters.
  if (cachedLocale && storage === cachedStorage && navigator === cachedNavigator) return cachedLocale;
  cachedStorage = storage;
  cachedNavigator = navigator;
  const preference = getLocalePreference();
  const languages = Array.isArray(navigator?.languages) && navigator.languages.length
    ? navigator.languages : navigator?.language ? [navigator.language] : [];
  cachedLocale = resolveLocale(preference, languages);
  return cachedLocale;
}

export function setLocalePreference(preference) {
  if (!preferences.has(preference)) throw new TypeError('Invalid locale preference');
  // Storage is the sole persistence path. Failure must leave the current page
  // and its unsaved values untouched, so never publish an in-memory fallback.
  if (!globalThis.localStorage) throw new Error('Locale storage unavailable');
  globalThis.localStorage.setItem(STORAGE_KEY, preference);
  cachedLocale = undefined;
}

export function formatList(values = []) {
  const locale = getLocale();
  let formatter = listFormats.get(locale);
  if (!formatter) {
    formatter = new Intl.ListFormat(locale);
    listFormats.set(locale, formatter);
  }
  return formatter.format(values.map(String));
}

function message(key, params) {
  const locale = getLocale();
  let value = catalogs[locale][key];
  if (value === undefined) throw new Error(`Unknown translation key: ${key}`);
  if (typeof value !== 'string') {
    let rules = plurals.get(locale);
    if (!rules) { rules = new Intl.PluralRules(locale); plurals.set(locale, rules); }
    const count = Number(params.count);
    value = value[rules.select(Number.isFinite(count) ? count : 0)] ?? value.other;
  }
  if (typeof value !== 'string') throw new Error(`Invalid translation value: ${key}`);
  return value;
}

function interpolate(key, params, value) {
  return message(key, params).split(/(\{[A-Za-z][A-Za-z0-9_]*\})/).filter(Boolean).map((part) => {
    if (!/^\{[A-Za-z][A-Za-z0-9_]*\}$/.test(part)) return part;
    const name = part.slice(1, -1);
    if (!Object.hasOwn(params, name)) throw new Error(`Missing translation parameter ${name} for ${key}`);
    return value(params[name]);
  });
}

export function t(key, params = {}) { return interpolate(key, params, String).join(''); }

// Preserve links and live time elements while translations control word order.
export function tParts(key, params = {}) { return interpolate(key, params, (value) => value); }
