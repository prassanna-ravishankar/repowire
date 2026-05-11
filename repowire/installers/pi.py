"""Pi (pi.dev) extension installer.

Pi is an opencode-shaped runtime: integration is via TypeScript extensions
auto-loaded from ~/.pi/agent/extensions/, not subprocess hooks. See
docs/latest/extensions on pi.dev. The extension uses pi.on() event handlers
and pi.registerTool() to register the same mesh tools other runtimes expose
(list_peers, ask, ack, notify_peer, broadcast, whoami, set_description,
set_circle).

Per-session peer registration follows the same pattern as installers/opencode.py:
each root pi session gets its own PeerConn (own WebSocket, own peer_id), with
peer_ids cached at ~/.cache/repowire/pi-peer-ids.json so they survive process
restarts. Pi's session_start event fires eagerly at process boot with
reason "startup", so no post_spawn_warmup nudge is needed (unlike codex).

Pi has no MCP server config surface, so the daemon's MCP server is not
installed for pi peers. All tools are carried by the extension directly.
"""

from __future__ import annotations

from pathlib import Path

PLUGIN_CONTENT = r"""import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Type } from "@sinclair/typebox";
import * as fs from "node:fs";
import * as path from "node:path";
import * as os from "node:os";

interface PeerInfo {
  name: string;
  status: string;
  machine?: string;
  path?: string;
}

interface PendingQuery {
  correlationId: string;
  buffer: string[];
  hasError: boolean;
  errorPayload: unknown;
  timeoutHandle: ReturnType<typeof setTimeout>;
}

// Per-session peer connection. Each root session in the pi process gets its
// own PeerConn (its own WebSocket, peer_id, busy state, pending queries).
interface PeerConn {
  sessionId: string;
  peerId: string | null;
  peerName: string;
  ws: WebSocket | null;
  pendingQueries: Map<string, PendingQuery>;
  busy: boolean;
  reconnectTimeout: ReturnType<typeof setTimeout> | null;
  reconnectAttempts: number;
  closed: boolean;
  // Tracks the currently-streaming correlation. message_update deltas and
  // turn_end finalize get routed to this pending query.
  activeTurnCorrelationId: string | null;
}

const DAEMON_URL = process.env.REPOWIRE_DAEMON_URL || "http://127.0.0.1:8377";
const DAEMON_WS_URL = process.env.REPOWIRE_DAEMON_WS_URL || "ws://127.0.0.1:8377/ws";
const AUTH_TOKEN = process.env.REPOWIRE_AUTH_TOKEN || "";
const QUERY_TIMEOUT_MS = 120_000;
const MAX_RECONNECT_ATTEMPTS = 50;

// Module state (process-wide, not per-session).
let projectPath: string = process.cwd();
let circle: string = "default";
let tmuxSession: string | undefined = undefined;
let tmuxPane: string | undefined = undefined;

// Per-session registries.
const peerBySession = new Map<string, PeerConn>();

// peer_id persistence: pi may restart, and any in-memory peer_id is lost.
// Cache per (projectPath, sessionId) so each session reuses its peer_id
// across restarts (same approach as opencode-peer-ids.json).
const PEER_ID_CACHE_PATH = path.join(os.homedir(), ".cache", "repowire", "pi-peer-ids.json");

function cacheKey(projectPath: string, sessionId: string): string {
  return projectPath + "#" + sessionId;
}

function loadPeerId(projectPath: string, sessionId: string): string | null {
  try {
    if (!fs.existsSync(PEER_ID_CACHE_PATH)) return null;
    const raw = fs.readFileSync(PEER_ID_CACHE_PATH, "utf-8");
    const data = JSON.parse(raw) as Record<string, string>;
    return data[cacheKey(projectPath, sessionId)] ?? null;
  } catch (e) {
    console.debug("[repowire] Failed to load peer_id cache:", e);
    return null;
  }
}

function savePeerId(projectPath: string, sessionId: string, id: string): void {
  try {
    fs.mkdirSync(path.dirname(PEER_ID_CACHE_PATH), { recursive: true });
    let data: Record<string, string> = {};
    if (fs.existsSync(PEER_ID_CACHE_PATH)) {
      try {
        data = JSON.parse(fs.readFileSync(PEER_ID_CACHE_PATH, "utf-8")) as Record<string, string>;
      } catch {
        data = {};
      }
    }
    const key = cacheKey(projectPath, sessionId);
    if (data[key] === id) return;
    data[key] = id;
    fs.writeFileSync(PEER_ID_CACHE_PATH, JSON.stringify(data, null, 2));
  } catch (e) {
    console.debug("[repowire] Failed to save peer_id cache:", e);
  }
}

async function daemon(p: string, body?: object) {
  const res = await fetch(DAEMON_URL + p, {
    method: body ? "POST" : "GET",
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) throw new Error("Daemon error: " + res.status);
  return res.json();
}

function sanitizePeerName(name: string): string {
  return name.replace(/[^a-zA-Z0-9._-]/g, "_") || "unknown";
}

function peerNameFor(folder: string, sessionId: string, sessionName: string | null): string {
  const slug = sanitizePeerName(sessionName || sessionId.slice(-8)).slice(0, 32) || sessionId.slice(-8);
  return sanitizePeerName(folder + "-" + slug);
}

function connectPeerWebSocket(conn: PeerConn) {
  if (conn.closed) return;
  if (conn.ws?.readyState === WebSocket.OPEN) return;

  const ws = new WebSocket(DAEMON_WS_URL);
  conn.ws = ws;

  ws.onopen = () => {
    conn.reconnectAttempts = 0;
    const connectMsg: Record<string, unknown> = {
      type: "connect",
      display_name: conn.peerName,
      circle,
      backend: "pi",
      path: projectPath,
    };
    const cachedPeerId = conn.peerId || loadPeerId(projectPath, conn.sessionId);
    if (cachedPeerId) connectMsg.peer_id = cachedPeerId;
    if (tmuxSession) connectMsg.tmux_session = tmuxSession;
    if (tmuxPane) connectMsg.pane_id = tmuxPane;
    if (AUTH_TOKEN) connectMsg.auth_token = AUTH_TOKEN;
    ws.send(JSON.stringify(connectMsg));
  };

  ws.onmessage = async (event) => {
    try {
      const data = JSON.parse(event.data.toString());
      await handleDaemonMessage(conn, data);
    } catch (e) {
      console.error("[repowire] Failed to parse daemon message for " + conn.peerName + ":", e);
    }
  };

  ws.onclose = () => {
    if (conn.closed) return;
    console.debug("[repowire] WebSocket disconnected for " + conn.peerName + ", scheduling reconnect");
    schedulePeerReconnect(conn);
  };

  ws.onerror = (err) => {
    console.error("[repowire] WebSocket error for " + conn.peerName + ":", err);
  };
}

function schedulePeerReconnect(conn: PeerConn) {
  if (conn.closed) return;
  if (conn.reconnectTimeout) clearTimeout(conn.reconnectTimeout);
  conn.reconnectAttempts++;
  if (conn.reconnectAttempts > MAX_RECONNECT_ATTEMPTS) {
    console.error("[repowire] Exhausted reconnect attempts for " + conn.peerName + ", giving up");
    return;
  }
  const delay = Math.min(3000 * Math.pow(2, conn.reconnectAttempts - 1), 60000);
  conn.reconnectTimeout = setTimeout(() => connectPeerWebSocket(conn), delay);
}

function sendStatus(conn: PeerConn, status: "busy" | "idle" | "offline") {
  if (conn.ws?.readyState === WebSocket.OPEN) {
    conn.ws.send(JSON.stringify({ type: "status", status }));
  }
}

function sendResponse(conn: PeerConn, correlationId: string, text: string) {
  if (conn.ws?.readyState === WebSocket.OPEN) {
    conn.ws.send(JSON.stringify({ type: "response", correlation_id: correlationId, text }));
  }
}

function sendError(conn: PeerConn, correlationId: string, error: string) {
  if (conn.ws?.readyState === WebSocket.OPEN) {
    conn.ws.send(JSON.stringify({ type: "error", correlation_id: correlationId, error }));
  }
}

// Module-level references set in the extension factory. softInject branches
// on agent state via ctx.isIdle(): when idle, omit deliverAs; while streaming,
// use "steer" to interrupt and surface the inbound asks/notifications.
let piApi: ExtensionAPI | null = null;
let piCtx: ExtensionContext | null = null;

async function softInject(text: string): Promise<boolean> {
  if (!piApi) {
    console.warn("[repowire] No pi API available for soft inject");
    return false;
  }
  try {
    const idle = piCtx ? piCtx.isIdle() : true;
    if (idle) {
      piApi.sendUserMessage(text);
    } else {
      piApi.sendUserMessage(text, { deliverAs: "steer" });
    }
    return true;
  } catch (e) {
    console.warn("[repowire] Failed to soft-inject:", e);
    return false;
  }
}

async function handleDaemonMessage(conn: PeerConn, data: Record<string, unknown>) {
  const msgType = data.type as string;

  if (msgType === "connected") {
    if (data.session_id) {
      conn.peerId = data.session_id as string;
      console.debug("[repowire] " + conn.peerName + " connected with peer_id: " + conn.peerId);
      savePeerId(projectPath, conn.sessionId, conn.peerId);
    }
    sendStatus(conn, conn.busy ? "busy" : "idle");
  } else if (msgType === "query") {
    const correlationId = data.correlation_id as string;
    const fromPeer = data.from_peer as string;
    const text = data.text as string;
    await handleIncomingQuery(conn, correlationId, fromPeer, text);
  } else if (msgType === "ask") {
    // First-class ask: surface with [ask #cid] framing so the agent can
    // ack via the ack tool. Daemon doesn't track pickup -- open asks are
    // surfaced via Stop hook reminders until acked. Pi has no Stop hook;
    // reminder is delivered via a steer message instead.
    const correlationId = data.correlation_id as string;
    const fromPeer = (data.from_peer as string) || "unknown";
    const text = data.text as string;
    await softInject(
      "@" + fromPeer + " → " + conn.peerName + " [ask #" + correlationId + "]: " + text,
    );
  } else if (msgType === "ping") {
    if (conn.ws?.readyState === WebSocket.OPEN) {
      conn.ws.send(JSON.stringify({ type: "pong", pane_alive: true }));
    }
  } else if (msgType === "notify" || msgType === "broadcast") {
    const fromPeer = (data.from_peer as string) || "unknown";
    const text = data.text as string;
    const prefix = msgType === "broadcast" ? "[broadcast] " : "";
    await softInject("@" + fromPeer + " → " + conn.peerName + ": " + prefix + text);
  } else if (msgType === "permission_response") {
    return;
  }
}

function ensurePeer(sessionId: string, sessionName: string | null) {
  if (peerBySession.has(sessionId)) return;
  const folder = path.basename(projectPath) || "unknown";
  const conn: PeerConn = {
    sessionId,
    peerId: loadPeerId(projectPath, sessionId),
    peerName: peerNameFor(folder, sessionId, sessionName),
    ws: null,
    pendingQueries: new Map(),
    busy: false,
    reconnectTimeout: null,
    reconnectAttempts: 0,
    closed: false,
    activeTurnCorrelationId: null,
  };
  peerBySession.set(sessionId, conn);
  connectPeerWebSocket(conn);
}

function removePeer(sessionId: string) {
  const conn = peerBySession.get(sessionId);
  if (!conn) return;
  conn.closed = true;
  if (conn.reconnectTimeout) {
    clearTimeout(conn.reconnectTimeout);
    conn.reconnectTimeout = null;
  }
  for (const [, pending] of conn.pendingQueries) {
    clearTimeout(pending.timeoutHandle);
    sendError(conn, pending.correlationId, "session deleted");
  }
  conn.pendingQueries.clear();
  if (conn.ws) {
    sendStatus(conn, "offline");
    try { conn.ws.close(); } catch { /* ignore */ }
    conn.ws = null;
  }
  peerBySession.delete(sessionId);
}

// Inbound query handler. Pi's sendUserMessage triggers an agent response;
// we capture the next assistant turn via turn_start/message_update/turn_end
// events and resolve the pending query when the turn ends.
async function handleIncomingQuery(conn: PeerConn, correlationId: string, _fromPeer: string, text: string) {
  if (!piApi) {
    sendError(conn, correlationId, "Pi API not available");
    return;
  }

  // Concurrency guard: pi has a single active turn per session, so two
  // concurrent queries would overlap. Reject the second cleanly.
  if (conn.busy || conn.activeTurnCorrelationId) {
    sendError(conn, correlationId, "Session busy: another query is already in flight on this peer");
    return;
  }

  conn.busy = true;
  sendStatus(conn, "busy");
  conn.activeTurnCorrelationId = correlationId;

  const pending: PendingQuery = {
    correlationId,
    buffer: [],
    hasError: false,
    errorPayload: null,
    timeoutHandle: setTimeout(() => {
      if (conn.pendingQueries.has(correlationId)) {
        conn.pendingQueries.delete(correlationId);
        if (conn.activeTurnCorrelationId === correlationId) {
          conn.activeTurnCorrelationId = null;
        }
        sendError(conn, correlationId, "Query timed out waiting for pi response");
      }
    }, QUERY_TIMEOUT_MS),
  };
  conn.pendingQueries.set(correlationId, pending);

  try {
    piApi.sendUserMessage(text);
  } catch (e) {
    clearTimeout(pending.timeoutHandle);
    conn.pendingQueries.delete(correlationId);
    conn.activeTurnCorrelationId = null;
    const errorMsg = e instanceof Error ? e.message : String(e);
    console.error("[repowire] sendUserMessage failed for " + conn.peerName + ": " + errorMsg);
    sendError(conn, correlationId, errorMsg);
    conn.busy = false;
    sendStatus(conn, "idle");
  }
}

function flushPending(conn: PeerConn, correlationId: string) {
  const pending = conn.pendingQueries.get(correlationId);
  if (!pending) return;
  clearTimeout(pending.timeoutHandle);
  const reply = pending.buffer.join("");
  if (pending.hasError) {
    sendResponse(conn, correlationId, "Model error: " + JSON.stringify(pending.errorPayload));
  } else if (reply) {
    sendResponse(conn, correlationId, reply);
  } else {
    sendResponse(conn, correlationId, "(empty response: session ended turn without text output)");
  }
  conn.pendingQueries.delete(correlationId);
}

function cleanup() {
  for (const conn of peerBySession.values()) {
    conn.closed = true;
    if (conn.reconnectTimeout) clearTimeout(conn.reconnectTimeout);
    if (conn.ws) {
      sendStatus(conn, "offline");
      try { conn.ws.close(); } catch { /* ignore */ }
      conn.ws = null;
    }
  }
  peerBySession.clear();
}

// Resolve which PeerConn a tool call is attributed to. ctx.sessionManager
// exposes the active session id via getSessionId() (pi 0.74 ReadonlySessionManager).
// Fall back to the first registered peer if the lookup fails (subagent
// contexts, unknown shape, etc.).
function callerPeer(ctx: ExtensionContext | undefined): { peerName: string; peerId: string | null } {
  try {
    const activeId = ctx?.sessionManager?.getSessionId?.();
    if (activeId) {
      const conn = peerBySession.get(activeId);
      if (conn) return { peerName: conn.peerName, peerId: conn.peerId };
    }
  } catch {
    /* fall through */
  }
  const first = peerBySession.values().next().value as PeerConn | undefined;
  if (first) return { peerName: first.peerName, peerId: first.peerId };
  return { peerName: sanitizePeerName(path.basename(projectPath) || "unknown"), peerId: null };
}

export default async function repowireExtension(pi: ExtensionAPI) {
  piApi = pi;
  // Capture ctx from event handlers as they fire. ctx is staled by
  // newSession/fork/switchSession/reload, but repowire never invokes those,
  // so the latest captured ctx remains valid for soft-inject branching.
  function capture(_event: unknown, ctx: ExtensionContext) {
    piCtx = ctx;
  }

  // Resolve "the peer for the currently active session" from a captured ctx.
  // session_start carries no session id in pi 0.74 — we read it off ctx.sessionManager.
  function activePeerFromCtx(ctx: ExtensionContext | undefined): PeerConn | undefined {
    try {
      const sid = ctx?.sessionManager?.getSessionId?.();
      if (sid) return peerBySession.get(sid);
    } catch {
      /* fall through */
    }
    return undefined;
  }

  // Derive circle from tmux session name (matches Claude Code hooks).
  tmuxPane = process.env.TMUX_PANE;
  if (process.env.TMUX && tmuxPane) {
    try {
      const { execFileSync } = require("child_process");
      const session = execFileSync("tmux", ["display-message", "-t", tmuxPane, "-p", "#S"], { encoding: "utf-8" }).trim();
      const window = execFileSync("tmux", ["display-message", "-t", tmuxPane, "-p", "#W"], { encoding: "utf-8" }).trim();
      if (session) {
        circle = session;
        if (window) tmuxSession = session + ":" + window;
      }
    } catch (e) {
      console.warn("[repowire] Failed to derive circle from tmux:", e);
    }
  }

  // Session lifecycle. Register peer only on startup/new (not on resume/
  // reload/fork) to avoid double-registration on session-tree navigation.
  // Fork creates a new session id we'll see via a later session_start
  // with reason "new" if it becomes a root session.
  // session_start fires at boot (reason: "startup"), on resume/reload/fork
  // navigation, and on /new. SessionStartEvent carries only `reason` and an
  // optional previousSessionFile — the active session id is on ctx, not the
  // event. We register a peer only on startup/new.
  pi.on("session_start", async (event, ctx) => {
    capture(event, ctx);
    const reason = (event as { reason?: string }).reason;
    if (reason !== "startup" && reason !== "new") return;
    try {
      const sessionId = ctx.sessionManager.getSessionId?.();
      if (!sessionId) {
        console.warn("[repowire] session_start: no session id on ctx");
        return;
      }
      let sessionName: string | null = null;
      try { sessionName = ctx.sessionManager.getSessionName?.() ?? null; } catch { /* optional */ }
      ensurePeer(sessionId, sessionName);
    } catch (e) {
      console.warn("[repowire] session_start handler failed:", e);
    }
  });

  // session_shutdown carries `reason` ("quit" | "reload" | "new" | "resume" | "fork").
  // Pi tears down the extension runtime on quit/reload/new/resume/fork. Disconnect
  // the active peer cleanly.
  pi.on("session_shutdown", async (event, ctx) => {
    capture(event, ctx);
    try {
      const sessionId = ctx.sessionManager.getSessionId?.();
      if (sessionId) removePeer(sessionId);
    } catch {
      /* swallow — we're tearing down */
    }
  });

  // Scaffold for pre-compact handling. Out of scope for v1: in the future,
  // we may surface "your ask thread is about to be compacted" notifications
  // here so callers can re-issue or accept context loss. See PR body.
  pi.on("session_before_compact", async (event, ctx) => {
    capture(event, ctx);
    // no-op v1
  });

  pi.on("turn_start", async (event, ctx) => {
    capture(event, ctx);
    // Active correlation already set by handleIncomingQuery before
    // sendUserMessage. Nothing to do here unless we later support
    // detecting human-driven turns separately.
  });

  pi.on("turn_end", async (event, ctx) => {
    capture(event, ctx);
    // Finalize: route to the active session's peer only. If the turn was
    // driven by handleIncomingQuery, activeTurnCorrelationId is set.
    const conn = activePeerFromCtx(ctx);
    if (!conn) return;
    const cid = conn.activeTurnCorrelationId;
    if (cid) {
      flushPending(conn, cid);
      conn.activeTurnCorrelationId = null;
    }
    conn.busy = false;
    sendStatus(conn, "idle");
  });

  // message_update carries an assistantMessageEvent union. text_delta gives
  // us new text chunks. error type gives us the final error if streaming
  // failed (no errorMessage on message_end in pi 0.74). thinking_delta is
  // discarded — we only want answer text.
  pi.on("message_update", async (event, ctx) => {
    capture(event, ctx);
    const ame = (event as { assistantMessageEvent?: { type?: string; delta?: string; error?: unknown; reason?: string } }).assistantMessageEvent;
    if (!ame) return;
    const conn = activePeerFromCtx(ctx);
    if (!conn) return;
    const cid = conn.activeTurnCorrelationId;
    if (!cid) return;
    const pending = conn.pendingQueries.get(cid);
    if (!pending) return;
    if (ame.type === "text_delta" && typeof ame.delta === "string") {
      pending.buffer.push(ame.delta);
    } else if (ame.type === "error") {
      pending.hasError = true;
      pending.errorPayload = ame.error ?? ame.reason ?? "stream error";
    }
  });

  // beforeExit fires when the event loop is empty; cleanup is best-effort.
  // Signal handlers are one-shot and re-emit the default termination so
  // Node still exits cleanly.
  process.on("beforeExit", cleanup);
  process.once("SIGINT", () => { try { cleanup(); } finally { process.exit(130); } });
  process.once("SIGTERM", () => { try { cleanup(); } finally { process.exit(143); } });

  // Tools. Pi's parameter schema uses TypeBox; we use bare-shape objects
  // here for portability since pi's docs show both patterns. If pi
  // strictly requires TypeBox, swap in Type.Object().
  pi.registerTool({
    name: "list_peers",
    label: "Repowire: list peers",
    description: "List all available peers in the mesh network",
    parameters: Type.Object({}),
    async execute(_id, _params, _signal, _onUpdate, ctx) {
      const result = await daemon("/peers");
      const peers = result.peers || [];
      const rows = ["peer_id\tname\tproject\tcircle\tstatus\tpath\tdescription"];
      for (const p of peers) {
        const project = p.metadata?.project || "";
        rows.push([p.peer_id || "", p.display_name || p.name || "", project, p.circle || "", p.status || "", p.path || "", p.description || ""].join("\t"));
      }
      return { content: [{ type: "text", text: rows.join("\n") }], details: undefined };
    },
  });

  pi.registerTool({
    name: "ask",
    label: "Repowire: ask peer",
    description: "Open a non-blocking ask thread with a peer. Returns a correlation_id immediately. The peer responds via ack(corr_id) (bare close) or ack(corr_id, message) (reply, delivered as a notification framed [ack #cid from @peer] message).",
    parameters: Type.Object({
      peer_name: Type.String({ description: "Name of the peer to ask" }),
      query: Type.String({ description: "The question to ask" }),
      reply_to: Type.Optional(Type.String({ description: "If set, closes that prior ask before opening this one" })),
    }),
    async execute(_id, params, _signal, _onUpdate, ctx) {
      const me = callerPeer(ctx);
      const body: Record<string, unknown> = {
        from_peer: me.peerName,
        to_peer: params.peer_name,
        text: params.query,
      };
      if (params.reply_to) body.reply_to = params.reply_to;
      const result = await daemon("/ask", body);
      if (result.error) throw new Error(result.error);
      return { content: [{ type: "text", text: result.correlation_id || "" }], details: undefined };
    },
  });

  pi.registerTool({
    name: "ack",
    label: "Repowire: ack thread",
    description: "Close an open ask thread. Bare close: ack(corr_id). Reply: ack(corr_id, message) -- delivered to the original asker.",
    parameters: Type.Object({
      correlation_id: Type.String({ description: "The ask's correlation_id" }),
      message: Type.Optional(Type.String({ description: "Optional reply content" })),
    }),
    async execute(_id, params, _signal, _onUpdate, ctx) {
      const me = callerPeer(ctx);
      const body: Record<string, unknown> = {
        correlation_id: params.correlation_id,
        from_peer: me.peerName,
      };
      if (params.message !== undefined) body.message = params.message;
      await daemon("/ack", body);
      const text = "acked #" + params.correlation_id + (params.message ? " with reply" : "");
      return { content: [{ type: "text", text }], details: undefined };
    },
  });

  pi.registerTool({
    name: "notify_peer",
    label: "Repowire: notify peer",
    description: "Send a notification to another peer (fire-and-forget)",
    parameters: Type.Object({
      peer_name: Type.String({ description: "Name of the peer" }),
      message: Type.String({ description: "The message to send" }),
    }),
    async execute(_id, params, _signal, _onUpdate, ctx) {
      const me = callerPeer(ctx);
      await daemon("/notify", {
        from_peer: me.peerName,
        to_peer: params.peer_name,
        text: params.message,
      });
      return { content: [{ type: "text", text: "Notification sent" }], details: undefined };
    },
  });

  pi.registerTool({
    name: "broadcast",
    label: "Repowire: broadcast",
    description: "Broadcast a message to all peers in the mesh",
    parameters: Type.Object({
      message: Type.String({ description: "Message to broadcast" }),
    }),
    async execute(_id, params, _signal, _onUpdate, ctx) {
      const me = callerPeer(ctx);
      const result = await daemon("/broadcast", {
        from_peer: me.peerName,
        text: params.message,
      });
      const parts: string[] = [];
      parts.push("Broadcast sent to: " + (result.sent_to?.join(", ") || "no peers"));
      if (result.failed?.length) {
        const fails = result.failed.map((f: { peer: string; error: string }) => f.peer + " (" + f.error + ")").join(", ");
        parts.push("Failed: " + fails);
      }
      return { content: [{ type: "text", text: parts.join("; ") }], details: undefined };
    },
  });

  pi.registerTool({
    name: "whoami",
    label: "Repowire: whoami",
    description: "Get information about this peer in the mesh",
    parameters: Type.Object({}),
    async execute(_id, _params, _signal, _onUpdate, ctx) {
      const me = callerPeer(ctx);
      const identifier = me.peerId || me.peerName;
      try {
        const result = await daemon("/peers/" + encodeURIComponent(identifier));
        const project = result.metadata?.project || "";
        const header = "peer_id\tname\tproject\tcircle\tstatus\tpath\tmachine\tdescription";
        const row = [result.peer_id || "", result.display_name || result.name || "", project, result.circle || "", result.status || "", result.path || "", result.machine || "", result.description || ""].join("\t");
        return { content: [{ type: "text", text: header + "\n" + row }], details: undefined };
      } catch {
        const text = "peer_id\tname\tproject\tcircle\tstatus\tpath\tmachine\tdescription\n"
          + (me.peerId || "") + "\t" + me.peerName + "\t\t\tnot registered\t\t\t";
        return { content: [{ type: "text", text }], details: undefined };
      }
    },
  });

  pi.registerTool({
    name: "set_description",
    label: "Repowire: set description",
    description: "Update your task description, visible to other peers via list_peers. Call this at the start of a task.",
    parameters: Type.Object({
      description: Type.String({ description: "Short description of your current task" }),
    }),
    async execute(_id, params, _signal, _onUpdate, ctx) {
      const me = callerPeer(ctx);
      await daemon("/peers/" + encodeURIComponent(me.peerName) + "/description", { description: params.description });
      return { content: [{ type: "text", text: "description updated: " + params.description }], details: undefined };
    },
  });

  pi.registerTool({
    name: "set_circle",
    label: "Repowire: set circle",
    description: "Join a named circle to communicate with peers in that circle. Applies to all sessions in this pi process (circle is process-wide so reconnects keep it).",
    parameters: Type.Object({
      circle: Type.String({ description: "Circle name to join (e.g., 'dev', 'frontend')" }),
    }),
    async execute(_id, params, _signal, _onUpdate, _ctx) {
      circle = params.circle;
      let sent = 0;
      for (const conn of peerBySession.values()) {
        if (conn.ws?.readyState === WebSocket.OPEN) {
          conn.ws.send(JSON.stringify({ type: "set_circle", circle: params.circle }));
          sent++;
        }
      }
      const text = sent > 0
        ? "Joined circle: " + params.circle + " (" + sent + " session peer" + (sent === 1 ? "" : "s") + ")"
        : "Circle queued: " + params.circle + " (will apply on reconnect)";
      return { content: [{ type: "text", text }], details: undefined };
    },
  });
}
"""

