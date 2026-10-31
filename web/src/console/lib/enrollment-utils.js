import {t} from '../i18n.js';
import {nextOffset} from '../api.js';

// token 文本不进 URL，而是作为单个字面量 shell 参数引用。
export const shellQuote = (value) => `'${String(value).replaceAll("'", "'\\''")}'`;

export function enrollmentCommand(token) {
  const path = '/var/lib/orbitbeacon-agent/enrollment.token';
  return `install -m 0600 /dev/null ${path} &&\nprintf '%s\\n' ${shellQuote(token)} > ${path} &&\norbitbeacon-agent enroll --install-policy /etc/orbitbeacon/install-policy.json --token-file ${path}`;
}

// 安装脚本从 stdin 读取 token；printf 是 shell 内建命令，
// token 不会出现在进程参数列表中。
export function installerCommand(token, origin) {
  return `printf '%s\\n' ${shellQuote(token)} |\nsudo sh ./install-agent.sh --server ${shellQuote(origin)} --version "$VERSION"`;
}

export function enrollmentCountdown(expiresAt, now = Date.now()) {
  const seconds = Math.max(0, Math.ceil((Date.parse(expiresAt) - now) / 1000));
  if (!Number.isFinite(seconds) || seconds === 0) return t('nodes.enrollment.expired');
  return t('nodes.enrollment.remaining', {time: `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`});
}

export function enrollmentState(ticket, now = Date.now()) {
  if (ticket.revoked_at) return t('nodes.enrollment.revoked');
  if (Date.parse(ticket.expires_at) <= now) return t('nodes.enrollment.expired');
  if (ticket.uses >= ticket.max_uses) return t('nodes.enrollment.used');
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
