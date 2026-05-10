"""OpenCode plugin installer."""

from __future__ import annotations

from pathlib import Path

PLUGIN_CONTENT = """import type { Plugin, PluginInput } from "@opencode-ai/plugin"
import { tool } from "@opencode-ai/plugin"

type OpenCodeClient = PluginInput["client"]
import * as fs from "node:fs"
import * as path from "node:path"
import * as os from "node:os"

// Type definitions for event properties
interface SessionEventInfo {
  id?: string
  title?: string
  parentID?: string | null
}

interface MessageEventInfo {
  id?: string
  role?: string
  sessionID?: string
  parentID?: string  // assistant messages link to the originating user message
  model?: {
    providerID: string
    modelID: string
    variant?: string
  }
  time?: { completed?: number }
  status?: string
  // `finish` distinguishes terminal from intermediate completions. When the
  // model returns "tool-calls" opencode runs the tools and starts another
  // assistant message for the same parent userMessageId, so we must not
  // resolve the pending query yet.
  // Source: packages/opencode/src/session/prompt.ts:1440-1448.
  finish?: string
  error?: unknown
}

// message.part.updated payload: { sessionID, part, time }
// where part has { id, messageID, sessionID, type ("text"), text, ... }
interface MessagePartInfo {
  id?: string
  messageID?: string
  sessionID?: string
  type?: string
  text?: string
}

// message.part.delta payload: { sessionID, messageID, partID, field, delta }
// `delta` is the new chunk only (text-delta puts the new chars in `delta`).
interface MessagePartDeltaProps {
  sessionID?: string
  messageID?: string
  partID?: string
  field?: string  // e.g. "text-delta"
  delta?: string
}

interface EventWithProperties {
  type: string
  properties?: {
    info?: SessionEventInfo | MessageEventInfo
    part?: MessagePartInfo
    sessionID?: string
    messageID?: string
    partID?: string
    field?: string
    delta?: string
    status?: { type?: string }
  }
}

interface PeerInfo {
  name: string
  status: string
  machine?: string
  path?: string
}

interface PendingQuery {
  correlationId: string
  userMessageId: string             // pre-generated user message ID (parent of the assistant reply)
  assistantMessageIds: Set<string>  // assistants seen with parentID === userMessageId (tool-call loops produce many)
  textPartIds: Set<string>          // partIDs known to be text type
  reasoningPartIds: Set<string>     // partIDs known to be reasoning (deltas ignored)
  textByPartId: Map<string, string> // accumulated text per text partID — joined in arrival order at flush
  partOrder: string[]               // insertion order of text partIDs (Maps preserve set order, but explicit for clarity)
  pendingDeltasByPart: Map<string, string>  // deltas seen before part.updated identified the part
  hasError: boolean
  errorPayload: unknown
  timeoutHandle: ReturnType<typeof setTimeout>
}

// Per-session peer connection. Each root session in the opencode server gets
// its own PeerConn (its own WebSocket, peer_id, busy state, pending queries).
interface PeerConn {
  sessionId: string
  sessionTitle: string | null
  peerId: string | null
  peerName: string
  ws: WebSocket | null
  pendingQueries: Map<string, PendingQuery>  // key: userMessageId
  pendingByAssistantId: Map<string, PendingQuery>  // populated once assistant ID known
  busy: boolean
  flushTimer: ReturnType<typeof setTimeout> | null  // deferred flush after idle (race-safe vs late part.updated)
  reconnectTimeout: ReturnType<typeof setTimeout> | null
  reconnectAttempts: number
  closed: boolean
}

// Configuration
const DAEMON_URL = process.env.REPOWIRE_DAEMON_URL || "http://127.0.0.1:8377"
const DAEMON_WS_URL = process.env.REPOWIRE_DAEMON_WS_URL || "ws://127.0.0.1:8377/ws"
const AUTH_TOKEN = process.env.REPOWIRE_AUTH_TOKEN || ""
const QUERY_TIMEOUT_MS = 120_000
const MAX_RECONNECT_ATTEMPTS = 50

// Module state (server-wide, not per-session).
let projectPath: string = ""
let serverUrl: string | null = null
let opencodeClient: OpenCodeClient | null = null
let activeModel: { providerID: string; modelID: string } | null = null
let circle: string = "default"
let tmuxSession: string | undefined = undefined
let tmuxPane: string | undefined = undefined

// Per-session registries.
const peerBySession = new Map<string, PeerConn>()

// peer_id persistence: opencode runs as a fresh node process per session, so
// any in-memory peer_id is lost on restart. We cache per (projectPath, sessionId)
// so each session reuses its peer_id across restarts (issue #81/#93).
const PEER_ID_CACHE_PATH = path.join(os.homedir(), ".cache", "repowire", "opencode-peer-ids.json")

function cacheKey(projectPath: string, sessionId: string): string {
  return `${projectPath}#${sessionId}`
}

function loadPeerId(projectPath: string, sessionId: string): string | null {
  try {
    if (!fs.existsSync(PEER_ID_CACHE_PATH)) return null
    const raw = fs.readFileSync(PEER_ID_CACHE_PATH, "utf-8")
    const data = JSON.parse(raw) as Record<string, string>
    return data[cacheKey(projectPath, sessionId)] ?? null
  } catch (e) {
    console.debug("[repowire] Failed to load peer_id cache:", e)
    return null
  }
}

function savePeerId(projectPath: string, sessionId: string, id: string): void {
  try {
    fs.mkdirSync(path.dirname(PEER_ID_CACHE_PATH), { recursive: true })
    let data: Record<string, string> = {}
    if (fs.existsSync(PEER_ID_CACHE_PATH)) {
      try {
        data = JSON.parse(fs.readFileSync(PEER_ID_CACHE_PATH, "utf-8")) as Record<string, string>
      } catch {
        data = {}
      }
    }
    const key = cacheKey(projectPath, sessionId)
    if (data[key] === id) return
    data[key] = id
    fs.writeFileSync(PEER_ID_CACHE_PATH, JSON.stringify(data, null, 2))
  } catch (e) {
    console.debug("[repowire] Failed to save peer_id cache:", e)
  }
}

// HTTP helper for daemon
async function daemon(p: string, body?: object) {
  const res = await fetch(`${DAEMON_URL}${p}`, {
    method: body ? "POST" : "GET",
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) throw new Error(`Daemon error: ${res.status}`)
  return res.json()
}

function sanitizePeerName(name: string): string {
  return name.replace(/[^a-zA-Z0-9._-]/g, "_") || "unknown"
}

function peerNameFor(folder: string, session: { id: string; title?: string | null }): string {
  const slug = sanitizePeerName(session.title || session.id.slice(-8)).slice(0, 32) || session.id.slice(-8)
  return sanitizePeerName(`${folder}-${slug}`)
}

// Open a WebSocket for a single PeerConn. Each session has its own WS.
function connectPeerWebSocket(conn: PeerConn) {
  if (conn.closed) return
  if (conn.ws?.readyState === WebSocket.OPEN) return

  const ws = new WebSocket(DAEMON_WS_URL)
  conn.ws = ws

  ws.onopen = () => {
    conn.reconnectAttempts = 0
    const connectMsg: Record<string, unknown> = {
      type: "connect",
      display_name: conn.peerName,
      circle,
      backend: "opencode",
      path: projectPath,
    }
    const cachedPeerId = conn.peerId || loadPeerId(projectPath, conn.sessionId)
    if (cachedPeerId) connectMsg.peer_id = cachedPeerId
    if (tmuxSession) connectMsg.tmux_session = tmuxSession
    if (tmuxPane) connectMsg.pane_id = tmuxPane
    if (AUTH_TOKEN) connectMsg.auth_token = AUTH_TOKEN
    ws.send(JSON.stringify(connectMsg))
  }

  ws.onmessage = async (event) => {
    try {
      const data = JSON.parse(event.data.toString())
      await handleDaemonMessage(conn, data)
    } catch (e) {
      console.error(`[repowire] Failed to parse daemon message for ${conn.peerName}:`, e)
    }
  }

  ws.onclose = () => {
    if (conn.closed) return
    console.debug(`[repowire] WebSocket disconnected for ${conn.peerName}, scheduling reconnect`)
    schedulePeerReconnect(conn)
  }

  ws.onerror = (err) => {
    console.error(`[repowire] WebSocket error for ${conn.peerName}:`, err)
  }
}

function schedulePeerReconnect(conn: PeerConn) {
  if (conn.closed) return
  if (conn.reconnectTimeout) clearTimeout(conn.reconnectTimeout)
  conn.reconnectAttempts++
  if (conn.reconnectAttempts > MAX_RECONNECT_ATTEMPTS) {
    console.error(`[repowire] Exhausted reconnect attempts for ${conn.peerName}, giving up`)
    return
  }
  const delay = Math.min(3000 * Math.pow(2, conn.reconnectAttempts - 1), 60000)
  conn.reconnectTimeout = setTimeout(() => connectPeerWebSocket(conn), delay)
}

function sendStatus(conn: PeerConn, status: "busy" | "idle" | "offline") {
  if (conn.ws?.readyState === WebSocket.OPEN) {
    conn.ws.send(JSON.stringify({ type: "status", status }))
  }
}

function sendResponse(conn: PeerConn, correlationId: string, text: string) {
  if (conn.ws?.readyState === WebSocket.OPEN) {
    conn.ws.send(JSON.stringify({ type: "response", correlation_id: correlationId, text }))
  }
}

function sendError(conn: PeerConn, correlationId: string, error: string) {
  if (conn.ws?.readyState === WebSocket.OPEN) {
    conn.ws.send(JSON.stringify({ type: "error", correlation_id: correlationId, error }))
  }
}

// Handle messages from daemon, scoped to one PeerConn (one WS).
async function handleDaemonMessage(conn: PeerConn, data: Record<string, unknown>) {
  const msgType = data.type as string

  if (msgType === "connected") {
    if (data.session_id) {
      conn.peerId = data.session_id as string
      console.debug(`[repowire] ${conn.peerName} connected with peer_id: ${conn.peerId}`)
      savePeerId(projectPath, conn.sessionId, conn.peerId)
    }
    sendStatus(conn, conn.busy ? "busy" : "idle")
  } else if (msgType === "query") {
    const correlationId = data.correlation_id as string
    const fromPeer = data.from_peer as string
    const text = data.text as string
    await handleIncomingQuery(conn, correlationId, fromPeer, text)
  } else if (msgType === "ask") {
    // First-class ask: surface with [ask #cid] framing so the agent can
    // ack via the ack tool. Daemon doesn't track pickup under the
    // simplified model — open asks are surfaced via Stop hook reminders
    // until acked.
    const correlationId = data.correlation_id as string
    const fromPeer = (data.from_peer as string) || "unknown"
    const text = data.text as string
    await softInject(
      `@${fromPeer} → ${conn.peerName} [ask #${correlationId}]: ${text}`,
    )
  } else if (msgType === "ping") {
    if (conn.ws?.readyState === WebSocket.OPEN) {
      conn.ws.send(JSON.stringify({ type: "pong", pane_alive: true }))
    }
  } else if (msgType === "notify" || msgType === "broadcast") {
    const fromPeer = (data.from_peer as string) || "unknown"
    const text = data.text as string
    const prefix = msgType === "broadcast" ? "[broadcast] " : ""
    // Soft-inject into the global TUI prompt. tui.prompt.append has no
    // per-session target, so prefix with target peer name to disambiguate
    // when the user navigates between sessions.
    await softInject(`@${fromPeer} → ${conn.peerName}: ${prefix}${text}`)
  } else if (msgType === "permission_response") {
    return
  }
}

async function softInject(text: string): Promise<boolean> {
  if (!serverUrl) {
    console.warn("[repowire] No serverUrl available for soft inject")
    return false
  }
  try {
    const res = await fetch(`${serverUrl}tui/publish`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        type: "tui.prompt.append",
        properties: { text: text + " " },
      }),
    })
    if (!res.ok) {
      console.warn(`[repowire] tui.prompt.append failed: ${res.status}`)
      return false
    }
    return true
  } catch (e) {
    console.warn("[repowire] Failed to publish tui event:", e)
    return false
  }
}

// Per-session lifecycle.
function ensurePeer(session: { id: string; title?: string | null }) {
  if (peerBySession.has(session.id)) return
  const folder = path.basename(projectPath) || "unknown"
  const conn: PeerConn = {
    sessionId: session.id,
    sessionTitle: session.title ?? null,
    peerId: loadPeerId(projectPath, session.id),
    peerName: peerNameFor(folder, session),
    ws: null,
    pendingQueries: new Map(),
    pendingByAssistantId: new Map(),
    busy: false,
    flushTimer: null,
    reconnectTimeout: null,
    reconnectAttempts: 0,
    closed: false,
  }
  peerBySession.set(session.id, conn)
  connectPeerWebSocket(conn)
}

function removePeer(sessionId: string) {
  const conn = peerBySession.get(sessionId)
  if (!conn) return
  conn.closed = true
  if (conn.reconnectTimeout) {
    clearTimeout(conn.reconnectTimeout)
    conn.reconnectTimeout = null
  }
  if (conn.flushTimer) {
    clearTimeout(conn.flushTimer)
    conn.flushTimer = null
  }
  for (const [, pending] of conn.pendingQueries) {
    clearTimeout(pending.timeoutHandle)
    sendError(conn, pending.correlationId, "session deleted")
  }
  conn.pendingQueries.clear()
  conn.pendingByAssistantId.clear()
  if (conn.ws) {
    sendStatus(conn, "offline")
    try { conn.ws.close() } catch { /* ignore */ }
    conn.ws = null
  }
  peerBySession.delete(sessionId)
}

// Inbound query handler scoped to a specific peer/session.
// https://github.com/prassanna-ravishankar/repowire/issues/74 (correlation),
// https://github.com/prassanna-ravishankar/repowire/issues/93 (per-session).
async function handleIncomingQuery(conn: PeerConn, correlationId: string, _fromPeer: string, text: string) {
  if (!opencodeClient) {
    sendError(conn, correlationId, "OpenCode client not available")
    return
  }

  // Concurrency guard: opencode has no per-session prompt mutex, so two
  // concurrent promptAsync calls would interleave streams. Reject the
  // second cleanly. Drive different sessions in parallel instead.
  if (conn.busy) {
    sendError(conn, correlationId, "Session busy: another query is already in flight on this peer")
    return
  }

  conn.busy = true
  sendStatus(conn, "busy")

  // Pre-generated user message ID. The assistant reply will have a fresh ID
  // with parentID set to this. We correlate via parentID, not via this ID
  // directly (the user message would resolve immediately on its first event,
  // which is wrong).
  const userMessageId = `msg_${correlationId}_${Date.now().toString(36)}`
  const pending: PendingQuery = {
    correlationId,
    userMessageId,
    assistantMessageIds: new Set(),
    textPartIds: new Set(),
    reasoningPartIds: new Set(),
    textByPartId: new Map(),
    partOrder: [],
    pendingDeltasByPart: new Map(),
    hasError: false,
    errorPayload: null,
    timeoutHandle: setTimeout(() => {
      if (conn.pendingQueries.has(userMessageId)) {
        conn.pendingQueries.delete(userMessageId)
        for (const aid of pending.assistantMessageIds) conn.pendingByAssistantId.delete(aid)
        sendError(conn, correlationId, "Query timed out waiting for OpenCode response")
      }
    }, QUERY_TIMEOUT_MS),
  }
  conn.pendingQueries.set(userMessageId, pending)

  try {
    const body: Record<string, unknown> = {
      messageID: userMessageId,
      parts: [{ type: "text", text }],
    }
    if (activeModel) body.model = activeModel
    await opencodeClient.session.promptAsync({
      path: { id: conn.sessionId },
      body,
    })
  } catch (e) {
    clearTimeout(pending.timeoutHandle)
    conn.pendingQueries.delete(userMessageId)
    for (const aid of pending.assistantMessageIds) conn.pendingByAssistantId.delete(aid)
    const errorMsg = e instanceof Error ? e.message : String(e)
    console.error(`[repowire] promptAsync failed for ${conn.peerName}: ${errorMsg}`)
    sendError(conn, correlationId, errorMsg)
    // Synchronous failure: the run never started, so opencode will not
    // publish session.status idle. Reset busy locally to unlock the peer.
    conn.busy = false
    sendStatus(conn, "idle")
  }
}

// Discover assistant messages whose parentID matches one of our pending
// user message IDs. Multiple assistants can share the same parent during
// tool-call loops (packages/opencode/src/session/prompt.ts:1440-1448), so
// we track the full set, not just the latest.
function trackAssistantMessage(conn: PeerConn, info: MessageEventInfo) {
  if (info.role !== "assistant" || !info.id || !info.parentID) return
  const pending = conn.pendingQueries.get(info.parentID)
  if (!pending) return
  if (info.error) {
    pending.hasError = true
    pending.errorPayload = info.error
  }
  if (pending.assistantMessageIds.has(info.id)) return
  pending.assistantMessageIds.add(info.id)
  conn.pendingByAssistantId.set(info.id, pending)
}

// message.part.updated carries the full latest part state. Track which
// partIDs are text parts so we can filter delta events to text only
// (reasoning deltas also use `field: "text"` and would otherwise leak in).
// Source: packages/opencode/src/session/processor.ts:522-573, 246-255.
function applyPartUpdated(conn: PeerConn, part: MessagePartInfo) {
  const messageId = part.messageID
  if (!messageId || !part.id) return
  const pending = conn.pendingByAssistantId.get(messageId)
  if (!pending) return

  if (part.type === "text") {
    const isNew = !pending.textPartIds.has(part.id)
    pending.textPartIds.add(part.id)
    if (isNew) pending.partOrder.push(part.id)

    const buffered = pending.pendingDeltasByPart.get(part.id) || ""
    pending.pendingDeltasByPart.delete(part.id)

    // opencode emits the initial text part with text: "" before any deltas
    // (processor.ts:522-531), then a final full snapshot at text-end. An
    // empty snapshot must NOT discard accumulated deltas, but a non-empty
    // snapshot is authoritative for THIS part only (other text parts in
    // the same pending are joined separately at flush).
    if (typeof part.text === "string" && part.text.length > 0) {
      pending.textByPartId.set(part.id, part.text)
    } else {
      const existing = pending.textByPartId.get(part.id) || ""
      pending.textByPartId.set(part.id, existing + buffered)
    }
  } else if (part.type === "reasoning") {
    pending.reasoningPartIds.add(part.id)
    pending.pendingDeltasByPart.delete(part.id)  // discard reasoning deltas
  }
}

// message.part.delta carries a chunk in `delta`. Both text and reasoning
// deltas use field "text" (processor.ts:246-255 + 534-543), so we filter
// by the partID classification we built from message.part.updated. If the
// partID is unknown (delta arrived first), buffer it under the partID and
// replay/discard once the part is identified.
function applyPartDelta(conn: PeerConn, props: MessagePartDeltaProps) {
  const messageId = props.messageID
  if (!messageId) return
  const pending = conn.pendingByAssistantId.get(messageId)
  if (!pending) return
  if (props.field !== "text" || typeof props.delta !== "string" || !props.partID) return

  if (pending.textPartIds.has(props.partID)) {
    const existing = pending.textByPartId.get(props.partID) || ""
    pending.textByPartId.set(props.partID, existing + props.delta)
    return
  }
  if (pending.reasoningPartIds.has(props.partID)) {
    return
  }
  // Unknown partID — defer until message.part.updated classifies it.
  pending.pendingDeltasByPart.set(
    props.partID,
    (pending.pendingDeltasByPart.get(props.partID) || "") + props.delta,
  )
}

// Resolve all pending queries on this peer. Called via deferred flush when
// session.status reports idle — the brief delay protects against a race
// where session.status (direct publish) arrives before the final
// message.part.updated (sync-published) for the answer text.
const FLUSH_DEFER_MS = 100

function scheduleFlush(conn: PeerConn) {
  // Snapshot which queries are eligible to flush at idle. A new query
  // accepted during the deferred window must NOT be flushed by this timer
  // — it gets its own idle cycle.
  const eligible = Array.from(conn.pendingQueries.keys())
  if (conn.flushTimer) clearTimeout(conn.flushTimer)
  conn.flushTimer = setTimeout(() => {
    conn.flushTimer = null
    flushPendingNow(conn, eligible)
  }, FLUSH_DEFER_MS)
}

function flushPendingNow(conn: PeerConn, userMessageIds?: string[]) {
  const ids = userMessageIds ?? Array.from(conn.pendingQueries.keys())
  if (ids.length === 0) return
  for (const userMessageId of ids) {
    const pending = conn.pendingQueries.get(userMessageId)
    if (!pending) continue
    clearTimeout(pending.timeoutHandle)
    for (const aid of pending.assistantMessageIds) conn.pendingByAssistantId.delete(aid)

    // Concatenate all text parts in arrival order. Tool-call loops produce
    // multiple text parts; we want them all, joined with a blank line so
    // the reply reads like a continuous response.
    const chunks: string[] = []
    for (const partId of pending.partOrder) {
      const text = pending.textByPartId.get(partId)
      if (text) chunks.push(text)
    }
    const reply = chunks.join("\\n\\n")

    if (pending.hasError) {
      sendResponse(conn, pending.correlationId, `Model error: ${JSON.stringify(pending.errorPayload)}`)
    } else if (reply) {
      sendResponse(conn, pending.correlationId, reply)
    } else if (pending.assistantMessageIds.size === 0) {
      // No assistant ever parented on our userMessageId. This typically
      // means a concurrent TUI/API prompt landed just after ours, the
      // runner picked their user message as `lastUser`, and our user got
      // absorbed without its own assistant turn (prompt.ts:1412-1515).
      sendError(
        conn,
        pending.correlationId,
        "Query did not produce a dedicated assistant turn (likely a concurrent prompt absorbed it)",
      )
    } else {
      sendResponse(conn, pending.correlationId, "(empty response: session went idle without output)")
    }
    conn.pendingQueries.delete(userMessageId)
  }
}

// Tool caller attribution: ctx.sessionID identifies the calling session
// (packages/plugin/src/tool.ts:4-21,32-36 in opencode). Map it to the
// PeerConn so from_peer reflects the actual session, not a project-wide alias.
function callerPeer(ctx: { sessionID?: string } | undefined): { peerName: string; peerId: string | null } {
  const sid = ctx?.sessionID
  const conn = sid ? peerBySession.get(sid) : undefined
  if (conn) return { peerName: conn.peerName, peerId: conn.peerId }
  // Subagent or unknown context: pick first available root peer as fallback.
  const fallback = peerBySession.values().next().value as PeerConn | undefined
  if (fallback) return { peerName: fallback.peerName, peerId: fallback.peerId }
  return { peerName: sanitizePeerName(path.basename(projectPath) || "unknown"), peerId: null }
}

function cleanup() {
  for (const conn of peerBySession.values()) {
    conn.closed = true
    if (conn.reconnectTimeout) clearTimeout(conn.reconnectTimeout)
    if (conn.flushTimer) clearTimeout(conn.flushTimer)
    if (conn.ws) {
      sendStatus(conn, "offline")
      try { conn.ws.close() } catch { /* ignore */ }
      conn.ws = null
    }
  }
  peerBySession.clear()
}

// Main plugin export
export const RepowirePlugin: Plugin = async ({ client, directory, ...rest }) => {
  projectPath = directory
  opencodeClient = client

  const su = (rest as { serverUrl?: URL | string }).serverUrl
  if (su) serverUrl = typeof su === "string" ? su : su.toString()

  // Derive circle from tmux session name (matches Claude Code hooks).
  tmuxPane = process.env.TMUX_PANE
  if (process.env.TMUX && tmuxPane) {
    try {
      const { execFileSync } = require("child_process")
      const session = execFileSync("tmux", ["display-message", "-t", tmuxPane, "-p", "#S"], { encoding: "utf-8" }).trim()
      const window = execFileSync("tmux", ["display-message", "-t", tmuxPane, "-p", "#W"], { encoding: "utf-8" }).trim()
      if (session) {
        circle = session
        if (window) tmuxSession = `${session}:${window}`
      }
    } catch (e) {
      console.warn("[repowire] Failed to derive circle from tmux:", e)
    }
  }

  // Bootstrap: enumerate existing root sessions on next tick, AFTER opencode
  // finishes its own startup. Awaiting client.session.list() here would
  // deadlock — the plugin loader blocks server startup, but session.list()
  // hits opencode's /session endpoint which isn't ready until startup
  // completes. setTimeout(0) yields control back to opencode's loader.
  setTimeout(() => {
    void (async () => {
      try {
        const result = await client.session.list()
        const sessions = result?.data
        if (Array.isArray(sessions)) {
          for (const s of sessions) {
            if (s && s.parentID == null && typeof s.id === "string") {
              ensurePeer({ id: s.id, title: (s as { title?: string }).title })
            }
          }
        }
      } catch (e) {
        console.warn("[repowire] Failed to enumerate sessions on bootstrap:", e)
      }
    })()
  }, 0)

  // beforeExit fires when the event loop is empty; cleanup is best-effort.
  // For signals, install a one-shot handler that cleans up and re-emits the
  // default behavior (process.exit). Without `once`, our handler would
  // override Node's default termination behavior on SIGINT/SIGTERM.
  process.on("beforeExit", cleanup)
  process.once("SIGINT", () => { try { cleanup() } finally { process.exit(130) } })
  process.once("SIGTERM", () => { try { cleanup() } finally { process.exit(143) } })

  return {
    tool: {
      list_peers: tool({
        description: "List all available peers in the mesh network",
        args: {},
        async execute(_args, _ctx) {
          const result = await daemon("/peers")
          const peers = result.peers || []
          const rows = ["peer_id\tname\tproject\tcircle\tstatus\tpath\tdescription"]
          for (const p of peers) {
            const project = p.metadata?.project || ""
            rows.push([p.peer_id || "", p.display_name || p.name || "", project, p.circle || "", p.status || "", p.path || "", p.description || ""].join("\\t"))
          }
          return rows.join("\\n")
        },
      }),
      ask: tool({
        description: "Open a non-blocking ask thread with a peer. Returns a correlation_id immediately. The peer responds via ack(corr_id) (bare close) or ack(corr_id, message) (reply, delivered as a notification framed [ack #cid from @peer] message).",
        args: {
          peer_name: tool.schema.string().describe("Name of the peer to ask"),
          query: tool.schema.string().describe("The question to ask"),
          reply_to: tool.schema.string().optional().describe("If set, closes that prior ask before opening this one"),
        },
        async execute({ peer_name, query, reply_to }, ctx) {
          const me = callerPeer(ctx as { sessionID?: string })
          const body: any = {
            from_peer: me.peerName,
            to_peer: peer_name,
            text: query,
          }
          if (reply_to) body.reply_to = reply_to
          const result = await daemon("/ask", body)
          if (result.error) throw new Error(result.error)
          return result.correlation_id || ""
        },
      }),
      ack: tool({
        description: "Close an open ask thread. Bare close: ack(corr_id). Reply: ack(corr_id, message) -- delivered to the original asker.",
        args: {
          correlation_id: tool.schema.string().describe("The ask's correlation_id"),
          message: tool.schema.string().optional().describe("Optional reply content"),
        },
        async execute({ correlation_id, message }, ctx) {
          const me = callerPeer(ctx as { sessionID?: string })
          const body: any = {
            correlation_id,
            from_peer: me.peerName,
          }
          if (message !== undefined) body.message = message
          await daemon("/ack", body)
          return `acked #${correlation_id}` + (message ? " with reply" : "")
        },
      }),
      notify_peer: tool({
        description: "Send a notification to another peer (fire-and-forget)",
        args: {
          peer_name: tool.schema.string().describe("Name of the peer"),
          message: tool.schema.string().describe("The message to send"),
        },
        async execute({ peer_name, message }, ctx) {
          const me = callerPeer(ctx as { sessionID?: string })
          await daemon("/notify", {
            from_peer: me.peerName,
            to_peer: peer_name,
            text: message,
          })
          return "Notification sent"
        },
      }),
      broadcast: tool({
        description: "Broadcast a message to all peers in the mesh",
        args: {
          message: tool.schema.string().describe("Message to broadcast"),
        },
        async execute({ message }, ctx) {
          const me = callerPeer(ctx as { sessionID?: string })
          const result = await daemon("/broadcast", {
            from_peer: me.peerName,
            text: message,
          })
          const parts: string[] = []
          parts.push(`Broadcast sent to: ${result.sent_to?.join(", ") || "no peers"}`)
          if (result.failed?.length) {
            const fails = result.failed.map((f: { peer: string; error: string }) => `${f.peer} (${f.error})`).join(", ")
            parts.push(`Failed: ${fails}`)
          }
          return parts.join("; ")
        },
      }),
      whoami: tool({
        description: "Get information about this peer in the mesh",
        args: {},
        async execute(_args, ctx) {
          const me = callerPeer(ctx as { sessionID?: string })
          const identifier = me.peerId || me.peerName
          try {
            const result = await daemon(`/peers/${encodeURIComponent(identifier)}`)
            const project = result.metadata?.project || ""
            const header = "peer_id\tname\tproject\tcircle\tstatus\tpath\tmachine\tdescription"
            const row = [result.peer_id || "", result.display_name || result.name || "", project, result.circle || "", result.status || "", result.path || "", result.machine || "", result.description || ""].join("\t")
            return `${header}\n${row}`
          } catch {
            return `peer_id\tname\tproject\tcircle\tstatus\tpath\tmachine\tdescription\n${me.peerId || ""}\t${me.peerName}\t\t\tnot registered\t\t\t`
          }
        },
      }),
      set_description: tool({
        description: "Update your task description, visible to other peers via list_peers. Call this at the start of a task.",
        args: {
          description: tool.schema.string().describe("Short description of your current task"),
        },
        async execute({ description }, ctx) {
          const me = callerPeer(ctx as { sessionID?: string })
          await daemon(`/peers/${encodeURIComponent(me.peerName)}/description`, { description })
          return `description updated: ${description}`
        },
      }),
      set_circle: tool({
        description: "Join a named circle to communicate with peers in that circle. Applies to all sessions in this opencode server (circle is server-wide so reconnects keep it).",
        args: {
          circle: tool.schema.string().describe("Circle name to join (e.g., 'dev', 'frontend')"),
        },
        async execute({ circle: target }, _ctx) {
          // Update module state so reconnect carries the new circle.
          circle = target
          let sent = 0
          for (const conn of peerBySession.values()) {
            if (conn.ws?.readyState === WebSocket.OPEN) {
              conn.ws.send(JSON.stringify({ type: "set_circle", circle: target }))
              sent++
            }
          }
          return sent > 0
            ? `Joined circle: ${target} (${sent} session peer${sent === 1 ? "" : "s"})`
            : `Circle queued: ${target} (will apply on reconnect)`
        },
      }),
    },
    // Per-session event routing. Every session/message event carries
    // sessionID, so dispatch to the matching PeerConn.
    event: async ({ event }) => {
      const typedEvent = event as EventWithProperties
      const props = typedEvent.properties

      if (typedEvent.type === "session.created") {
        const info = props?.info as SessionEventInfo | undefined
        if (info?.id && info.parentID == null) {
          ensurePeer({ id: info.id, title: info.title })
        }
      } else if (typedEvent.type === "session.updated") {
        const info = props?.info as SessionEventInfo | undefined
        if (info?.id && info.parentID == null && !peerBySession.has(info.id)) {
          ensurePeer({ id: info.id, title: info.title })
        }
      } else if (typedEvent.type === "session.deleted") {
        const info = props?.info as SessionEventInfo | undefined
        if (info?.id) removePeer(info.id)
      } else if (typedEvent.type === "session.status") {
        // Authoritative busy/idle from opencode's run-state. Idle is the
        // canonical "done" signal for pending queries — using it avoids
        // re-implementing opencode's terminal-message logic (tool-call
        // loops, "stop with tool calls", reasoning vs text, etc.).
        // Source: packages/opencode/src/session/status.ts:38-83.
        const sid = props?.sessionID
        if (!sid) return
        const conn = peerBySession.get(sid)
        if (!conn) return
        const statusType = props?.status?.type
        if (statusType === "busy") {
          conn.busy = true
          sendStatus(conn, "busy")
        } else if (statusType === "idle") {
          conn.busy = false
          scheduleFlush(conn)
          sendStatus(conn, "idle")
        }
        // "retry" is left as-is (no status flip).
      } else if (typedEvent.type === "session.idle") {
        // Legacy event; session.status is the modern path, but a few opencode
        // versions still publish session.idle. Treat it the same as idle.
        const info = props?.info as SessionEventInfo | undefined
        const sid = info?.id || props?.sessionID
        if (!sid) return
        const conn = peerBySession.get(sid)
        if (!conn) return
        conn.busy = false
        scheduleFlush(conn)
        sendStatus(conn, "idle")
      } else if (typedEvent.type === "message.updated") {
        const info = props?.info as MessageEventInfo | undefined
        if (!info?.sessionID) return
        const conn = peerBySession.get(info.sessionID)
        if (!conn) return

        if (info.role === "user" && info.model) {
          activeModel = { providerID: info.model.providerID, modelID: info.model.modelID }
        }

        // Discover assistant messages (also captures error payload).
        // Finalization waits for session.status idle.
        trackAssistantMessage(conn, info)
      } else if (typedEvent.type === "message.part.updated") {
        const part = props?.part
        if (!part?.sessionID) return
        const conn = peerBySession.get(part.sessionID)
        if (!conn) return
        applyPartUpdated(conn, part)
      } else if (typedEvent.type === "message.part.delta") {
        const sid = props?.sessionID
        if (!sid) return
        const conn = peerBySession.get(sid)
        if (!conn) return
        applyPartDelta(conn, props as MessagePartDeltaProps)
      }
    },
    // Permission relay: forward tool-approval prompts to the mesh so
    // telegram/dashboard users can see what opencode is asking for. Attribute
    // from the calling session's peer when sessionID is in the input.
    "permission.ask": async (input, _output) => {
      try {
        const payload = input as Record<string, unknown>
        // opencode field names: `permission` is the permission slug (e.g. "bash"),
        // `sessionID` is canonical (packages/opencode/src/permission/index.ts:40-52).
        const toolName = (payload.permission || payload.name || "tool") as string
        const description = (payload.description || "") as string
        const requestId = (payload.id || payload.request_id || "") as string
        const sid = payload.sessionID as string | undefined
        const me = callerPeer(sid ? { sessionID: sid } : undefined)
        const controller = new AbortController()
        const timeout = setTimeout(() => controller.abort(), 1500)
        await fetch(`${DAEMON_URL}/notify`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          signal: controller.signal,
          body: JSON.stringify({
            from_peer: me.peerName,
            to_peer: "telegram",
            text:
              `Permission request: ${toolName}\\n` +
              (description ? `${description}\\n` : "") +
              (requestId ? `Reply "yes ${requestId}" or "no ${requestId}"` : ""),
          }),
        }).catch(() => undefined)
        clearTimeout(timeout)
      } catch (e) {
        console.debug("[repowire] permission relay failed:", e)
      }
    },
    // Per-session system prompt: tell the LLM which peer name it is so it
    // introduces itself correctly when other peers ask.
    "experimental.chat.system.transform": async (input, output) => {
      try {
        const sid = (input as { sessionID?: string })?.sessionID
        const conn = sid ? peerBySession.get(sid) : undefined

        const controller = new AbortController()
        const timeout = setTimeout(() => controller.abort(), 2000)
        const res = await fetch(`${DAEMON_URL}/peers`, { signal: controller.signal })
        clearTimeout(timeout)
        if (!res.ok) return
        const result = await res.json()
        const peers = (result.peers || []) as PeerInfo[]
        const myNames = new Set<string>()
        for (const c of peerBySession.values()) myNames.add(c.peerName)
        const otherPeers = peers.filter((p: PeerInfo) => !myNames.has(p.name) && p.status === "online")

        const peerList = otherPeers.length > 0
          ? otherPeers.map((p: PeerInfo) =>
              `  - ${p.name} on ${p.machine || "unknown"} (${p.path || "unknown path"})`
            ).join("\\n")
          : "  (no other peers online)"

        const identity = conn
          ? `You are peer "${conn.peerName}" in the Repowire mesh.`
          : `You are one of these mesh peers: ${[...myNames].join(", ") || "(none)"}.`

        output.system.push(`[Repowire Mesh] ${identity}

Other peers online:
${peerList}

IMPORTANT: When asked about other projects, ask the peer directly via ask tool rather than searching locally. ask returns a correlation_id immediately; the peer closes the thread via ack (bare = seen, no action) or ack(message) (reply). Inbound asks arrive framed [ask #cid] -- you must ack them.
Use list_peers to see current peer status. Use notify_peer for fire-and-forget messages.`)
      } catch (e) {
        console.debug("[repowire] Failed to fetch peer context:", e)
      }
    },
  }
}
"""

