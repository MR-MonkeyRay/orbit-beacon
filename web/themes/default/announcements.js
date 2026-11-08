import {h} from '../../src/shared/dom.js';
import {t} from '../../src/shared/i18n.js';

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
      const body = h('p', {class: 'announcement-body'});
      level.textContent = t(`alerts.severity.${item.level}`);
      title.textContent = item.title;
      body.textContent = item.body;
      element.append(level, title, body);
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
