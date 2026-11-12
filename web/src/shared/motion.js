// CSS 与 JS 共用同一组时长和缓动；仅在启动动效时读取。
// 读不到变量时（测试环境或样式未加载）返回 0 时长，避免 WAAPI 因 NaN 抛错。
export function motionTiming(duration, easing) {
  const style = typeof getComputedStyle === 'function' ? getComputedStyle(document.documentElement) : null;
  const value = style?.getPropertyValue(duration).trim() || '';
  const milliseconds = Number.parseFloat(value) * (value.endsWith('ms') ? 1 : 1000);
  return {duration: Number.isFinite(milliseconds) ? milliseconds : 0,
    easing: style?.getPropertyValue(easing).trim() || 'ease'};
}
