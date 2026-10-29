import {svg} from '../shared/dom.js';

// The fleet drawn as satellites around a beacon. Rings sit on a tilted plane
// (a squashed group); each ring rotates in CSS, and each satellite counter-rotates
// so its vertically pre-stretched ellipse stays round after the squash.
const CENTER = 200;
const SQUASH = 0.45;
const RADII = [92, 140, 188];
const SAT = 5.5;

export function createConstellation(container) {
  // The tilted outer ring spans roughly y 100–300, so the view box is cropped to it.
  const root = svg('svg', {viewBox: '0 86 400 228', class: 'orbits idle'});
  const plane = svg('g', {transform: `rotate(-18 ${CENTER} ${CENTER}) translate(0 ${CENTER * (1 - SQUASH)}) scale(1 ${SQUASH})`});
  const rings = RADII.map((radius, index) => {
    const ring = svg('g', {class: `ring r${index + 1}`});
    ring.append(svg('circle', {class: 'path', cx: CENTER, cy: CENTER, r: radius}));
    plane.append(ring);
    return ring;
  });
  const beacon = svg('g', {class: 'beacon'});
  beacon.append(svg('circle', {class: 'halo', cx: CENTER, cy: CENTER, r: 14}), svg('circle', {class: 'core', cx: CENTER, cy: CENTER, r: 14}));
  root.append(plane, beacon);
  container.append(root);
  let satellites = [];
  // The orbit animations never stop on their own, so they pause while the
  // drawing is scrolled away or the page is hidden.
  let onScreen = true;
  const pause = () => root.classList.toggle('paused', !onScreen || document.hidden);
  if (typeof IntersectionObserver === 'function') {
    new IntersectionObserver(([entry]) => { onScreen = entry.isIntersecting; pause(); }).observe(root);
  }
  document.addEventListener('visibilitychange', pause);

  return {
    update(nodes) {
      root.classList.toggle('idle', nodes.length === 0);
      if (satellites.length !== nodes.length) {
        for (const satellite of satellites) satellite.remove();
        satellites = nodes.map((_, index) => {
          const ring = index % RADII.length;
          // Golden-angle spacing keeps satellites apart at any count.
          const angle = (index * 137.508 + ring * 40) * Math.PI / 180;
          const satellite = svg('ellipse', {
            cx: (CENTER + RADII[ring] * Math.cos(angle)).toFixed(1),
            cy: (CENTER + RADII[ring] * Math.sin(angle)).toFixed(1),
            rx: SAT, ry: (SAT / SQUASH).toFixed(1)
          });
          rings[ring].append(satellite);
          return satellite;
        });
      }
      nodes.forEach((node, index) => satellites[index].setAttribute('class', `sat ${node.status}`));
    },
    clear() { this.update([]); }
  };
}
