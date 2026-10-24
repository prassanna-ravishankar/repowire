import { DurableObject } from 'cloudflare:workers';

// How long an agent's tool call waits for the daemon before failing.
const CALL_TIMEOUT_MS = 60_000;

export interface GatewayCall { id: string; agent: string; tool: string; args: Record<string, unknown> }
interface Pending { socket: WebSocket; resolve(text: string): void; reject(error: Error): void }

/**
 * One owner's link to their Repowire daemon. The daemon dials in and holds a
 * hibernating WebSocket; agents' mesh tool calls go out over it and the
 * daemon runs them as the calling hosted peer. Protocol pings are answered by
 * the runtime, so an idle link never wakes this object.
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
    return new Response(null, { status: 101, webSocket: client });
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

  webSocketMessage(_socket: WebSocket, message: string | ArrayBuffer) {
    if (typeof message !== 'string') return;
    const frame = JSON.parse(message) as { type?: string; id?: string; text?: string; error?: boolean };
    if (frame.type !== 'result' || typeof frame.id !== 'string') return;
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