# Extension file location. Pi auto-discovers from
# ~/.pi/agent/extensions/*.ts (global) and .pi/extensions/*.ts (project-local).
# We install globally so every pi invocation gets the mesh tools.
GLOBAL_EXTENSION_DIR = Path.home() / ".pi" / "agent" / "extensions"
LOCAL_EXTENSION_DIR = Path(".pi") / "extensions"
EXTENSION_FILENAME = "repowire.ts"


def _get_extension_path(global_install: bool) -> Path:
    if global_install:
        return GLOBAL_EXTENSION_DIR / EXTENSION_FILENAME
    return LOCAL_EXTENSION_DIR / EXTENSION_FILENAME


def install_extension(global_install: bool = True) -> bool:
    """Install the Pi extension.

    Args:
        global_install: If True, install to ~/.pi/agent/extensions/.
                       If False, install to .pi/extensions/.

    Returns:
        True if installation successful.
    """
    extension_path = _get_extension_path(global_install)
    extension_path.parent.mkdir(parents=True, exist_ok=True)
    extension_path.write_text(PLUGIN_CONTENT)
    return True


def uninstall_extension(global_install: bool = True) -> bool:
    """Uninstall the Pi extension."""
    extension_path = _get_extension_path(global_install)
    if extension_path.exists():
        extension_path.unlink()
        return True
    return False


def check_extension_installed(global_install: bool = True) -> bool:
    """Check if the Pi extension is installed."""
    return _get_extension_path(global_install).exists()
