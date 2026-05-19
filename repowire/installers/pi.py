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
import * as crypto from "node:crypto";
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
  // True once we have soft-injected the SessionStart mesh primer for this
  // session. One-shot per PeerConn lifetime so reconnects don't re-prime.
  introduced: boolean;
}

const DAEMON_URL = process.env.REPOWIRE_DAEMON_URL || "http://127.0.0.1:8377";
const DAEMON_WS_URL = process.env.REPOWIRE_DAEMON_WS_URL || "ws://127.0.0.1:8377/ws";
const AUTH_TOKEN = process.env.REPOWIRE_AUTH_TOKEN || "";
const QUERY_TIMEOUT_MS = 120_000;
const MAX_RECONNECT_ATTEMPTS = 50;
const SPAWN_HINT_TTL_MS = 300_000;

// Module state (process-wide, not per-session).
let projectPath: string = process.cwd();
let circle: string = "default";
let role: string | undefined = undefined;
let tmuxSession: string | undefined = undefined;
let tmuxPane: string | undefined = undefined;

// Spawn hint consumer. Matches repowire/spawn_hints.py: hash key is
// sha256(`${resolved_path}::${backend}`).slice(0,16), file is JSON with
// {path, backend, circle, role?, ts}. Read once at startup; delete on use.
// Uses fs.realpathSync to canonicalize symlinks the same way Python's
// Path.resolve() does — Node's path.resolve() is purely lexical and would
// produce a different hash key for symlinked workspace paths.
function consumeSpawnHint(projectPath: string, backend: string): { circle?: string; role?: string } | null {
  try {
    let resolved: string;
    try {
      resolved = fs.realpathSync(projectPath);
    } catch {
      resolved = path.resolve(projectPath);
    }
    const raw = `${resolved}::${backend}`;
    const key = crypto.createHash("sha256").update(raw).digest("hex").slice(0, 16);
    const hintPath = path.join(os.homedir(), ".cache", "repowire", "spawn-hints", `${key}.json`);
    if (!fs.existsSync(hintPath)) return null;
    const data = JSON.parse(fs.readFileSync(hintPath, "utf-8")) as {
      circle?: unknown; role?: unknown; ts?: unknown;
    };
    try { fs.unlinkSync(hintPath); } catch { /* best-effort */ }
    if (typeof data.ts !== "number") return null;
    if (Date.now() - data.ts * 1000 > SPAWN_HINT_TTL_MS) return null;
    const out: { circle?: string; role?: string } = {};
    if (typeof data.circle === "string" && data.circle) out.circle = data.circle;
    if (typeof data.role === "string" && data.role) out.role = data.role;
    return out;
  } catch (e) {
    console.debug("[repowire] consumeSpawnHint failed:", e);
    return null;
  }
}

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
  if (!res.ok) {
    let detail = "";
    try {
      detail = (await res.text()).trim();
    } catch { /* best-effort */ }
    const suffix = detail ? ": " + detail : "";
    throw new Error("Daemon error " + res.status + suffix);
  }
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
    if (role) connectMsg.role = role;
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

// Active path: inbound asks/notifications/broadcasts from peers. These are
// genuine mesh traffic and should reach the agent now — when idle, trigger a
// turn via sendUserMessage; while streaming, queue with deliverAs:"steer".
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

// Passive path: reminders and the SessionStart mesh primer. Must NEVER
// trigger a turn — otherwise pollAndRemindOpenAsks at turn_end would
// self-trigger the next turn, which would fire turn_end again, looping.
// pi.sendMessage with deliverAs:"nextTurn" queues the content as context
// for the next genuinely human-driven turn without interrupting or
// triggering anything.
function passiveInject(text: string, customType: string): boolean {
  if (!piApi) {
    console.warn("[repowire] No pi API available for passive inject");
    return false;
  }
  try {
    piApi.sendMessage(
      { customType, content: text, display: true },
      { deliverAs: "nextTurn" },
    );
    return true;
  } catch (e) {
    console.warn("[repowire] Failed to passive-inject:", e);
    return false;
  }
}

