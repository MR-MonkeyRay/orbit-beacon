import {h} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {button} from './components.js';

export function historyError(message, {retryID, onRetry, status = false}) {
  return h('div', {class: 'history-error'},
    h('p', {class: 'muted', ...(status ? {role: 'status'} : {})}, message),
    button(t('common.retry'), {small: true, id: retryID, onClick: onRetry}));
}
