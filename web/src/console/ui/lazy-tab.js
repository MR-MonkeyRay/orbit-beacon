import {h} from '../../shared/dom.js';
import {errorText} from '../../shared/errors.js';
import {t} from '../i18n.js';
import {getNavigationSignal} from '../api.js';
import {button, skeleton} from './components.js';

// 标签页数据按需加载：加载失败只影响当前标签页，提供原地重试。
export function lazyTab(load, render) {
  const navigation = getNavigationSignal();
  let result;
  let hasResult = false;
  let pending;
  let requestEpoch = 0;
  return () => {
    const element = h('div');
    const start = () => {
      if (navigation?.aborted) return;
      element.replaceChildren(skeleton());
      if (!pending) { pending = load(); requestEpoch += 1; }
      const request = pending;
      const epoch = requestEpoch;
      request.then((value) => {
        if (navigation?.aborted || epoch !== requestEpoch) return;
        if (!hasResult) {
          result = value;
          hasResult = true;
          pending = null;
        }
        element.replaceChildren(render(value));
      }, (error) => {
        if (navigation?.aborted || epoch !== requestEpoch) return;
        pending = null;
        element.replaceChildren(h('div', {class: 'form-error', role: 'alert'},
          h('p', null, errorText(error)),
          button(t('common.retry'), {onClick: start})));
      });
    };
    if (hasResult) element.replaceChildren(render(result));
    else start();
    return element;
  };
}
