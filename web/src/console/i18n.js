import en from '../shared/locales/en.js';
import zhCN from '../shared/locales/zh-CN.js';
import {setCatalogs} from '../shared/i18n.js';

setCatalogs({en, 'zh-CN': zhCN});
export {getLocale, getLocalePreference, resolveLocale, setLocalePreference, t} from '../shared/i18n.js';