// Build a SessionStart-style mesh primer matching hooks/session_handler.py
// format_peers_context. Soft-injected once at session start so pi agents
// know which peer they are, who else is online, and how to use ask/ack.
async function buildMeshContext(myPeerName: string): Promise<string | null> {
  try {
    const result = await daemon("/peers");
    const peers = (result.peers || []) as Array<{
      name?: string; status?: string; path?: string;
      backend?: string; description?: string; metadata?: { branch?: string };
    }>;
    const others = peers.filter((p) => p.name !== myPeerName && p.status === "online");

    const lines: string[] = [];
    lines.push(
      "[Repowire Mesh] You are peer \"" + myPeerName + "\". You have access to other coding sessions working on related projects:",
    );
    if (others.length === 0) {
      lines.push("  (no other peers online)");
    } else {
      for (const p of others) {
        const branch = p.metadata?.branch ? " on " + p.metadata.branch : "";
        const projectName = path.basename(p.path || "") || p.name || "";
        const agent = p.backend || "claude-code";
        const desc = p.description ? " - " + p.description : "";
        lines.push("  - " + (p.name || "") + branch + " (" + projectName + ", " + agent + ")" + desc);
      }
    }
    lines.push("");
    lines.push(
      "IMPORTANT: When asked about these projects, ask the peer directly via ask() rather than searching locally. Use ask for tracked work that needs an explicit ack; it is non-blocking, returns a correlation_id, and the peer responds via ack(corr_id) or ack(corr_id, message). Use ask(reply_to=corr_id, ...) to chain a follow-up that closes the prior thread.",
    );
    lines.push(
      "Use notify_peer for fire-and-forget updates, reminders, and nudges. Messages from @dashboard or @telegram are from the human user - treat them like direct instructions. Use notify_peer('telegram', msg) to send updates to the user's phone; dashboard sees chat turns automatically.",
    );
    lines.push(
      "Inbound asks arrive framed as `@peer [ask #corr_id]: ...` -- you MUST close them with ack(corr_id) (bare seen-no-action) or ack(corr_id, message) (reply). Otherwise repowire will inject a reminder on your next turn. Inbound replies arrive as `[ack #corr_id from @peer] message` -- those are closures, no ack needed.",
    );
    lines.push(
      "Call set_description(\"brief task summary\") early - it becomes your title in the dashboard and peer list.",
    );
    lines.push("Peer list may be outdated - use list_peers() to refresh.");
    return lines.join("\n");
  } catch (e) {
    console.debug("[repowire] buildMeshContext failed:", e);
    return null;
  }
}

