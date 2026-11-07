import {svg} from './dom.js';

export function orbitSVG({className, viewBox, center, squash, tilt, radii, verticalOffset = center * (1 - squash), beaconRadius = 0, satellites = []}) {
  const root = svg('svg', {viewBox, class: className, 'aria-hidden': 'true'});
  const plane = svg('g', {transform: `rotate(${tilt} ${center} ${center}) translate(0 ${verticalOffset}) scale(1 ${squash})`});
  const rings = radii.map((radius, index) => {
    const ring = svg('g', {class: `ring r${index + 1}`});
    ring.append(svg('circle', {class: 'path', cx: center, cy: center, r: radius}));
    plane.append(ring);
    return ring;
  });
  for (const {ring, x, y, radius = 5.5, verticalRadius = radius / squash, className: satelliteClass = 'sat'} of satellites) {
    rings[ring].append(svg('ellipse', {class: satelliteClass, cx: x, cy: y, rx: radius, ry: verticalRadius}));
  }
  root.append(plane);
  if (beaconRadius) {
    const beacon = svg('g', {class: 'beacon'});
    beacon.append(svg('circle', {class: 'halo', cx: center, cy: center, r: beaconRadius}), svg('circle', {class: 'core', cx: center, cy: center, r: beaconRadius}));
    root.append(beacon);
  }
  return {root, rings};
}
