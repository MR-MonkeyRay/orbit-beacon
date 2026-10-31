import en from '../shared/locales/en.js';
import zhCN from '../shared/locales/zh-CN.js';
import {setCatalogs} from '../shared/i18n.js';

setCatalogs({en, 'zh-CN': zhCN});
export {formatList, getLocale, getLocalePreference, resolveLocale, setLocalePreference, t, tParts} from '../shared/i18n.js';
