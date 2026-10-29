import {svg} from '../shared/dom.js';
import {orbitSVG} from '../shared/orbit.js';

// The fleet drawn as satellites around a beacon. Rings sit on a tilted plane
// (a squashed group); each ring rotates in CSS, and each satellite counter-rotates
// so its vertically pre-stretched ellipse stays round after the squash.
const CENTER = 200;
const SQUASH = 0.45;
const RADII = [92, 140, 188];
const SAT = 5.5;

export function createConstellation(container) {
  // The tilted outer ring spans roughly y 100–300, so the view box is cropped to it.
  const {root, rings} = orbitSVG({className: 'orbits idle', viewBox: '0 86 400 228', center: CENTER, squash: SQUASH, tilt: -18, radii: RADII, beaconRadius: 14});
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
