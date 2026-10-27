// Pure pieces of the cross-machine protocol (rfcs/cross-machine-circles.md)
// shared by the target routes, the source tools and the Gateway.

export const MANAGED = 'managed';

const ID = { ask: /^ask-(\d+)-[0-9a-f]{32}$/, notify: /^notif-(\d+)-[0-9a-f]{32}$/ };

// The creation time an id carries: <kind>-<unix seconds>-<32 hex>.
export function idTime(kind: 'ask' | 'notify', id: unknown): number | null {
  const match = typeof id === 'string' ? ID[kind].exec(id) : null;
  return match ? Number(match[1]) : null;
}

export function newRemoteID(kind: 'ask' | 'notify'): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  const hex = [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('');
  return `${kind === 'notify' ? 'notif' : 'ask'}-${Math.floor(Date.now() / 1000)}-${hex}`;
}

// A run's result in the daemon's close vocabulary: an answer is an ack with
// its text, anything else closes with the run's reason.
export function remoteResult(status: string, reason: string | null, text: string | null): [string, string | null] {
  if (status === 'done') return text ? ['ack_with_msg', text] : ['ack', null];
  return [reason ?? 'unanswered', null];
}

// How a settled ask reaches its asker, as the daemon frames it.
export function framedReply(id: string, from: string, reason: string, reply: string | null): string {
  const body = reply ?? '';
  if (reason === 'ack' || reason === 'ack_with_msg') return `[ack #${id} from @${from}] ${body}`;
  if (reason === 'declined') return `[declined #${id} from @${from}] ${body}`;
  return `[closed #${id} @${from}: ${reason}] ${body}`.trimEnd();
}
