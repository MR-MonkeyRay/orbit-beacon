import {t} from '../i18n.js';
import {nextOffset} from '../api.js';

// 将 Origin 作为单个字面量 shell 参数引用。
export const shellQuote = (value) => `'${String(value).replaceAll("'", "'\\''")}'`;

export function enrollmentCommand() {
  const path = '/opt/orbit-beacon/state/enrollment.token';
  // 子 shell 隔离变量和 trap；失败或收到信号时也恢复回显。
  return `(\n  set +x\n  set -e\n  unset token\n  trap 'stty echo' 0\n  trap 'exit 1' HUP INT TERM\n  install -m 0600 /dev/null ${path}\n  stty -echo\n  IFS= read -r token\n  stty echo\n  printf '%s\\n' "$token" > ${path}\n  unset token\n  /opt/orbit-beacon/bin/orbitbeacon-agent enroll --install-policy /opt/orbit-beacon/etc/install-policy.json --config /opt/orbit-beacon/etc/agent.json --token-file ${path}\n)`;
}

// 安装脚本在终端隐藏读取票据，命令文本不包含票据。
export function installerCommand(origin) {
  return `sudo sh ./install-agent.sh --server ${shellQuote(origin)} --version "$VERSION"`;
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
