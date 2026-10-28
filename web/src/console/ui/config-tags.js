import {byteLength, h, icon} from '../../shared/dom.js';
import {uid} from './components.js';

// Config selections share the tag editor interaction but have their own byte and count limits.
export function configTagEditor(values = [], {label, fieldName = label, hint, maxItems = 8, maxBytes = 255, trim = true, separators = /[\n,]/, commitOnComma = true, validate} = {}) {
  let items = [...values];
  const entryID = uid('selection-entry');
  const labelID = `${entryID}-label`;
  const hintID = `${entryID}-hint`;
  const errorID = `${entryID}-error`;
  const entry = h('input', {type: 'text', enterkeyhint: 'done', id: entryID, 'aria-labelledby': labelID, 'aria-describedby': `${hintID} ${errorID}`});
  const box = h('div', {class: 'tags', dataset: {tags: '', field: fieldName}, onclick: (event) => { if (event.target === box) entry.focus(); }});
  const error = h('span', {class: 'field-error', id: errorID, hidden: true, role: 'alert'});
  const showError = (message) => { error.textContent = message; error.hidden = !message; if (message) entry.setAttribute('aria-invalid', 'true'); else entry.removeAttribute('aria-invalid'); };
  const changed = () => { render(); box.dispatchEvent(new Event('tags', {bubbles: true})); };
  const add = (raw) => {
    const value = trim ? raw.trim() : raw;
    if (!value) return true;
    if (items.length >= maxItems) { showError(`This list is limited to ${maxItems} entries.`); return false; }
    if (byteLength(value) > maxBytes) { showError(`Each entry is limited to ${maxBytes} UTF-8 bytes.`); return false; }
    if (validate) { const message = validate(value); if (message) { showError(message); return false; } }
    if (items.includes(value)) { showError(`“${value}” is already added.`); return false; }
    items.push(value);
    showError('');
    return true;
  };
  box.getValue = () => [...items];
  box.showError = showError;
  box.focus = () => entry.focus();
  box.commit = () => {
    const rejected = entry.value.split(separators).filter((part) => !add(part));
    entry.value = rejected.join(', ');
    changed();
    return rejected.length === 0;
  };
  entry.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' || (commitOnComma && event.key === ',')) { event.preventDefault(); box.commit(); entry.focus(); }
    else if (event.key === 'Backspace' && entry.value === '' && items.length) { items.pop(); changed(); entry.focus(); }
  });
  entry.addEventListener('paste', (event) => {
    const pasted = event.clipboardData?.getData('text');
    if (pasted && separators.test(pasted)) { event.preventDefault(); entry.value += pasted; box.commit(); }
  });
  entry.addEventListener('blur', () => { if (entry.value.trim()) box.commit(); });
  function render() {
    box.replaceChildren(...items.map((item) => h('span', {class: 'tag'}, h('span', {title: item}, item),
      h('button', {type: 'button', 'aria-label': `Remove ${label || 'entry'} ${item}`, onclick: () => { items = items.filter((value) => value !== item); changed(); entry.focus(); }}, icon('x')))), entry);
    entry.placeholder = items.length ? `Add ${label?.toLowerCase() || 'entry'}` : `Type ${label?.toLowerCase() || 'an entry'} and press Enter`;
  }
  render();
  return h('div', {class: 'field'}, h('label', {for: entry.id, class: 'field-label', id: labelID}, label), box,
    h('span', {class: 'hint', id: hintID}, hint), error);
}
