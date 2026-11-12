import {byteLength, h, icon} from '../../shared/dom.js';
import {t} from '../i18n.js';
import {errorText, fieldErrorText} from '../../shared/errors.js';
import {busy, button, iconButton, uid} from './components.js';
import {listbox} from './listbox.js';
import {toastError} from './feedback.js';

// API 限制是 UTF-8 字节数，浏览器的 maxlength 无法表达。
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

export const input = (props = {}) => h('input', {class: ['checkbox', 'radio'].includes(props.type) ? '' : 'input', type: 'text', ...props});

export function toggleSwitch(label, control) {
  control.setAttribute('role', 'switch');
  return h('label', {class: 'switch'}, control, h('span', {class: 'switch-track', 'aria-hidden': 'true'}),
    h('span', {class: 'switch-text'}, label));
}

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

// 原生 select 保留提交值和约束校验，文本输入只负责搜索和显示名称。
export function combobox(choices, value, props = {}) {
  const control = select([], '', props);
  control.hidden = true;
  const entry = input({autocomplete: 'off', dataset: {transient: ''}});
  let searchable = true;
  control.entry = entry;
  Object.defineProperty(control, 'visibleControl', {get: () => searchable ? entry : control});
  const property = (name, changed) => {
    let owner = control;
    while (owner && !Object.getOwnPropertyDescriptor(owner, name)?.get) owner = Object.getPrototypeOf(owner);
    const descriptor = owner && Object.getOwnPropertyDescriptor(owner, name);
    let fallback = control[name];
    Object.defineProperty(control, name, {configurable: true,
      get() { return descriptor ? descriptor.get.call(control) : fallback; },
      set(next) {
        if (descriptor) descriptor.set.call(control, next); else fallback = next;
        changed(next);
      }
    });
  };
  let items = [];
  const syncValue = (next) => {
    const id = String(next);
    let choice = items.find(([value]) => String(value) === id);
    if (!choice && id) {
      choice = [id, t('nodes.unknownID', {id: id.slice(0, 8)})];
      items.push(choice);
      control.append(h('option', {value: id}, choice[1]));
      // 原生 select 在未知选项插入前会把 value 置空。
      control.value = id;
      list.setChoices(items);
      return;
    }
    entry.value = id ? choice?.[1] || '' : '';
  };
  const list = listbox(entry, {emptyText: t('nodes.noMatching'), onChoose: ([id]) => {
    control.value = id;
    control.dispatchEvent(new Event('change', {bubbles: true}));
  }});
  property('value', syncValue);
  property('disabled', (disabled) => { entry.disabled = Boolean(disabled); if (disabled) list.close(); });
  property('required', (required) => entry.setAttribute('aria-required', String(Boolean(required))));
  control.disabled = Boolean(props.disabled);
  control.required = Boolean(props.required);
  control.setChoices = (next) => {
    list.close();
    items = next.map(([id, label]) => [String(id), label]);
    entry.placeholder = items.find(([id]) => !id)?.[1] || '';
    control.replaceChildren(...items.map(([id, label]) => h('option', {value: id}, label)));
    list.setChoices(items);
    control.value = '';
  };
  // 全选后输入即替换当前名称，否则搜索词会接在名称后面。
  entry.addEventListener('focus', () => entry.select());
  entry.addEventListener('click', () => { if (list.element.hidden) { entry.select(); list.open(); } });
  entry.addEventListener('blur', () => syncValue(control.value));
  control.addEventListener('change', () => syncValue(control.value));
  const nativeFocus = control.focus.bind(control);
  control.focus = (options) => searchable ? entry.focus(options) : nativeFocus(options);
  control.setSearchable = (enabled) => {
    searchable = enabled;
    control.hidden = enabled;
    entry.hidden = !enabled;
    list.close();
  };
  control.wrapper = h('div', {class: 'choice-control'}, control, entry, list.element);
  control.setChoices(choices);
  control.value = value;
  return control;
}

// 应用只提供 customError。浏览器原生消息
// 可能与所选界面语言不同。
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

