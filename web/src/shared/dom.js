// Minimal element factory shared by the console and the public page.
export const SVG = 'http://www.w3.org/2000/svg';
let motionQuery;
export const reducedMotion = {
  get matches() {
    motionQuery ||= typeof matchMedia === 'function' ? matchMedia('(prefers-reduced-motion: reduce)') : {matches: false};
    return motionQuery.matches;
  }
};

// h('button', {type: 'button', onclick}, 'Label'): known DOM properties are
// assigned directly, everything else (aria-*, data via `dataset`) as attributes.
// Children are appended first so properties such as select.value apply to them.
export function h(tag, props, ...children) {
  const element = document.createElement(tag);
  append(element, children);
  if (props) {
    for (const [key, value] of Object.entries(props)) {
      if (value === undefined || value === null || value === false) continue;
      if (key === 'class') element.className = value;
      else if (key === 'dataset') Object.assign(element.dataset, value);
      else if (key.startsWith('on') && typeof value === 'function') element.addEventListener(key.slice(2), value);
      else if (key.includes('-') || !(key in element)) element.setAttribute(key, value === true ? '' : value);
      else element[key] = value;
    }
  }
  return element;
}

export function append(parent, children) {
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    parent.append(child instanceof Node ? child : String(child));
  }
  return parent;
}

// References a symbol from the sprite the build inlines into each HTML shell.
export function icon(name, extra = '') {
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('class', `icon ${extra}`.trim());
  svg.setAttribute('aria-hidden', 'true');
  const use = document.createElementNS(SVG, 'use');
  use.setAttribute('href', `#i-${name}`);
  svg.append(use);
  return svg;
}

export function svg(tag, attributes = {}) {
  const element = document.createElementNS(SVG, tag);
  for (const [key, value] of Object.entries(attributes)) element.setAttribute(key, value);
  return element;
}

const utf8 = new TextEncoder();
export const byteLength = (value) => utf8.encode(value).length;