# Plugin file locations
GLOBAL_PLUGIN_DIR = Path.home() / ".opencode" / "plugin"
LOCAL_PLUGIN_DIR = Path(".opencode") / "plugin"
PLUGIN_FILENAME = "repowire.ts"


def _get_plugin_path(global_install: bool) -> Path:
    """Get the plugin path based on install type."""
    if global_install:
        return GLOBAL_PLUGIN_DIR / PLUGIN_FILENAME
    return LOCAL_PLUGIN_DIR / PLUGIN_FILENAME


def install_plugin(global_install: bool = True) -> bool:
    """Install the OpenCode plugin.

    Args:
        global_install: If True, install to ~/.config/opencode/plugin/
                       If False, install to .opencode/plugin/

    Returns:
        True if installation successful
    """
    plugin_path = _get_plugin_path(global_install)
    plugin_path.parent.mkdir(parents=True, exist_ok=True)
    plugin_path.write_text(PLUGIN_CONTENT)
    return True


def uninstall_plugin(global_install: bool = True) -> bool:
    """Uninstall the OpenCode plugin.

    Args:
        global_install: If True, uninstall from ~/.opencode/plugin/
                       If False, uninstall from .opencode/plugin/

    Returns:
        True if plugin was removed, False if it wasn't installed
    """
    plugin_path = _get_plugin_path(global_install)
    if plugin_path.exists():
        plugin_path.unlink()
        return True
    return False


def check_plugin_installed(global_install: bool = True) -> bool:
    """Check if the OpenCode plugin is installed.

    Args:
        global_install: If True, check ~/.config/opencode/plugin/
                       If False, check .opencode/plugin/

    Returns:
        True if plugin is installed
    """
    plugin_path = _get_plugin_path(global_install)
    return plugin_path.exists()