// 带标签、提示和内联错误的控件。校验消息在
// 首次提交后，或用户离开非空字段时出现。
// options.inner 在布局中替换控件本身（如密码包装器）。
export function field(label, control, options = {}) {
  const visible = control.entry || control;
  if (!visible.id) visible.id = uid('field');
  if (!control.id) control.id = control.entry ? `${visible.id}-value` : visible.id;
  const hintID = options.hint ? `${visible.id}-hint` : null;
  const labelID = `${visible.id}-label`;
  if (control.entry) control.wrapper.querySelector('[role="listbox"]').setAttribute('aria-labelledby', labelID);
  const error = h('span', {class: 'field-error', id: `${visible.id}-error`, hidden: true});
  for (const target of new Set([control, visible])) target.setAttribute('aria-describedby', [hintID, error.id].filter(Boolean).join(' '));
  control.showError = (message) => {
    error.textContent = message;
    error.hidden = !message;
    for (const target of new Set([control, visible])) {
      if (message) target.setAttribute('aria-invalid', 'true');
      else target.removeAttribute('aria-invalid');
    }
  };
  control.addEventListener('invalid', (event) => {
    event.preventDefault();
    visible.classList.add('touched');
    control.showError(validationText(control));
    const active = document.activeElement;
    if (!(active && active.form === control.form && active.matches(':invalid'))) control.focus();
  });
  control.addEventListener('change', () => { if (control.entry) control.showError(validationText(control)); });
  visible.addEventListener('input', () => {
    if (visible.classList.contains('touched') || visible.hasAttribute('aria-invalid')) control.showError(validationText(control));
  });
  visible.addEventListener('blur', () => {
    if (control.value !== '') {
      visible.classList.add('touched');
      // touched 触发 .touched:invalid 的红框，但 aria-invalid 只在 showError 时设置；
      // blur 后同步一次，让读屏与视觉一致。
      control.showError(validationText(control));
    }
  });
  const check = ['checkbox', 'radio'].includes(control.type) && !options.inner;
  // aria-required 已表达必填；星号只做视觉提示，对读屏隐藏。
  // 星号放在 label 旁边而不是内部：运行期 required 会切换（通道类型、
  // 告警指标等），且部分标签整体重写 textContent，放在内部会被擦掉。
  const marker = check || options.requiredMarker === false ? null : h('span', {class: 'field-required', 'aria-hidden': 'true'}, '*');
  const syncMarker = () => { if (marker) marker.hidden = !control.required; };
  if (marker) {
    let owner = control;
    while (owner && !Object.getOwnPropertyDescriptor(owner, 'required')?.get) owner = Object.getPrototypeOf(owner);
    const walk = owner && Object.getOwnPropertyDescriptor(owner, 'required');
    let fallback = control.required;
    Object.defineProperty(control, 'required', {configurable: true,
      get() { return walk?.get ? walk.get.call(control) : fallback; },
      set(next) { if (walk?.set) walk.set.call(control, next); else fallback = next; syncMarker(); }
    });
  }
  syncMarker();
  return h('div', {class: 'field'},
    check ? h('label', {class: 'check-row', for: control.id}, control, h('strong', null, label))
      : h('div', {class: 'field-label-row'}, h('label', {for: visible.id, class: 'field-label', id: labelID}, label), marker),
    check ? null : options.inner || (control.entry ? control.wrapper : control),
    options.hint ? h('span', {class: 'text-note', id: hintID}, options.hint) : null,
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


// 将 validation_failed 字段消息应用到
// data-field 或 name 匹配的控件。返回是否有控件匹配。
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

// 提交时运行 action 并使按钮进入忙碌状态。有 errorBox 时
// 错误显示在其中（对话框位于 toast 层之上），否则显示为 toast。
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

// 没有保存栏的表单（包括密码表单）共享同一基线。
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

// 保存栏复用基线，只添加自己的按钮/备注状态。
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

// 节点标签编辑器：Enter 或逗号添加，输入为空时 Backspace 删除最后一个标签。
// 粘贴的列表会拆分并去除首尾空白；标签须为 1–255 字节且不重复。
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
  box.setValue = (values) => { tags = [...values]; entry.value = ''; box.showError(''); render(); };
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
    entry.placeholder = t('controls.tags.add');
  }
  render();
  return h('div', {class: 'field'}, h('label', {for: entry.id, class: 'field-label', id: labelID}, t('controls.tags.label')), box,
    h('span', {class: 'text-note', id: hintID}, t('controls.tags.hint')), error);
}

