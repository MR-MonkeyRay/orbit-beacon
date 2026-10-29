// Minimal DOM for asynchronous console panels; deliberately no HTML parsing.
export class TestElement extends EventTarget {
  constructor(tagName = '') {
    super();
    this.tagName = tagName;
    this.children = [];
    this.dataset = {};
    this.attributes = new Map();
    this.hidden = false;
    this.disabled = false;
    this.id = '';
    this.className = '';
    this.style = {};
    this.value = '';
    this.type = tagName === 'input' ? 'text' : '';
    this.name = '';
    this.checked = false;
    this.selected = false;
    this.required = false;
    this.open = false;
    this.classList = {add: (name) => { this.className += ` ${name}`; }, remove: (name) => { this.className = this.className.split(' ').filter((item) => item !== name).join(' '); },
      contains: (name) => this.className.split(' ').includes(name), toggle: (name, value) => value ? this.classList.add(name) : this.classList.remove(name)};
  }
  append(...children) {
    for (let child of children) {
      if (child instanceof TestElement) { child.remove(); child.parentElement = this; }
      else child = String(child);
      this.children.push(child);
    }
  }
  prepend(child) { this.append(child); this.children.unshift(this.children.pop()); }
  remove() {
    if (this.parentElement) this.parentElement.children = this.parentElement.children.filter((item) => item !== this);
    this.parentElement = null;
  }
  replaceChildren(...children) { this.children.forEach((child) => { if (child instanceof TestElement) child.parentElement = null; }); this.children = []; this.append(...children); }
  setAttribute(key, value) { this.attributes.set(key, String(value)); if (key === 'id') this.id = String(value); }
  getAttribute(key) { return key === 'id' ? this.id : this.attributes.get(key) ?? null; }
  removeAttribute(key) { this.attributes.delete(key); }
  hasAttribute(key) { return this.attributes.has(key); }
  get isConnected() { return this.tagName === 'body' || Boolean(this.parentElement?.isConnected); }
  set textContent(value) { this.replaceChildren(String(value)); }
  get textContent() { return this.children.map((child) => child instanceof TestElement ? child.textContent : child).join(''); }
  get firstElementChild() { return this.children.find((child) => child instanceof TestElement) || null; }
  get lastElementChild() { return this.children.findLast((child) => child instanceof TestElement) || null; }
  setSelectionRange(start, end) { this.selectionStart = start; this.selectionEnd = end; }
  contains(target) { return this === target || this.children.some((child) => child instanceof TestElement && child.contains(target)); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  querySelectorAll(selector) {
    return elements(this).slice(1).filter((element) => selector.split(',').some((part) => {
      part = part.trim();
      if (part === '[data-nav]') return element.dataset.nav !== undefined;
      const nav = part.match(/^\[data-nav="(.+)"\]$/);
      if (nav) return element.dataset.nav === nav[1];
      if (part === '[data-autofocus]') return element.dataset.autofocus !== undefined;
      if (part === '[data-tags]') return element.dataset.tags !== undefined;
      if (part === '[name]') return element.name !== '';
      const dirtyForm = part.match(/^form\[data-dirty="(true|false)"\]$/);
      if (dirtyForm) return element.tagName === 'form' && element.dataset.dirty === dirtyForm[1];
      if (part.startsWith('.')) return element.classList.contains(part.slice(1));
      if (part.startsWith('#')) return element.id === part.slice(1);
      const dataField = part.match(/^\[data-field="(.+)"\]$/);
      if (dataField) return element.dataset.field === dataField[1];
      if (part === 'input[data-node-id]') return element.tagName === 'input' && element.dataset.nodeId !== undefined;
      return element.tagName === part;
    }));
  }
  get options() { return this.children.filter((child) => child instanceof TestElement && child.tagName === 'option'); }
  get selectedOptions() { return this.options.filter((option) => option.selected); }
  get elements() { return {namedItem: (name) => elements(this).find((element) => element.name === name) || null}; }
  setCustomValidity(message) { this.validationMessage = message; }
  get validity() { return {valid: !this.validationMessage}; }
  matches(selector) { return selector === ':invalid' && !this.validity.valid; }
  showModal() { this.open = true; }
  close(value) { this.open = false; this.returnValue = value; this.dispatchEvent(new Event('close')); }
  focus() { globalThis.document.activeElement = this; }
  click() { this.dispatchEvent(new Event('click')); }
}
export const elements = (node) => [node, ...node.children.filter((child) => child instanceof TestElement).flatMap(elements)];
export const flush = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };
export function installDOM() {
  const originals = new Map();
  const observers = new Set();
  const timers = new Map();
  let timerID = 0;
  const body = new TestElement('body');
  const document = new EventTarget();
  Object.assign(document, {body, hidden: false, activeElement: null,
    createElement: (tag) => new TestElement(tag), createElementNS: (_ns, tag) => new TestElement(tag),
    getElementById: (id) => elements(body).find((element) => element.id === id) || null,
    querySelector: (selector) => body.querySelector(selector), querySelectorAll: (selector) => body.querySelectorAll(selector), documentElement: new TestElement('html')});
  const values = {CSS: {escape: (value) => value}, Node: TestElement, document, localStorage: {getItem: () => '24h', setItem() {}},
    location: {pathname: '/alerts', search: '', hash: ''},
    MutationObserver: class {
      constructor(callback) { this.callback = callback; }
      observe() { observers.add(this); }
      disconnect() { observers.delete(this); }
    },
    setInterval: (callback) => { timers.set(++timerID, callback); return timerID; },
    clearInterval: (id) => timers.delete(id)};
  for (const [key, value] of Object.entries(values)) { originals.set(key, globalThis[key]); globalThis[key] = value; }
  return {body, document, timers, mutations: () => [...observers].forEach((observer) => observer.callback()),
    restore: () => { for (const [key, value] of originals) { if (value === undefined) delete globalThis[key]; else globalThis[key] = value; } }};
}
