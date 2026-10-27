import {byteLength, h, icon} from '../../shared/dom.js';
import {shortID} from '../format.js';
import {busy, button, iconButton, uid} from './components.js';
import {toastError} from './feedback.js';

// API limits are UTF-8 byte counts, which the browser's maxlength cannot express.
export function validateBytes(control, min, max, label) {
  const check = () => {
    const size = byteLength(control.value);
    let message = '';
    if (size === 0 && min > 0 && control.required) message = `${label} is required.`;
    else if (size > 0 && size < min) message = `${label} needs at least ${min} UTF-8 bytes.`;
    else if (size > max) message = `${label} is limited to ${max} UTF-8 bytes (currently ${size}).`;
    control.setCustomValidity(message);
  };
  control.addEventListener('input', check);
  check();
  return check;
}

export const input = (props) => h('input', {class: 'input', type: 'text', ...props});

export function textarea(props, value) {
  const element = h('textarea', {class: 'input', rows: 4, ...props});
  element.value = value;
  return element;
}

export function select(choices, value, props = {}) {
  const element = h('select', {class: 'input', ...props}, choices.map(([id, title]) => h('option', {value: id}, title)));
  element.value = value;
  return element;
}

// Labelled control with hint and inline error. Validation messages appear after
// the first submit attempt or once the user leaves a non-empty field.
// options.inner replaces the control in the layout (e.g. a password wrapper).
export function field(label, control, options = {}) {
  if (!control.id) control.id = uid('field');
  const hintID = options.hint ? `${control.id}-hint` : null;
  const error = h('span', {class: 'field-error', id: `${control.id}-error`, hidden: true});
  control.setAttribute('aria-describedby', [hintID, error.id].filter(Boolean).join(' '));
  control.showError = (message) => {
    error.textContent = message;
    error.hidden = !message;
    if (message) control.setAttribute('aria-invalid', 'true');
    else control.removeAttribute('aria-invalid');
  };
  control.addEventListener('invalid', (event) => {
    event.preventDefault();
    control.classList.add('touched');
    control.showError(control.validationMessage);
    const active = document.activeElement;
    if (!(active && active.form === control.form && active.matches(':invalid'))) control.focus();
  });
  control.addEventListener('input', () => {
    if (control.classList.contains('touched') || control.hasAttribute('aria-invalid')) control.showError(control.validity.valid ? '' : control.validationMessage);
  });
  control.addEventListener('blur', () => { if (control.value !== '') control.classList.add('touched'); });
  return h('div', {class: 'field'},
    h('label', {for: control.id, class: 'field-label'}, label),
    options.inner || control,
    options.hint ? h('span', {class: 'hint', id: hintID}, options.hint) : null,
    error);
}

export function passwordInput(props) {
  const control = input({type: 'password', ...props});
  const toggle = iconButton('eye', 'Show password', () => {
    const show = control.type === 'password';
    control.type = show ? 'text' : 'password';
    toggle.replaceChildren(icon(show ? 'eye-off' : 'eye'));
    toggle.setAttribute('aria-pressed', String(show));
    control.focus();
  });
  toggle.setAttribute('aria-pressed', 'false');
  control.wrapper = h('div', {class: 'with-action'}, control, toggle);
  return control;
}

// Slider and number input bound together; the number input is the form value.
export function rangeField(label, name, value, {min, max, step, hint}) {
  const number = input({type: 'number', name, min, max, step: 1, value, required: true, inputMode: 'numeric', id: `field-${name}`});
  const range = h('input', {type: 'range', min, max, step, value, tabIndex: -1, 'aria-hidden': 'true'});
  range.addEventListener('input', () => { number.value = range.value; number.dispatchEvent(new Event('input', {bubbles: true})); });
  number.addEventListener('input', () => { if (number.value !== '') range.value = number.value; });
  return field(label, number, {inner: h('div', {class: 'range-field'}, range, number), hint});
}

// Applies validation_failed field messages to controls with matching
// data-field or name attributes. Returns whether any control matched.
function applyFieldErrors(form, error) {
  if (!error.fields) return false;
  let first = null;
  for (const [key, message] of Object.entries(error.fields)) {
    const control = form.querySelector(`[data-field="${CSS.escape(key)}"]`) || form.elements.namedItem(key);
    if (control && control.showError) {
      control.showError(message);
      first = first || control;
    }
  }
  if (first) first.focus();
  return Boolean(first);
}