// Turn-boundary ask-reminder backstop. Equivalent to the Stop hook poll
// other backends do: fetch /asks/pending for this peer; if any open asks
// exist, softInject a compact reminder so the agent acks them on the
// next turn. Open asks reappear every turn_end until acked — no
// once-only flag, mirrors hooks/ask_lifecycle.py.
async function pollAndRemindOpenAsks(conn: PeerConn): Promise<void> {
  if (!conn.peerId) return;
  try {
    const url = DAEMON_URL + "/asks/pending?peer_id=" + encodeURIComponent(conn.peerId);
    const headers: Record<string, string> = { "Content-Type": "application/json" };
    const res = await fetch(url, { headers });
    if (!res.ok) return;
    const result = await res.json() as { asks?: Array<{ correlation_id?: string; from_peer?: string; text?: string }> };
    const asks = result.asks || [];
    if (asks.length === 0) return;
    const lines: string[] = [];
    lines.push(
      "[repowire] " + asks.length + " open ask(s). Handle each: ack(corr_id) bare if no reply needed, ack(corr_id, message) to reply.",
    );
    for (const a of asks) {
      const cid = a.correlation_id || "?";
      const fromPeer = a.from_peer || "?";
      let body = (a.text || "").trim().replace(/\n/g, " ");
      if (body.length > 150) body = body.slice(0, 149) + "…";
      const head = "  - #" + cid + " from @" + fromPeer;
      lines.push(body ? head + ": " + body : head);
    }
    passiveInject(lines.join("\n"), "repowire-ask-reminder");
  } catch (e) {
    console.debug("[repowire] pollAndRemindOpenAsks failed:", e);
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
    // SessionStart-equivalent mesh primer: tell the agent who it is, who
    // else is online, and how ask/ack works. Passive delivery (nextTurn)
    // so the primer rides the next genuine user turn rather than starting
    // an unsolicited one on connect. One-shot per PeerConn — flip
    // `introduced` only after passiveInject confirms, so a transient
    // failure leaves room for a retry on the next `connected` frame.
    if (!conn.introduced) {
      const primer = await buildMeshContext(conn.peerName);
      if (primer && passiveInject(primer, "repowire-mesh-primer")) {
        conn.introduced = true;
      }
    }
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
    introduced: false,
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

  // Consume spawn hint: recovers `role` (and `circle` as fallback when tmux
  // derivation failed) for peers spawned via `spawn_peer` (e.g. orchestrator).
  // Hint file is one-shot (deleted on read) and TTL-bounded.
  const hint = consumeSpawnHint(projectPath, "pi");
  if (hint) {
    if (hint.role) role = hint.role;
    if (hint.circle && circle === "default") circle = hint.circle;
  }

  // Session lifecycle. session_start fires at boot (reason: "startup"), on
  // resume/reload/fork navigation, and on /new. SessionStartEvent carries
  // only `reason` and an optional previousSessionFile — the active session id
  // is on ctx, not the event. We register on every reason: extensions get a
  // fresh runtime on resume/reload/fork, so without re-registering the peer
  // would never reappear in the mesh. ensurePeer is keyed by sessionId and
  // is idempotent, so re-firing for the same id is a no-op.
  pi.on("session_start", async (event, ctx) => {
    capture(event, ctx);
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
    // Turn-boundary ask-reminder backstop: mirror Stop hook behavior for
    // claude-code/codex/gemini. Open asks reappear every turn_end until
    // acked. Fire-and-forget; failures are logged but don't block the turn.
    void pollAndRemindOpenAsks(conn);
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
    description: "List reachable peers in the mesh. Use this to find peer_id/name, status, circle, path, and description before ask/notify_peer. Peer lists can be stale; refresh before targeting. Use ask/notify_peer for mesh peers, not SendMessage.",
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
    description: "Open a non-blocking ask thread with a peer. Use when work needs a tracked thread and explicit ack, such as worker status checks or reviewer checkpoints. Returns a correlation_id immediately; watch notifications for the eventual ack. Use notify_peer for fire-and-forget updates, reminders, and nudges. Do not use SendMessage for mesh peers.",
    parameters: Type.Object({
      peer_name: Type.String({ description: "Display name or peer_id of the peer to ask" }),
      query: Type.String({ description: "The question or request to send" }),
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
    description: "Send a fire-and-forget notification to another peer. Use for status updates, replies to notifications, reminders, self-wakes, and nudges that do not require closure. Special peer 'telegram' sends to the user's phone; dashboard sees chat turns automatically. Daemon-side success is not delivery confirmation; use ask when confirmed closure matters.",
    parameters: Type.Object({
      peer_name: Type.String({ description: "Display name or peer_id of the peer to notify" }),
      message: Type.String({ description: "The notification message" }),
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
    description: "Broadcast to all online peers across the mesh. Use for announcements that affect everyone, such as deployment updates or breaking changes. Do not use for replies or tracked work; use ack/ask for ask threads and notify_peer for one peer.",
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
    description: "Update your short task description, visible to other peers via list_peers. Call this at the start of a task and when your focus shifts so peers know what you are working on.",
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
    name: "spawn_peer",
    label: "Repowire: spawn peer",
    description: "Spawn a new coding session in a different project directory. The command must exactly match daemon.spawn.allowed_commands; if none are configured, spawn is disabled. The spawned agent self-registers shortly after start; use list_peers to confirm and get peer_id. Circle maps to tmux session name and cannot be reassigned after spawn. Pass message with first-turn context; codex needs it or the default warmup to register promptly. After spawn, use ask for tracked work or notify_peer for fire-and-forget prompts, not SendMessage.",
    parameters: Type.Object({
      path: Type.String({ description: "Absolute path to the project directory" }),
      command: Type.String({ description: "Command to run (must be in allowed_commands)" }),
      circle: Type.Optional(Type.String({ description: "Circle to spawn into (default: 'default')" })),
      message: Type.Optional(Type.String({ description: "Optional first-turn prompt for the spawned agent" })),
    }),
    async execute(_id, params, _signal, _onUpdate, _ctx) {
      const body: Record<string, unknown> = {
        path: params.path,
        command: params.command,
        circle: params.circle || "default",
      };
      if (params.message !== undefined) body.message = params.message;
      const result = await daemon("/spawn", body);
      const name = result.display_name as string;
      const tmux = result.tmux_session as string;
      const text = "Spawned " + name + " (tmux: " + tmux + "). Peer will self-register shortly. Use list_peers() to confirm and get peer_id. Address it as '" + name + "' via ask/notify_peer.";
      return { content: [{ type: "text", text }], details: undefined };
    },
  });

  pi.registerTool({
    name: "kill_peer",
    label: "Repowire: kill peer",
    description: "Kill a registered local coding session. The peer is always deregistered from the mesh. The tmux pane behind it is only killed if the daemon spawned the peer via spawn_peer in the current daemon lifetime. Externally attached peers, or peers whose ownership was lost across a daemon restart, are deregistered without touching tmux — verify and follow up with `tmux kill-pane` if the pane survives.",
    parameters: Type.Object({
      peer_identifier: Type.String({ description: "Peer ID or display name from list_peers" }),
      circle: Type.Optional(Type.String({ description: "Optional circle to disambiguate display names" })),
    }),
    async execute(_id, params, _signal, _onUpdate, ctx) {
      const me = callerPeer(ctx);
      const body: Record<string, unknown> = {
        peer_identifier: params.peer_identifier,
        from_peer: me.peerName,
      };
      if (params.circle !== undefined) body.circle = params.circle;
      const result = await daemon("/kill-peer", body);
      const scoped = params.circle ? " in circle " + params.circle : "";
      const tmuxKilled = result?.tmux_killed;
      let tmuxNote: string;
      if (tmuxKilled === true) {
        tmuxNote = "tmux pane killed";
      } else if (tmuxKilled === false) {
        tmuxNote = "tmux pane kill attempted but failed (verify with `tmux list-panes`)";
      } else {
        tmuxNote = "tmux pane kill skipped (daemon ownership not proven — externally attached, or daemon restarted since spawn). Verify with `tmux list-panes` and manually `tmux kill-pane` if needed.";
      }
      const text = "Killed peer " + params.peer_identifier + scoped + ": " + tmuxNote;
      return { content: [{ type: "text", text }], details: undefined };
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
