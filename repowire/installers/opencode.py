"""OpenCode plugin installer."""

from __future__ import annotations

from pathlib import Path

PLUGIN_CONTENT = """import type { Plugin, PluginClient } from "@opencode-ai/plugin"
import { tool } from "@opencode-ai/plugin"
import * as fs from "node:fs"
import * as path from "node:path"
import * as os from "node:os"

// Type definitions for event properties
interface SessionEventInfo {
  id?: string
  parentID?: string | null
}

interface MessageEventInfo {
  id?: string
  role?: string
  sessionID?: string
  model?: {
    providerID: string
    modelID: string
    variant?: string
  }
  time?: { completed?: number }
  status?: string
  error?: unknown
  parts?: Array<{ type: string; text?: string }>
}

interface EventWithProperties {
  type: string
  properties?: {
    info?: SessionEventInfo | MessageEventInfo
    parts?: Array<{ type: string; text?: string }>
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
  buffer: string
  timeoutHandle: ReturnType<typeof setTimeout>
}

// Configuration
const DAEMON_URL = process.env.REPOWIRE_DAEMON_URL || "http://127.0.0.1:8377"
const DAEMON_WS_URL = process.env.REPOWIRE_DAEMON_WS_URL || "ws://127.0.0.1:8377/ws"
const AUTH_TOKEN = process.env.REPOWIRE_AUTH_TOKEN || ""  // Optional auth token
const QUERY_TIMEOUT_MS = 120_000

// State
let ws: WebSocket | null = null
let peerName: string = "unknown"
let peerId: string | null = null  // Daemon-assigned unique ID
let projectPath: string = ""
let primarySessionId: string | null = null
let serverUrl: string | null = null
let reconnectTimeout: ReturnType<typeof setTimeout> | null = null
let reconnectAttempts: number = 0
let opencodeClient: PluginClient | null = null
let activeModel: { providerID: string; modelID: string } | null = null
const pendingQueries = new Map<string, PendingQuery>()

// Note: We no longer rely on TMUX_PANE for identity. The daemon assigns
// a unique peer_id (repow-{circle}-{uuid8}) on registration.

// peer_id persistence: opencode runs as a fresh node process per session, so
// any in-memory peer_id is lost on restart. Without persistence, the daemon
// allocates a new id each time and the previous one lingers as a ghost peer.
// We cache `{ [projectPath]: peer_id }` to disk and replay it in the connect
// message; the daemon's allocate_and_register reuses the existing peer in-place
// if the id matches. (Issue #81.)
const PEER_ID_CACHE_PATH = path.join(os.homedir(), ".cache", "repowire", "opencode-peer-id.json")

function loadPeerId(projectPath: string): string | null {
  try {
    if (!fs.existsSync(PEER_ID_CACHE_PATH)) return null
    const raw = fs.readFileSync(PEER_ID_CACHE_PATH, "utf-8")
    const data = JSON.parse(raw) as Record<string, string>
    return data[projectPath] ?? null
  } catch (e) {
    console.debug("[repowire] Failed to load peer_id cache:", e)
    return null
  }
}

function savePeerId(projectPath: string, id: string): void {
  try {
    fs.mkdirSync(path.dirname(PEER_ID_CACHE_PATH), { recursive: true })
    let data: Record<string, string> = {}
    if (fs.existsSync(PEER_ID_CACHE_PATH)) {
      try {
        data = JSON.parse(fs.readFileSync(PEER_ID_CACHE_PATH, "utf-8")) as Record<string, string>
      } catch {
        // Corrupt cache: start fresh rather than fail to save.
        data = {}
      }
    }
    if (data[projectPath] === id) return
    data[projectPath] = id
    fs.writeFileSync(PEER_ID_CACHE_PATH, JSON.stringify(data, null, 2))
  } catch (e) {
    console.debug("[repowire] Failed to save peer_id cache:", e)
  }
}

// HTTP helpers for daemon
async function daemon(path: string, body?: object) {
  const res = await fetch(`${DAEMON_URL}${path}`, {
    method: body ? "POST" : "GET",
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) throw new Error(`Daemon error: ${res.status}`)
  return res.json()
}

// WebSocket connection to daemon
function connectWebSocket() {
  if (ws?.readyState === WebSocket.OPEN) return

  ws = new WebSocket(DAEMON_WS_URL)

  ws.onopen = () => {
    reconnectAttempts = 0  // Reset on successful connection
    // Send connect message - daemon assigns session_id and registers peer
    // Derive circle from tmux session name (like Claude Code hooks do)
    let circle = "default"
    let tmuxSession: string | undefined
    const tmuxPane = process.env.TMUX_PANE
    if (process.env.TMUX && tmuxPane) {
      try {
        const { execFileSync } = require("child_process")
        // Use -t to target our specific pane, not the most recently active session
        const session = execFileSync("tmux", ["display-message", "-t", tmuxPane, "-p", "#S"], { encoding: "utf-8" }).trim()
        const window = execFileSync("tmux", ["display-message", "-t", tmuxPane, "-p", "#W"], { encoding: "utf-8" }).trim()
        if (session) {
          circle = session
          if (window) {
            tmuxSession = `${session}:${window}`
          }
        }
      } catch (e) {
        console.warn("[repowire] Failed to derive circle from tmux:", e)
      }
    }

    const connectMsg: Record<string, unknown> = {
      type: "connect",
      display_name: peerName,
      circle,
      backend: "opencode",
      path: projectPath,
    }

    // Replay cached peer_id so the daemon reuses the existing peer in-place
    // instead of allocating a new one (which would leave the previous as a
    // ghost). On first run there's nothing to replay.
    const cachedPeerId = peerId || loadPeerId(projectPath)
    if (cachedPeerId) {
      connectMsg.peer_id = cachedPeerId
    }

    if (tmuxSession) {
      connectMsg.tmux_session = tmuxSession
    }

    if (tmuxPane) {
      connectMsg.pane_id = tmuxPane
    }

    if (AUTH_TOKEN) {
      connectMsg.auth_token = AUTH_TOKEN
    }
    ws?.send(JSON.stringify(connectMsg))
  }

  ws.onmessage = async (event) => {
    try {
      const data = JSON.parse(event.data.toString())
      await handleDaemonMessage(data)
    } catch (e) {
      console.error("[repowire] Failed to parse daemon message:", e)
    }
  }

  ws.onclose = () => {
    console.debug(`[repowire] WebSocket disconnected, scheduling reconnect`)
    scheduleReconnect()
  }

  ws.onerror = (err) => {
    console.error("[repowire] WebSocket error:", err)
  }
}

const MAX_RECONNECT_ATTEMPTS = 50

function scheduleReconnect() {
  if (reconnectTimeout) clearTimeout(reconnectTimeout)
  reconnectAttempts++
  if (reconnectAttempts > MAX_RECONNECT_ATTEMPTS) {
    console.error(`[repowire] Exhausted ${MAX_RECONNECT_ATTEMPTS} reconnect attempts, giving up`)
    return
  }
  // Exponential backoff: 3s, 6s, 12s, 24s, max 60s
  const delay = Math.min(3000 * Math.pow(2, reconnectAttempts - 1), 60000)
  reconnectTimeout = setTimeout(() => {
    connectWebSocket()
  }, delay)
}

function sendStatus(status: "busy" | "idle" | "offline") {
  if (ws?.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: "status", status }))
  } else {
    console.warn(`[repowire] Cannot send status '${status}': WebSocket not open`)
  }
}

// Session tracking is now handled internally, no need to send to daemon

function sendResponse(correlationId: string, text: string) {
  if (ws?.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: "response", correlation_id: correlationId, text }))
  } else {
    console.warn(`[repowire] Cannot send response for ${correlationId}: WebSocket not open`)
  }
}

function sendError(correlationId: string, error: string) {
  if (ws?.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: "error", correlation_id: correlationId, error }))
  } else {
    console.warn(`[repowire] Cannot send error for ${correlationId}: WebSocket not open`)
  }
}

// Handle messages from daemon
async function handleDaemonMessage(data: Record<string, unknown>) {
  const msgType = data.type as string

  if (msgType === "connected") {
    // Store daemon-assigned session_id as peer_id and persist for next run
    if (data.session_id) {
      peerId = data.session_id as string
      console.debug(`[repowire] Connected with session_id: ${peerId}`)
      if (projectPath) savePeerId(projectPath, peerId)
    }
    sendStatus("idle")
  } else if (msgType === "query") {
    const correlationId = data.correlation_id as string
    const fromPeer = data.from_peer as string
    const text = data.text as string
    await handleIncomingQuery(correlationId, fromPeer, text)
  } else if (msgType === "ping") {
    // Respond to daemon liveness check
    if (ws?.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: "pong", pane_alive: true }))
    }
  } else if (msgType === "notify" || msgType === "broadcast") {
    const fromPeer = (data.from_peer as string) || "unknown"
    const text = data.text as string
    const prefix = msgType === "broadcast" ? "[broadcast] " : ""
    await softInject(`@${fromPeer}: ${prefix}${text}`)
  } else if (msgType === "permission_response") {
    // Daemon-relayed approval reply (future hook, currently unused)
    return
  }
}

// Soft-inject text into the user's input box via tui.prompt.append.
// No model call fires; the user reviews and decides whether to send.
async function softInject(text: string) {
  if (!serverUrl) {
    console.warn("[repowire] No serverUrl available for soft inject")
    return
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
    }
  } catch (e) {
    console.warn("[repowire] Failed to publish tui event:", e)
  }
}

// Resolve the primary (root) session. Falls back to listing, then creating.
async function resolvePrimarySession(): Promise<string | null> {
  if (primarySessionId) return primarySessionId
  if (!opencodeClient) return null

  try {
    const result = await opencodeClient.session.list()
    const sessions = result?.data
    if (Array.isArray(sessions) && sessions.length > 0) {
      // Prefer root sessions (parentID null). Fall back to most recent.
      const root = sessions.find((s: any) => s.parentID == null)
      const chosen = root || sessions[sessions.length - 1]
      primarySessionId = chosen.id
      return primarySessionId
    }
  } catch (e) {
    console.warn("[repowire] Failed to list sessions:", e)
  }

  try {
    const result = await opencodeClient.session.create({ body: {} })
    if (result?.data?.id) {
      primarySessionId = result.data.id
      return primarySessionId
    }
  } catch (e) {
    console.warn("[repowire] Failed to create session:", e)
  }

  return null
}

// Handle incoming query: fire promptAsync, correlate response via message.updated.
// https://github.com/prassanna-ravishankar/repowire/issues/74
async function handleIncomingQuery(correlationId: string, fromPeer: string, text: string) {
  if (!opencodeClient) {
    sendError(correlationId, "OpenCode client not available")
    return
  }

  const sessionId = await resolvePrimarySession()
  if (!sessionId) {
    sendError(correlationId, "Could not resolve or create OpenCode session")
    return
  }

  sendStatus("busy")

  // Pre-generate a message ID so we can correlate the response from the event
  // stream without waiting for the prompt API call to return.
  const messageId = `msg_${correlationId}_${Date.now().toString(36)}`

  const timeoutHandle = setTimeout(() => {
    if (pendingQueries.has(messageId)) {
      pendingQueries.delete(messageId)
      sendError(correlationId, "Query timed out waiting for OpenCode response")
      sendStatus("idle")
    }
  }, QUERY_TIMEOUT_MS)

  pendingQueries.set(messageId, { correlationId, buffer: "", timeoutHandle })

  try {
    const body: Record<string, unknown> = {
      messageID: messageId,
      parts: [{ type: "text", text }],
    }
    if (activeModel) body.model = activeModel

    await opencodeClient.session.promptAsync({
      path: { id: sessionId },
      body,
    })
  } catch (e) {
    const pending = pendingQueries.get(messageId)
    if (pending) {
      clearTimeout(pending.timeoutHandle)
      pendingQueries.delete(messageId)
    }
    const errorMsg = e instanceof Error ? e.message : String(e)
    console.error(`[repowire] promptAsync failed: ${errorMsg}`)
    sendError(correlationId, errorMsg)
    sendStatus("idle")
  }
}

// Resolve a pending query when its message reaches a terminal state.
function resolvePendingQuery(messageId: string, info: MessageEventInfo) {
  const pending = pendingQueries.get(messageId)
  if (!pending) return

  // info.parts is the full message state on every event. Concatenate all
  // text parts to rebuild the response, then keep the latest non-empty
  // result as buffer (in case a later event arrives with parts cleared).
  const parts = info.parts || []
  let text = ""
  for (const part of parts) {
    if (part.type === "text" && part.text) text += part.text
  }
  if (text) pending.buffer = text

  const isCompleted = info.time?.completed || info.status === "completed"
  const hasError = info.error
  if (!isCompleted && !hasError) return

  clearTimeout(pending.timeoutHandle)
  pendingQueries.delete(messageId)
  if (hasError) {
    sendResponse(pending.correlationId, `Model error: ${JSON.stringify(hasError)}`)
  } else if (pending.buffer) {
    sendResponse(pending.correlationId, pending.buffer)
  } else {
    sendResponse(pending.correlationId, "(empty response: model completed without output)")
  }
  sendStatus("idle")
}

// Cleanup function
function cleanup() {
  if (reconnectTimeout) {
    clearTimeout(reconnectTimeout)
    reconnectTimeout = null
  }
  if (ws) {
    sendStatus("offline")
    ws.close()
    ws = null
  }
}

// Sanitize peer name to match daemon validation (alphanumeric, dots, underscore, hyphen)
function sanitizePeerName(name: string): string {
  return name.replace(/[^a-zA-Z0-9._-]/g, "_") || "unknown"
}

// Main plugin export
export const RepowirePlugin: Plugin = async ({ client, directory, ...rest }) => {
  peerName = sanitizePeerName(directory.split("/").pop() || "unknown")
  projectPath = directory
  opencodeClient = client  // Store client for later use

  // Capture serverUrl for tui/publish (POST /tui/publish soft-inject path).
  // PluginInput.serverUrl is a URL object whose href ends with a trailing slash.
  const su = (rest as { serverUrl?: URL | string }).serverUrl
  if (su) serverUrl = typeof su === "string" ? su : su.toString()

  // Connect to daemon via WebSocket
  connectWebSocket()

  // primarySessionId is set by event handler when session.updated fires for a
  // root session (parentID == null), or by resolvePrimarySession on demand.

  // Register cleanup on process exit
  process.on("beforeExit", cleanup)
  process.on("SIGINT", cleanup)
  process.on("SIGTERM", cleanup)

  return {
    tool: {
      list_peers: tool({
        description: "List all available peers in the mesh network",
        args: {},
        async execute() {
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
      ask_peer: tool({
        description: "Ask another peer a question and wait for their response",
        args: {
          peer_name: tool.schema.string().describe("Name of the peer to ask"),
          query: tool.schema.string().describe("The question to ask"),
        },
        async execute({ peer_name, query }) {
          const result = await daemon("/query", {
            from_peer: peerName,
            to_peer: peer_name,
            text: query
          })
          if (result.error) throw new Error(result.error)
          return result.text
        },
      }),
      notify_peer: tool({
        description: "Send a notification to another peer (fire-and-forget)",
        args: {
          peer_name: tool.schema.string().describe("Name of the peer"),
          message: tool.schema.string().describe("The message to send"),
        },
        async execute({ peer_name, message }) {
          await daemon("/notify", {
            from_peer: peerName,
            to_peer: peer_name,
            text: message
          })
          return "Notification sent"
        },
      }),
      broadcast: tool({
        description: "Broadcast a message to all peers in the mesh",
        args: {
          message: tool.schema.string().describe("Message to broadcast"),
        },
        async execute({ message }) {
          const result = await daemon("/broadcast", {
            from_peer: peerName,
            text: message
          })
          return `Broadcast sent to: ${result.sent_to?.join(", ") || "no peers"}`
        },
      }),
      whoami: tool({
        description: "Get information about this peer in the mesh",
        args: {},
        async execute() {
          const identifier = peerId || peerName
          try {
            const result = await daemon(`/peers/${encodeURIComponent(identifier)}`)
            const project = result.metadata?.project || ""
            const header = "peer_id\tname\tproject\tcircle\tstatus\tpath\tmachine\tdescription"
            const row = [result.peer_id || "", result.display_name || result.name || "", project, result.circle || "", result.status || "", result.path || "", result.machine || "", result.description || ""].join("\t")
            return `${header}\n${row}`
          } catch {
            return `peer_id\tname\tproject\tcircle\tstatus\tpath\tmachine\tdescription\n${peerId || ""}\t${peerName}\t\t\tnot registered\t\t\t`
          }
        },
      }),
      set_description: tool({
        description: "Update your task description, visible to other peers via list_peers. Call this at the start of a task.",
        args: {
          description: tool.schema.string().describe("Short description of your current task"),
        },
        async execute({ description }) {
          await daemon(`/peers/${encodeURIComponent(peerName)}/description`, { description })
          return `description updated: ${description}`
        },
      }),
      set_circle: tool({
        description: "Join a named circle to communicate with peers in that circle. Use this to communicate with peers in different circles (e.g., Claude Code sessions in tmux).",
        args: {
          circle: tool.schema.string().describe("Circle name to join (e.g., 'dev', 'frontend')"),
        },
        async execute({ circle }) {
          if (ws?.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: "set_circle", circle }))
            return `Joined circle: ${circle}`
          }
          return "Error: Not connected to daemon"
        },
      }),
    },
    // Event hook to track sessions and correlate query responses.
    // Subagents create sub-sessions with their own IDs (parentID set), and
    // their events fire on the same bus. Filter to root sessions for primary
    // tracking so subagent activity doesn't clobber state.
    event: async ({ event }) => {
      const typedEvent = event as EventWithProperties
      if (typedEvent.type === "session.updated") {
        const info = typedEvent.properties?.info as SessionEventInfo | undefined
        // Pin primarySessionId only for root sessions. parentID == null means
        // "this is a root session." Refresh on every root update so /new and
        // attach correctly retarget the pin.
        if (info?.id && info.parentID == null) {
          primarySessionId = info.id
        }
      } else if (typedEvent.type === "message.updated") {
        const info = typedEvent.properties?.info as MessageEventInfo | undefined
        if (!info) return

        // Only the primary session drives busy/idle and query correlation.
        if (info.sessionID && info.sessionID === primarySessionId) {
          if (info.role === "assistant") sendStatus("busy")
          if (info.id) resolvePendingQuery(info.id, info)
        }

        // Track active model for context-compaction parity (issue #74).
        if (info.role === "user" && info.model) {
          activeModel = { providerID: info.model.providerID, modelID: info.model.modelID }
        }
      } else if (typedEvent.type === "session.idle") {
        const info = typedEvent.properties?.info as SessionEventInfo | undefined
        // Only flip status when the primary session goes idle. Subagent idle
        // events are per-sub-session and shouldn't unblock the parent's view.
        if (!info?.id || info.id === primarySessionId) {
          sendStatus("idle")
        }
      } else if (typedEvent.type === "session.deleted") {
        const info = typedEvent.properties?.info as SessionEventInfo | undefined
        if (info?.id && info.id === primarySessionId) {
          primarySessionId = null
          sendStatus("idle")
        }
      }
    },
    // Permission relay: forward tool-approval prompts to the mesh so
    // telegram/dashboard users can see what opencode is asking for. Modeled
    // on the channel transport's permission relay (channel/server.ts).
    // Fire-and-forget: opencode's own approval UI still gates the call.
    "permission.ask": async (input, _output) => {
      try {
        const payload = input as Record<string, unknown>
        const toolName = (payload.tool || payload.name || "tool") as string
        const description = (payload.description || "") as string
        const requestId = (payload.id || payload.request_id || "") as string
        const controller = new AbortController()
        const timeout = setTimeout(() => controller.abort(), 1500)
        await fetch(`${DAEMON_URL}/notify`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          signal: controller.signal,
          body: JSON.stringify({
            from_peer: peerName,
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
    // Inject mesh network context into system prompt
    "experimental.chat.system.transform": async (_input, output) => {
      try {
        const controller = new AbortController()
        const timeout = setTimeout(() => controller.abort(), 2000)
        const res = await fetch(`${DAEMON_URL}/peers`, { signal: controller.signal })
        clearTimeout(timeout)
        if (!res.ok) return
        const result = await res.json()
        const peers = (result.peers || []) as PeerInfo[]
        const otherPeers = peers.filter((p: PeerInfo) => p.name !== peerName && p.status === "online")

        if (otherPeers.length > 0) {
          const peerList = otherPeers.map((p: PeerInfo) =>
            `  - ${p.name} on ${p.machine || "unknown"} (${p.path || "unknown path"})`
          ).join("\\n")

          output.system.push(`[Repowire Mesh] You have access to other coding sessions working on related projects:
${peerList}

IMPORTANT: When asked about these projects, ask the peer directly via ask_peer tool rather than searching locally.
Use list_peers to see current peer status. Use notify_peer for fire-and-forget messages.
Peer list may be outdated - use list_peers tool to refresh.`)
        }
      } catch (e) {
        // Daemon not running or timeout - skip context injection
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
