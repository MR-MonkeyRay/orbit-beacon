import {t} from '../i18n.js';

// 将 Origin 作为单个字面量 shell 参数引用。
export const shellQuote = (value) => `'${String(value).replaceAll("'", "'\\''")}'`;

export function installerCommand(origin, version, key) {
  if (!/^[0-9]+\.[0-9]+\.[0-9]+(?:[.-][0-9A-Za-z.-]+)?$/.test(version || '')) return '';
  const url = `https://raw.githubusercontent.com/mr-monkeyray/orbit-beacon/v${version}/deploy/install-agent.sh`;
  return `f=$(mktemp) && curl -fsSL --proto '=https' --tlsv1.2 -o "$f" ${shellQuote(url)} && sudo sh "$f" --server ${shellQuote(origin)} --version ${shellQuote(version)}${key === undefined ? '' : ` --key ${shellQuote(key)}`}; rm -f "$f"`;
}

export function enrollmentCountdown(expiresAt, now = Date.now()) {
  const seconds = Math.max(0, Math.ceil((Date.parse(expiresAt) - now) / 1000));
  if (!Number.isFinite(seconds) || seconds === 0) return t('nodes.enrollment.expired');
  return t('nodes.enrollment.remaining', {time: `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`});
}
