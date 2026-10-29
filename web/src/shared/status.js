import {t} from './i18n.js';

// The public page uses a more explicit unknown-status label in Chinese.
export function statusLabels({unknown = 'common.status.unknown'} = {}) {
  return {online: t('common.status.online'), offline: t('common.status.offline'), never: t('common.status.never'),
    unknown: t(unknown), pending: t('common.status.pending'), disabled: t('common.status.disabled')};
}