// Runs action on submit with a busy button. Errors go to errorBox when given
// (dialogs sit above the toast layer), otherwise to a toast.
export function onSubmit(form, submitButton, action, errorBox) {
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    if (submitButton.classList.contains('busy')) return;
    if (errorBox) errorBox.hidden = true;
    busy(submitButton, true);
    try {
      await action();
    } catch (error) {
      applyFieldErrors(form, error);
      if (errorBox) {
        errorBox.replaceChildren(icon('alert'), h('span', null, error.message, error.requestID ? h('small', {class: 'muted'}, ` (request ${shortID(error.requestID)})`) : null));
        errorBox.hidden = false;
      } else toastError(error);
    } finally {
      busy(submitButton, false);
      if (form.syncDirty) form.syncDirty();
    }
  });
}

// Save is enabled only while the form differs from its initial values; the
// router asks before leaving a page with a dirty form.
export function trackDirty(form, controls) {
  const snapshot = () => JSON.stringify([...form.querySelectorAll('[name],[data-tags]')]
    .map((element) => (element.dataset.tags !== undefined ? element.getValue() : element.type === 'checkbox' ? element.checked : element.value)));
  const initial = snapshot();
  form.syncDirty = () => {
    const dirty = snapshot() !== initial;
    form.dataset.dirty = String(dirty);
    controls.save.disabled = !dirty;
    controls.discard.hidden = !dirty;
    controls.note.classList.toggle('on', dirty);
  };
  form.markClean = () => { form.dataset.dirty = 'false'; };
  for (const type of ['input', 'change', 'tags']) form.addEventListener(type, form.syncDirty);
  form.syncDirty();
}

export function saveBar(label, id) {
  const save = button(label, {type: 'submit', variant: 'primary', id});
  const discard = button('Discard changes', {variant: 'ghost'});
  const note = h('span', {class: 'dirty-note'}, 'Unsaved changes');
  return {save, discard, note, bar: h('div', {class: 'form-actions'}, save, discard, note)};
}

// Chip editor for node tags: Enter or comma adds, Backspace on empty removes the
// last tag, pasted lists are split and trimmed. Tags are 1–255 bytes and unique.
export function tagEditor(values) {
  let tags = [...values];
  const entry = h('input', {type: 'text', 'aria-label': 'Add tag', enterkeyhint: 'done', id: 'tag-entry'});
  const box = h('div', {class: 'tags', dataset: {tags: '', field: 'tags'}, onclick: (event) => { if (event.target === box) entry.focus(); }});
  const error = h('span', {class: 'field-error', hidden: true, role: 'alert'});
  const changed = () => { render(); box.dispatchEvent(new Event('tags', {bubbles: true})); };
  const add = (raw) => {
    const value = raw.trim();
    if (value === '') return true;
    if (byteLength(value) > 255) { box.showError('Each tag is limited to 255 UTF-8 bytes.'); return false; }
    if (tags.includes(value)) { box.showError(`“${value}” is already added.`); return false; }
    tags.push(value);
    box.showError('');
    return true;
  };
  box.getValue = () => tags;
  box.showError = (message) => { error.textContent = message; error.hidden = !message; };
  box.focus = () => entry.focus();
  box.commit = () => {
    const rejected = entry.value.split(/[\n,]/).filter((part) => !add(part));
    entry.value = rejected.join(', ');
    changed();
    return rejected.length === 0;
  };
  entry.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' || event.key === ',') { event.preventDefault(); box.commit(); entry.focus(); }
    else if (event.key === 'Backspace' && entry.value === '' && tags.length) { tags.pop(); changed(); entry.focus(); }
  });
  entry.addEventListener('paste', (event) => {
    const pasted = event.clipboardData && event.clipboardData.getData('text');
    if (pasted && /[\n,]/.test(pasted)) { event.preventDefault(); entry.value += pasted; box.commit(); }
  });
  entry.addEventListener('blur', () => { if (entry.value.trim()) box.commit(); });
  function render() {
    box.replaceChildren(...tags.map((tag) => h('span', {class: 'tag'}, h('span', {title: tag}, tag),
      h('button', {type: 'button', 'aria-label': `Remove tag ${tag}`, onclick: () => { tags = tags.filter((item) => item !== tag); changed(); entry.focus(); }}, icon('x')))), entry);
    entry.placeholder = tags.length ? 'Add tag' : 'Type a tag and press Enter';
  }
  render();
  return h('div', {class: 'field'}, h('label', {for: entry.id, class: 'field-label'}, 'Tags'), box,
    h('span', {class: 'hint'}, 'Press Enter or comma to add a tag. Each tag is 1–255 UTF-8 bytes.'), error);
}

