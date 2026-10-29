import {h, icon, svg} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {STATUS} from '../format.js';

// IDs restart on every page render, so a quiet refresh rebuilds the same IDs
// and the router can return focus to the element that had it.
let idCounter = 0;
export function resetIDs() { idCounter = 0; }
export function uid(prefix) { idCounter += 1; return `${prefix}-${idCounter}`; }

export const link = (href, ...children) => h('a', {href, dataset: {link: ''}}, ...children);
export const linkButton = (href, label) => h('a', {class: 'btn', href, dataset: {link: ''}}, label);

export function button(label, options = {}) {
  const classes = ['btn', options.variant && `btn-${options.variant}`, options.small && 'btn-sm', options.className].filter(Boolean).join(' ');
  return h('button', {type: options.type || 'button', class: classes, onclick: options.onClick, disabled: options.disabled, id: options.id || uid('button')},
    options.icon ? icon(options.icon) : null, label, options.trailingIcon ? icon(options.trailingIcon) : null);
}

export function iconButton(name, label, onClick) {
  return h('button', {type: 'button', class: 'icon-button sm', 'aria-label': label, title: label, onclick: onClick, id: uid('button')}, icon(name));
}

export function busy(element, on) {
  element.classList.toggle('busy', on);
  element.setAttribute('aria-busy', String(on));
  element.disabled = on;
  const form = element.form || element.closest?.('form');
  if (form) form.dataset.busy = String(on);
}

export function panel(title, options = {}, ...body) {
  const titleID = title ? uid('panel') : null;
  return h('section', {class: 'panel', 'aria-labelledby': titleID},
    title ? h('div', {class: 'panel-head'}, h('h2', {id: titleID}, title), options.action) : null,
    h('div', {class: 'panel-body'}, body));
}

export function pageHead(title, subtitle, ...actions) {
  actions = actions.filter(Boolean);
  return h('header', {class: 'page-head'},
    h('div', null, h('h1', {tabindex: '-1'}, title), subtitle ? h('p', null, subtitle) : null),
    actions.length ? h('div', {class: 'head-actions'}, actions) : null);
}

const statusLabel = (status) => status === 'deleted' ? t('controls.deleted') : STATUS[status] || status;

export const statusBadge = (status) => h('span', {class: `badge ${status}`}, statusLabel(status));

export function patchStatusBadge(element, status) {
  element.className = `badge ${status}`;
  const label = statusLabel(status);
  if (element.textContent !== label) element.textContent = label;
}

// Transport names stay out of the UI. Only transitions update the live region.
export function liveStatus() {
  const element = h('span', {class: 'live-status', role: 'status', 'aria-live': 'polite', 'aria-atomic': 'true'}, t('controls.live.connecting'));
  let previous = 'connecting';
  return {element, update({state}) {
    if (state === previous) return;
    previous = state;
    element.dataset.state = state;
    element.textContent = t(state === 'live' ? 'controls.live.live' : state === 'connecting' ? 'controls.live.connecting' : 'controls.live.stale');
  }};
}

export function facts(entries) {
  return buildReadonlyFacts(entries, true).element;
}

// Keep the list and its cells stable; callers patch readonly values, never forms.
export function readonlyFacts(entries = []) {
  return buildReadonlyFacts(entries, false);
}

function buildReadonlyFacts(entries, flattenChildren) {
  const element = h('dl', {class: 'facts'});
  const cells = new Map();
  const update = (values) => {
    const occurrences = new Map();
    for (const [label, value] of values) {
      const occurrence = occurrences.get(label) || 0;
      occurrences.set(label, occurrence + 1);
      let group = cells.get(label);
      if (!group) {
        group = [];
        cells.set(label, group);
      }
      let cell = group[occurrence];
      if (!cell) {
        cell = h('dd');
        group.push(cell);
        element.append(h('div', null, h('dt', null, label), cell));
      }
      const empty = value === null || value === undefined || value === '';
      cell.className = empty ? 'none' : '';
      const content = empty ? t('controls.notSet') : value;
      if (content instanceof Node && cell.firstElementChild === content) continue;
      if (!(content instanceof Node) && !Array.isArray(content) && cell.textContent === String(content)) continue;
      const children = flattenChildren
        ? (Array.isArray(content) ? content.flat(Infinity) : [content])
          .filter((child) => child !== null && child !== undefined && child !== false)
          .map((child) => child instanceof Node ? child : String(child))
        : (Array.isArray(content) ? content : [content]);
      cell.replaceChildren(...children);
    }
  };
  update(entries);
  return {element, update};
}

export function dataTable({caption, columns, body, className = '', colgroup, hidden = false, ...wrapperProps}) {
  return h('div', {class: 'table-wrap', hidden, ...wrapperProps}, h('table', {class: `table ${className}`.trim()},
    caption ? h('caption', {class: 'sr'}, caption) : null,
    colgroup,
    h('thead', null, h('tr', null, columns.map((label) => h('th', {scope: 'col'}, label)))), body));
}

export function note(text) { return h('p', {class: 'note'}, icon('info'), h('span', null, text)); }

export function callout(text, kind = 'warn') {
  return h('div', {class: `callout ${kind === 'info' ? 'info' : ''}`.trim()}, icon(kind === 'info' ? 'info' : 'alert'), h('span', null, text));
}

function orbitArt() {
  const art = svg('svg', {class: 'art', viewBox: '0 0 120 72', 'aria-hidden': 'true'});
  art.innerHTML = '<ellipse class="o" cx="60" cy="36" rx="52" ry="22" transform="rotate(-12 60 36)"/><ellipse class="o" cx="60" cy="36" rx="32" ry="13" transform="rotate(-12 60 36)"/><circle class="c" cx="60" cy="36" r="7"/><circle class="s" cx="108" cy="25" r="4"/>';
  return art;
}

export function emptyState(title, message, ...actions) {
  actions = actions.filter(Boolean);
  return h('div', {class: 'empty'}, orbitArt(), h('h2', null, title), message ? h('p', null, message) : null,
    actions.length ? h('div', {class: 'head-actions'}, actions) : null);
}

export function skeleton() {
  const line = (width) => h('div', {class: 'sk sk-line', style: `width:${width}%`});
  return h('div', {'aria-busy': 'true', 'aria-label': t('controls.loading')},
    h('div', {class: 'sk sk-title'}), line(38),
    h('div', {class: 'grid-2'}, h('div', {class: 'sk sk-block'}), h('div', {class: 'sk sk-block'})),
    h('div', {class: 'sk sk-block', style: 'margin-top:18px;height:240px'}));
}
