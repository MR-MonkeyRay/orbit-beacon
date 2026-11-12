import {h} from '../shared/dom.js';
import {t, getLocale} from '../shared/i18n.js';
import {availabilityBar} from '../shared/availability.js';

export function serviceSection() {
  const rows = h('ul', {class: 'public-service-list'});
  const root = h('section', {class: 'public-services', hidden: true, 'aria-labelledby': 'public-services-heading'}, h('h2', {id: 'public-services-heading'}, t('public.services')), rows);
  let entries = [];
  root.update = (services, generatedAt) => {
    root.hidden = services.length === 0;
    while (entries.length > services.length) entries.pop().root.remove();
    while (entries.length < services.length) {
      const name = h('strong'), dot = h('span'), latency = h('span', {class: 'service-latency num'}), bar = availabilityBar();
      const row = h('li', {class: 'public-service-row'}, h('div', {class: 'public-service-label'}, dot, name, latency), bar);
      entries.push({root: row, name, dot, latency, bar}); rows.append(row);
    }
    const start = new Date(generatedAt); start.setUTCHours(0, 0, 0, 0); start.setUTCDate(start.getUTCDate() - 29);
    services.forEach((service, i) => {
      const entry = entries[i];
      entry.name.textContent = service.name;
      entry.dot.className = `dot ${service.status === 'up' ? 'online' : service.status === 'down' ? 'offline' : 'unknown'}`;
      entry.dot.setAttribute('role', 'img'); entry.dot.setAttribute('aria-label', t(`public.serviceStatus.${service.status}`));
      entry.latency.textContent = service.latency_ms === null ? '—' : `${new Intl.NumberFormat(getLocale(), {maximumFractionDigits: 1}).format(service.latency_ms)} ms`;
      // Daily percentages are not sample-weighted totals; omit a misleading total.
      entry.bar.update({start_day: start.toISOString().slice(0, 10), days: service.availability, percent: null});
      entry.bar.setAttribute('aria-label', `${service.name}: ${t('public.servicesDaily')}. ${service.availability.map((v, i) => `${new Date(start.getTime() + i * 86400000).toISOString().slice(0, 10)}: ${v === null ? t('public.availabilityNoData') : `${new Intl.NumberFormat(getLocale(), {maximumFractionDigits: 2}).format(v)}%`}`).join('; ')}`);
    });
  };
  return root;
}
