import {nextOffset} from '../api.js';

// Keep token text out of URLs and quote it as one literal shell argument.
export const shellQuote = (value) => `'${String(value).replaceAll("'", "'\\''")}'`;

export function enrollmentCommand(token) {
  const path = '/var/lib/orbitbeacon-agent/enrollment.token';
  return `install -m 0600 /dev/null ${path} &&\nprintf '%s\\n' ${shellQuote(token)} > ${path} &&\norbitbeacon-agent enroll --install-policy /etc/orbitbeacon/install-policy.json --token-file ${path}`;
}

// The installer reads the token from stdin; printf is a shell builtin, so the
// token never appears in a process argument list.
export function installerCommand(token, origin) {
  return `printf '%s\\n' ${shellQuote(token)} |\nsudo sh ./install-agent.sh --server ${shellQuote(origin)} --version "$VERSION"`;
}

export function enrollmentCountdown(expiresAt, now = Date.now()) {
  const seconds = Math.max(0, Math.ceil((Date.parse(expiresAt) - now) / 1000));
  if (!Number.isFinite(seconds) || seconds === 0) return 'Expired';
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')} remaining`;
}

export function enrollmentState(ticket, now = Date.now()) {
  if (ticket.revoked_at) return 'Revoked';
  if (Date.parse(ticket.expires_at) <= now) return 'Expired';
  if (ticket.uses >= ticket.max_uses) return 'Used';
  return 'Active';
}

// Filter after pagination: node_id is not a supported server query parameter.
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
