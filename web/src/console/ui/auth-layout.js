import {t} from '../i18n.js';
import {h} from '../../shared/dom.js';
import {orbitSVG} from '../../shared/orbit.js';
import {brandMark, consoleLocaleSelect} from '../shell.js';

// Shared bare authentication layout retains the console's existing orbit art.
function orbits() {
  return orbitSVG({className: 'login-orbits', viewBox: '0 0 1100 1100', center: 550, squash: .42, tilt: -16, verticalOffset: 320,
    radii: [300, 420, 540], satellites: [
      {ring: 0, x: 850, y: 550, radius: 7, verticalRadius: 16.7}, {ring: 0, x: 250, y: 550, radius: 5, verticalRadius: 11.9},
      {ring: 1, x: 550, y: 130, radius: 8, verticalRadius: 19}, {ring: 2, x: 10, y: 550, radius: 6, verticalRadius: 14.3},
      {ring: 2, x: 932, y: 932, radius: 5, verticalRadius: 11.9}
    ]}).root;
}

export function authLayout({title, description, content, footer}) {
  return h('section', {class: 'login'}, orbits(),
    h('div', null,
      h('div', {class: 'login-card'},
        h('div', {class: 'brand'}, brandMark(), h('span', null, 'OrbitBeacon')),
        h('label', {class: 'console-locale auth-locale'}, h('span', null, t('common.language')), consoleLocaleSelect({navigation: true})),
        h('h1', {tabindex: '-1'}, title),
        h('p', null, description), content),
      footer ? h('p', {class: 'login-foot'}, footer) : null));
}
