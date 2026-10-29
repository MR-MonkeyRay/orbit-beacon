import {byteLength, h, icon} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {errorText, fieldErrorText} from '../../shared/errors.js';
import {busy, button, iconButton, uid} from './components.js';
import {toastError} from './feedback.js';

// API limits are UTF-8 byte counts, which the browser's maxlength cannot express.
export function validateBytes(control, min, max, label) {
  const check = () => {
    const size = byteLength(control.value);
    let message = '';
    if (size === 0 && min > 0 && control.required) message = t('controls.validation.bytesRequired', {label});
    else if (size > 0 && size < min) message = t('controls.validation.bytesMinimum', {label, min});
    else if (size > max) message = t('controls.validation.bytesMaximum', {label, max, size});
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

// Only customError is supplied by the application. Native browser messages may
// use a different language from the selected interface locale.
function validationText(control) {
  const validity = control.validity;
  if (validity.valid) return '';
  if (validity.customError) return control.validationMessage;
  if (validity.valueMissing) return t('controls.validation.required');
  if (validity.typeMismatch) return t('controls.validation.type');
  if (validity.patternMismatch) return t('controls.validation.pattern');
  if (validity.rangeUnderflow) return t('controls.validation.minimum', {min: control.min});
  if (validity.rangeOverflow) return t('controls.validation.maximum', {max: control.max});
  if (validity.stepMismatch) return t('controls.validation.step', {step: control.step || 1});
  if (validity.tooShort) return t('controls.validation.tooShort', {min: control.minLength});
  if (validity.tooLong) return t('controls.validation.tooLong', {max: control.maxLength});
  if (validity.badInput) return t('controls.validation.badInput');
  return t('controls.validation.invalid');
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
    control.showError(validationText(control));
    const active = document.activeElement;
    if (!(active && active.form === control.form && active.matches(':invalid'))) control.focus();
  });
  control.addEventListener('input', () => {
    if (control.classList.contains('touched') || control.hasAttribute('aria-invalid')) control.showError(validationText(control));
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
  const toggle = iconButton('eye', t('controls.password.show'), () => {
    const show = control.type === 'password';
    control.type = show ? 'text' : 'password';
    toggle.replaceChildren(icon(show ? 'eye-off' : 'eye'));
    toggle.setAttribute('aria-pressed', String(show));
    const label = t(show ? 'controls.password.hide' : 'controls.password.show');
    toggle.setAttribute('aria-label', label);
    toggle.title = label;
    control.focus();
  });
  toggle.setAttribute('aria-pressed', 'false');
  control.wrapper = h('div', {class: 'with-action'}, control, toggle);
  return control;
}


// Applies validation_failed field messages to controls with matching
// data-field or name attributes. Returns whether any control matched.
function applyFieldErrors(form, error) {
  if (!error.fields) return false;
  let first = null;
  for (const [key, message] of Object.entries(error.fields)) {
    const control = form.querySelector(`[data-field="${CSS.escape(key)}"]`) || form.elements.namedItem(key);
    if (control && control.showError) {
      control.showError(error.clientValidation ? message : fieldErrorText(message));
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
    form.dataset.busy = 'true';
    try {
      await action();
    } catch (error) {
      if (error.name === 'AbortError') return;
      if (!form.isConnected) return;
      applyFieldErrors(form, error);
      if (errorBox) {
        errorBox.replaceChildren(icon('alert'), h('span', null, errorText(error)));
        errorBox.hidden = false;
      } else toastError(error);
    } finally {
      busy(submitButton, false);
      form.dataset.busy = 'false';
      if (form.syncDirty) form.syncDirty();
    }
  });
}

// Forms without save bars (including password forms) share the same baseline.
export function trackFormDirty(form) {
  const snapshot = () => JSON.stringify([...form.querySelectorAll('[name],[data-tags]')]
    .map((element) => element.dataset.tags !== undefined ? element.getValue() : element.multiple ? [...element.selectedOptions].map((option) => option.value)
      : ['checkbox', 'radio'].includes(element.type) ? element.checked : element.value));
  let baseline = snapshot();
  const state = {
    get dirty() { return form.dataset.dirty === 'true'; },
    syncDirty() { form.dataset.dirty = String(snapshot() !== baseline); },
    markClean() { baseline = snapshot(); form.syncDirty(); }
  };
  form.syncDirty = state.syncDirty;
  form.markClean = state.markClean;
  for (const type of ['input', 'change', 'tags']) form.addEventListener(type, () => form.syncDirty());
  form.syncDirty();
  return state;
}

// Save bars reuse the baseline and add only their own button/note state.
export function trackDirty(form, controls) {
  const state = trackFormDirty(form);
  form.syncDirty = () => {
    state.syncDirty();
    controls.save.disabled = !state.dirty;
    controls.discard.hidden = !state.dirty;
    controls.note.classList.toggle('on', state.dirty);
  };
  form.syncDirty();
  return state;
}

export function saveBar(label, id) {
  const save = button(label, {type: 'submit', variant: 'primary', id});
  const discard = button(t('controls.discard'), {variant: 'ghost'});
  const note = h('span', {class: 'dirty-note'}, t('controls.unsaved'));
  return {save, discard, note, bar: h('div', {class: 'form-actions'}, save, discard, note)};
}

// Chip editor for node tags: Enter or comma adds, Backspace on empty removes the
// last tag, pasted lists are split and trimmed. Tags are 1–255 bytes and unique.
export function tagEditor(values) {
  let tags = [...values];
  const entryID = uid('tag-entry');
  const labelID = `${entryID}-label`;
  const hintID = `${entryID}-hint`;
  const errorID = `${entryID}-error`;
  const entry = h('input', {type: 'text', enterkeyhint: 'done', id: entryID,
    'aria-labelledby': labelID, 'aria-describedby': `${hintID} ${errorID}`});
  const box = h('div', {class: 'tags', dataset: {tags: '', field: 'tags'}, onclick: (event) => { if (event.target === box) entry.focus(); }});
  const error = h('span', {class: 'field-error', id: errorID, hidden: true, role: 'alert'});
  const changed = () => { render(); box.dispatchEvent(new Event('tags', {bubbles: true})); };
  const add = (raw) => {
    const value = raw.trim();
    if (value === '') return true;
    if (byteLength(value) > 255) { box.showError(t('controls.tags.maximum')); return false; }
    if (tags.includes(value)) { box.showError(t('controls.tags.duplicate', {value})); return false; }
    tags.push(value);
    box.showError('');
    return true;
  };
  box.getValue = () => tags;
  box.showError = (message) => {
    error.textContent = message;
    error.hidden = !message;
    if (message) entry.setAttribute('aria-invalid', 'true');
    else entry.removeAttribute('aria-invalid');
  };
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
      h('button', {type: 'button', 'aria-label': t('controls.tags.remove', {tag}), onclick: () => { tags = tags.filter((item) => item !== tag); changed(); entry.focus(); }}, icon('x')))), entry);
    entry.placeholder = t(tags.length ? 'controls.tags.add' : 'controls.tags.placeholder');
  }
  render();
  return h('div', {class: 'field'}, h('label', {for: entry.id, class: 'field-label', id: labelID}, t('controls.tags.label')), box,
    h('span', {class: 'hint', id: hintID}, t('controls.tags.hint')), error);
}

