import {h} from '../../src/shared/dom.js';
import {t, tParts} from '../../src/shared/i18n.js';

import {formatTimestamp} from '../../src/shared/format.js';

const time = (iso) => h('time', {datetime: iso, title: iso}, formatTimestamp(new Date(iso)));

export function announcementRenderer(container) {
  let previous = [];
  return (items) => {
    const unused = [...previous];
    const next = items.map((item) => {
      const key = JSON.stringify([item.level, item.title, item.body, item.starts_at, item.ends_at]);
      const index = unused.findIndex((entry) => entry.key === key);
      if (index >= 0) return unused.splice(index, 1)[0];
      const element = h('article', {class: `public-announcement ${item.level}`});
      const level = h('p', {class: 'announcement-level'});
      const title = h('h2');
      const when = h('p', {class: 'announcement-when'},
        item.ends_at ? tParts('public.announcement.window', {start: time(item.starts_at), end: time(item.ends_at)}) : tParts('public.announcement.starts', {start: time(item.starts_at)}));
      const body = h('p', {class: 'announcement-body'});
      level.textContent = t(`alerts.severity.${item.level}`);
      title.textContent = item.title;
      body.textContent = item.body;
      element.append(level, title, when, body);
      return {key, element};
    });
    for (const entry of unused) entry.element.remove();
    next.forEach((entry, index) => {
      if (container.children[index] !== entry.element) container.insertBefore(entry.element, container.children[index] || null);
    });
    container.hidden = next.length === 0;
    previous = next;
  };
}
