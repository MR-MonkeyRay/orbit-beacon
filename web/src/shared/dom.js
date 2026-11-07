// 控制台与公开页共用的极简元素工厂。
const SVG = 'http://www.w3.org/2000/svg';
let motionQuery;
export const reducedMotion = {
  get matches() {
    motionQuery ||= typeof matchMedia === 'function' ? matchMedia('(prefers-reduced-motion: reduce)') : {matches: false};
    return motionQuery.matches;
  }
};

// h('button', {type: 'button', onclick}, 'Label')：已知 DOM property 直接赋值，
// aria-* 等其余项设为 attribute，dataset 写入 data-* 属性。
// 先追加子元素，使 select.value 等属性能作用于它们。
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

function append(parent, children) {
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    parent.append(child instanceof Node ? child : String(child));
  }
  return parent;
}

// 引用构建时内联到每个 HTML 外壳中的 sprite 里的 symbol。
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
