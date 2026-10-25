import { DurableObject } from 'cloudflare:workers';

// How long an agent's tool call waits for the daemon before failing.
const CALL_TIMEOUT_MS = 60_000;

export interface GatewayCall { id: string; agent: string; tool: string; args: Record<string, unknown> }
export interface GatewayReport { agent: string; operationId: string; status: string; reason?: string; text?: string }

// Keep an outbox entry well inside the storage value limit.
const REPORT_TEXT_LIMIT = 64_000;
interface Pending { socket: WebSocket; resolve(text: string): void; reject(error: Error): void }

/**
 * One owner's link to their Repowire daemon. The daemon dials in and holds a
 * hibernating WebSocket; agents' mesh tool calls go out over it and the
 * daemon runs them as the calling hosted peer. Settled runs are reported
 * through a durable outbox: sent at once when the daemon is connected, kept
 * otherwise, and resent on every connect until the daemon acknowledges them.
 * Protocol pings are answered by the runtime, so an idle link never wakes
 * this object.
 */
export class Gateway extends DurableObject {
  private pending = new Map<string, Pending>();

  async fetch(request: Request): Promise<Response> {
    if (request.headers.get('Upgrade') !== 'websocket') return new Response('Expected WebSocket', { status: 426 });
    // One daemon per owner: a reconnect replaces a socket that may be half open.
    for (const old of this.ctx.getWebSockets()) {
      this.webSocketClose(old);
      old.close(1012, 'Replaced by a newer daemon connection');
    }
    const [client, server] = Object.values(new WebSocketPair());
    this.ctx.acceptWebSocket(server);
    for (const [id, report] of await this.ctx.storage.list<GatewayReport>({ prefix: 'outbox:' })) {
      server.send(JSON.stringify({ type: 'settled', id, ...report }));
    }
    return new Response(null, { status: 101, webSocket: client });
  }

  async report(report: GatewayReport) {
    const id = `outbox:${report.agent}:${report.operationId}`;
    const stored = { ...report, text: report.text?.slice(0, REPORT_TEXT_LIMIT) };
    await this.ctx.storage.put(id, stored);
    this.ctx.getWebSockets().at(-1)?.send(JSON.stringify({ type: 'settled', id, ...stored }));
  }

  call(call: GatewayCall): Promise<string> {
    const socket = this.ctx.getWebSockets().at(-1);
    if (!socket) return Promise.reject(new Error('The Repowire daemon is not connected; mesh tools are unavailable right now'));
    const key = `${call.agent}:${call.id}`;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => this.settle(key, new Error(`The daemon did not answer ${call.tool} within 60 seconds; it may still have run`)), CALL_TIMEOUT_MS);
      this.pending.set(key, {
        socket,
        resolve: (text) => { clearTimeout(timer); resolve(text); },
        reject: (error) => { clearTimeout(timer); reject(error); },
      });
      socket.send(JSON.stringify({ type: 'call', id: key, agent: call.agent, tool: call.tool, args: call.args }));
    });
  }

  async webSocketMessage(_socket: WebSocket, message: string | ArrayBuffer) {
    if (typeof message !== 'string') return;
    const frame = JSON.parse(message) as { type?: string; id?: string; text?: string; error?: boolean };
    if (typeof frame.id !== 'string') return;
    if (frame.type === 'ack' && frame.id.startsWith('outbox:')) {
      await this.ctx.storage.delete(frame.id);
      return;
    }
    if (frame.type !== 'result') return;
    this.settle(frame.id, frame.error ? new Error(frame.text || 'Mesh tool failed') : frame.text ?? '');
  }

  webSocketClose(socket: WebSocket) {
    for (const [key, pending] of this.pending) {
      if (pending.socket === socket) this.settle(key, new Error('The daemon disconnected before answering; the call may have run'));
    }
  }

  private settle(key: string, outcome: string | Error) {
    const pending = this.pending.get(key);
    if (!pending) return;
    this.pending.delete(key);
    if (outcome instanceof Error) pending.reject(outcome);
    else pending.resolve(outcome);
  }
}
