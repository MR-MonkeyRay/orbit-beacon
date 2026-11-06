// CSS 与 JS 共用同一组时长和缓动；仅在启动动效时读取。
export function motionTiming(duration, easing) {
  const style = getComputedStyle(document.documentElement);
  const value = style.getPropertyValue(duration).trim();
  return {duration: Number.parseFloat(value) * (value.endsWith('ms') ? 1 : 1000),
    easing: style.getPropertyValue(easing).trim()};
}
