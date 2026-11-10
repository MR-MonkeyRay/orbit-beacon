import {t} from '../i18n.js';
import {nextOffset} from '../api.js';

// 将 Origin 作为单个字面量 shell 参数引用。
export const shellQuote = (value) => `'${String(value).replaceAll("'", "'\\''")}'`;

export function installerCommand(origin, version, token) {
  if (!/^[0-9]+\.[0-9]+\.[0-9]+(?:[.-][0-9A-Za-z.-]+)?$/.test(version || '')) return '';
  const url = `https://raw.githubusercontent.com/mr-monkeyray/orbit-beacon/v${version}/deploy/install-agent.sh`;
  return `(
  set +x
  set -eu
  umask 077
  work=$(mktemp -d)
  trap 'rm -rf -- "$work"' 0
  trap 'exit 1' HUP INT TERM
  curl --fail --silent --show-error --location --proto '=https' \\
    --proto-redir '=https' --tlsv1.2 --output "$work/install-agent.sh" ${shellQuote(url)}
  if [ "$(id -u)" -eq 0 ]; then
    printf '%s\\n' ${shellQuote(token)} | sh "$work/install-agent.sh" --server ${shellQuote(origin)} --version ${shellQuote(version)}
  else
    sudo -v
    printf '%s\\n' ${shellQuote(token)} | sudo -n sh "$work/install-agent.sh" --server ${shellQuote(origin)} --version ${shellQuote(version)}
  fi
)`;
}

export function enrollmentCountdown(expiresAt, now = Date.now()) {
  const seconds = Math.max(0, Math.ceil((Date.parse(expiresAt) - now) / 1000));
  if (!Number.isFinite(seconds) || seconds === 0) return t('nodes.enrollment.expired');
  return t('nodes.enrollment.remaining', {time: `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`});
}

export function enrollmentState(ticket, now = Date.now()) {
  if (ticket.revoked_at) return t('nodes.enrollment.revoked');
  if (ticket.uses > 0) return t('nodes.enrollment.used');
  if (Date.parse(ticket.expires_at) <= now) return t('nodes.enrollment.expired');
  return t('nodes.enrollment.active');
}

// 在分页后过滤：node_id 不是服务端支持的查询参数。
export async function nodeEnrollments(nodeID, request) {
  const tickets = new Map();
  for (let offset = 0; ;) {
    const page = await request(`/enrollments?limit=100&offset=${offset}`);
    for (const ticket of page.enrollments) if (ticket.node_id === nodeID) tickets.set(ticket.id, ticket);
    if (!page.has_more) break;
    offset = nextOffset(page, offset);
  }
  return [...tickets.values()];
}
