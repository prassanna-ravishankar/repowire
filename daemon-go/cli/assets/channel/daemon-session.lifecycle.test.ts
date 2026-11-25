import { afterEach, describe, expect, test } from "bun:test";
import { WebSocketServer, type WebSocket } from "ws";
import { DaemonSession } from "./daemon-session";

// A fake daemon: records every connect frame and lets a test script its
// answer to each one.
function fakeDaemon(answer: (frame: Record<string, unknown>, n: number, ws: WebSocket) => void) {
  const wss = new WebSocketServer({ port: 0 });
  const frames: Record<string, unknown>[] = [];
  wss.on("connection", (ws) => {
    ws.on("message", (data) => {
      const frame = JSON.parse(data.toString());
      if (frame.type !== "connect") return;
      frames.push(frame);
      answer(frame, frames.length, ws);
    });
  });
  const port = (wss.address() as { port: number }).port;
  return { url: `ws://127.0.0.1:${port}`, frames, close: () => wss.close() };
}

const until = async (ok: () => boolean, ms = 2000) => {
  const end = Date.now() + ms;
  while (!ok() && Date.now() < end) await new Promise((r) => setTimeout(r, 10));
  return ok();
};

const refuse = (ws: WebSocket, final: boolean) => {
  ws.send(JSON.stringify({ type: "error", code: "peer_retired", error: "retired", final }));
  ws.close(4004, "Peer retired");
};

describe("DaemonSession refusals (#628)", () => {
  const cleanup: (() => void)[] = [];
  afterEach(() => {
    cleanup.splice(0).forEach((f) => f());
  });
  delete process.env.TMUX; // placement without tmux: no probe of the live server

  test("an operator close tells the session once and stops reconnecting", async () => {
    const daemon = fakeDaemon((_frame, n, ws) => {
      if (n === 1) {
        ws.send(JSON.stringify({ type: "connected", session_id: "repow-1-aaaa", display_name: "app-claude-code" }));
        ws.close(); // a drop: the session reconnects with its claim
      } else {
        refuse(ws, true);
      }
    });
    cleanup.push(daemon.close);
    const notices: string[] = [];
    new DaemonSession({ daemonUrl: daemon.url, reconnectMs: 20 }).connect(() => {}, (n) => {
      notices.push(n);
    });

    expect(await until(() => notices.length === 1)).toBe(true);
    expect(daemon.frames[1]).toMatchObject({ peer_id: "repow-1-aaaa" });
    expect(notices[0]).toContain("left the mesh");
    expect(notices[0]).toContain("repow-1-aaaa");
    await new Promise((r) => setTimeout(r, 200)); // ten reconnect intervals
    expect(daemon.frames.length).toBe(2);
    expect(notices.length).toBe(1);
  });

  test("a notice that cannot be delivered does not crash the session", async () => {
    const daemon = fakeDaemon((_frame, _n, ws) => refuse(ws, true));
    cleanup.push(daemon.close);
    let tried = 0;
    new DaemonSession({ daemonUrl: daemon.url, reconnectMs: 20 }).connect(() => {}, async () => {
      tried++;
      throw new Error("stdio closed");
    });
    expect(await until(() => tried === 1)).toBe(true);
    await new Promise((r) => setTimeout(r, 200));
    expect(daemon.frames.length).toBe(1); // still stopped
  });

  test("a soft retirement reconnects without the claim and says nothing", async () => {
    const daemon = fakeDaemon((_frame, n, ws) => {
      if (n === 1) {
        ws.send(JSON.stringify({ type: "connected", session_id: "repow-1-bbbb", display_name: "app-claude-code" }));
        ws.close();
      } else if (n === 2) {
        refuse(ws, false);
      } else {
        ws.send(JSON.stringify({ type: "connected", session_id: "repow-1-cccc", display_name: "app-claude-code" }));
      }
    });
    cleanup.push(daemon.close);
    const notices: string[] = [];
    new DaemonSession({ daemonUrl: daemon.url, reconnectMs: 20 }).connect(() => {}, (n) => {
      notices.push(n);
    });

    expect(await until(() => daemon.frames.length >= 3)).toBe(true);
    expect(daemon.frames[1]).toMatchObject({ peer_id: "repow-1-bbbb" });
    expect(daemon.frames[2]).not.toHaveProperty("peer_id");
    expect(notices).toEqual([]);
  });
});
