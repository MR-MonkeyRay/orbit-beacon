import {t} from '../i18n.js';

const stateNames = {queued: 'common.queued', running: 'common.running', succeeded: 'common.succeeded', failed: 'common.failed'};
const categoryKeys = {configuration: 'common.configuration', monitoring: 'common.monitoring', events: 'common.events'};

export const taskStateKey = (state) => stateNames[state];
export const knownTaskState = (state) => Object.hasOwn(stateNames, state);
export const taskCategoryKey = (category) => categoryKeys[category];
export const taskState = (task) => t(taskStateKey(task.state) || 'data.state.unknown');
export const categoryNames = (categories = []) => categories
  .map((category) => t(taskCategoryKey(category) || 'data.category.unknown'))
  .join(t('data.listSeparator'));
