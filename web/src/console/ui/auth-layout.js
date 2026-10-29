import {h, svg} from '../../shared/dom.js';
import {brandMark} from '../shell.js';

// Shared bare authentication layout retains the console's existing orbit art.
function orbits() {
  const element = svg('svg', {class: 'login-orbits', viewBox: '0 0 1100 1100', 'aria-hidden': 'true'});
  element.innerHTML = '<g transform="rotate(-16 550 550) translate(0 320) scale(1 .42)">'
    + '<g class="ring r1"><circle class="path" cx="550" cy="550" r="300"/><ellipse class="sat" cx="850" cy="550" rx="7" ry="16.7"/><ellipse class="sat" cx="250" cy="550" rx="5" ry="11.9"/></g>'
    + '<g class="ring r2"><circle class="path" cx="550" cy="550" r="420"/><ellipse class="sat" cx="550" cy="130" rx="8" ry="19.0"/></g>'
    + '<g class="ring r3"><circle class="path" cx="550" cy="550" r="540"/><ellipse class="sat" cx="10" cy="550" rx="6" ry="14.3"/><ellipse class="sat" cx="932" cy="932" rx="5" ry="11.9"/></g></g>';
  return element;
}

export function authLayout({title, description, content, footer}) {
  return h('section', {class: 'login'}, orbits(),
    h('div', null,
      h('div', {class: 'login-card'},
        h('div', {class: 'brand'}, brandMark(), h('span', null, 'OrbitBeacon')),
        h('h1', {tabindex: '-1'}, title),
        h('p', null, description), content),
      footer ? h('p', {class: 'login-foot'}, footer) : null));
}
