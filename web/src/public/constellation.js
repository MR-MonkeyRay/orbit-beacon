import {svg} from '../shared/dom.js';
import {orbitSVG} from '../shared/orbit.js';

// 将集群绘制为围绕信标的卫星。圆环位于倾斜平面
// （压扁的分组）上；每个圆环用 CSS 旋转，每颗卫星反向旋转，
// 使其纵向预拉伸的椭圆在压扁后仍保持圆形。
const CENTER = 200;
const SQUASH = 0.45;
const RADII = [92, 140, 188];
const SAT = 5.5;

export function createConstellation(container) {
  // 倾斜的外环大致跨越 y 100–300，viewBox 据此裁剪。
  const {root, rings} = orbitSVG({className: 'orbits idle', viewBox: '0 86 400 228', center: CENTER, squash: SQUASH, tilt: -18, radii: RADII, beaconRadius: 14});
  container.append(root);
  let satellites = [];
  // 轨道动画不会自行停止，因此在
  // 图形滚出视口或页面隐藏时暂停。
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
          // 用黄金角分布卫星，减少聚集。
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
