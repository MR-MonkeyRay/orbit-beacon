import {h} from '../../shared/dom.js';
import {availabilityBar} from '../../shared/availability.js';
import {errorText} from '../../shared/errors.js';
import {t} from '../i18n.js';
import {api} from '../api.js';
import {button, panel} from '../ui/components.js';

export function availabilityPanel(id, signal) {
  const content = h('div', {'aria-busy': 'true'}, h('span', {class: 'sk sk-availability', 'aria-hidden': 'true'}));
  const root = panel(t('public.availability'), {}, content);
  if (signal?.aborted) return root;
  const load = () => {
    content.setAttribute('aria-busy', 'true');
    api(`/nodes/${encodeURIComponent(id)}/availability?days=30`, {signal}).then((data) => {
      if (signal?.aborted) return;
      const bar = availabilityBar();
      bar.update({start_day: data.days[0].day, percent: data.percent, days: data.days.map((day) => day.percent), details: data.days});
      content.replaceChildren(bar);
    }).catch((error) => {
      if (!signal?.aborted && error.name !== 'AbortError') content.replaceChildren(h('div', {class: 'history-error', role: 'status'},
        h('p', null, errorText(error)), button(t('common.retry'), {small: true, onClick: load})));
    }).finally(() => { if (!signal?.aborted) content.setAttribute('aria-busy', 'false'); });
  };
  load();
  return root;
}
