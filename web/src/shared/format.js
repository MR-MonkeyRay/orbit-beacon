import {getLocale} from './i18n.js';

const loadFormat = new Intl.NumberFormat(getLocale(), {useGrouping: false, minimumFractionDigits: 2, maximumFractionDigits: 2});

export const formatLoadNumber = (value) => loadFormat.format(Number(value.toFixed(2)));
