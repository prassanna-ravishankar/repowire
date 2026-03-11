"""OpenCode plugin installer."""

from __future__ import annotations

from pathlib import Path

PLUGIN_CONTENT = """import type { Plugin, PluginClient } from "@opencode-ai/plugin"
import { tool } from "@opencode-ai/plugin"

// Type definitions for event properties
interface SessionEventInfo {
  id?: string
}

interface MessageEventInfo {
  role?: string
  sessionID?: string
}

interface EventWithProperties {
  type: string
  properties?: {
    info?: SessionEventInfo | MessageEventInfo
  }
}

interface PeerInfo {
  name: string
  status: string
  machine?: string
  path?: string
}

// Configuration
const DAEMON_URL = process.env.REPOWIRE_DAEMON_URL || "http://127.0.0.1:8377"
const DAEMON_WS_URL = process.env.REPOWIRE_DAEMON_WS_URL || "ws://127.0.0.1:8377/ws"
const AUTH_TOKEN = process.env.REPOWIRE_AUTH_TOKEN || ""  // Optional auth token

// State
let ws: WebSocket | null = null
let peerName: string = "unknown"
let peerId: string | null = null  // Daemon-assigned unique ID
let projectPath: string = ""
let activeSessionId: string | null = null
let reconnectTimeout: ReturnType<typeof setTimeout> | null = null
let reconnectAttempts: number = 0
let opencodeClient: PluginClient | null = null
let stableNameSet: boolean = false

// Note: We no longer rely on TMUX_PANE for identity. The daemon assigns
// a unique peer_id (repow-{circle}-{uuid8}) on registration.

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
    // Store daemon-assigned session_id as peer_id
    if (data.session_id) {
      peerId = data.session_id as string
      console.debug(`[repowire] Connected with session_id: ${peerId}`)
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
    const text = data.text as string
    // Try to resolve session (like we do for queries)
    const sessionId = await resolveSessionId()
    if (!sessionId) {
      console.error(`[repowire] Cannot inject ${msgType}: no active session`)
      return
    }

    // Fire-and-forget - inject notification/broadcast
    if (opencodeClient) {
      try {
        await opencodeClient.session.prompt({
          path: { id: sessionId },
          body: { parts: [{ type: "text", text }] }
        })
      } catch (e) {
        console.error(`[repowire] Failed to inject ${msgType}:`, e)
      }
    }
  }
}

// Resolve active session - try tracked ID, then list, then create
async function resolveSessionId(): Promise<string | null> {
  if (activeSessionId) return activeSessionId
  if (!opencodeClient) return null

  // Try listing existing sessions
  try {
    const result = await opencodeClient.session.list()
    const sessions = result?.data
    if (Array.isArray(sessions) && sessions.length > 0) {
      activeSessionId = sessions[sessions.length - 1].id
      return activeSessionId
    }
  } catch (e) {
    console.warn("[repowire] Failed to list sessions:", e)
  }

  // Create a new session as last resort
  try {
    const result = await opencodeClient.session.create({ body: {} })
    if (result?.data?.id) {
      activeSessionId = result.data.id
      return activeSessionId
    }
  } catch (e) {
    console.warn("[repowire] Failed to create session:", e)
  }

  return null
}

// Handle incoming query - use sync prompt then poll for completed response
async function handleIncomingQuery(correlationId: string, fromPeer: string, text: string) {
  if (!opencodeClient) {
    sendError(correlationId, "OpenCode client not available")
    return
  }

  const sessionId = await resolveSessionId()
  if (!sessionId) {
    sendError(correlationId, "Could not resolve or create OpenCode session")
    return
  }

  sendStatus("busy")

  try {
    // session.prompt() fires the query. It returns immediately with the
    // message skeleton (0 parts), but the model IS processing.
    // Use session's default model (don't override)
    const result = await opencodeClient.session.prompt({
      path: { id: sessionId },
      body: { parts: [{ type: "text", text }] }
    })

    // Get the message ID from the response
    const messageId = result?.data?.info?.id
    if (!messageId) {
      sendError(correlationId, "No message ID returned from OpenCode session.prompt()")
      sendStatus("idle")
      return
    }

    // Poll for completion: check the message until parts are populated
    const maxWait = 120_000  // 120s
    const pollInterval = 1_000  // 1s (faster polling)
    const start = Date.now()

    while (Date.now() - start < maxWait) {
      await new Promise(r => setTimeout(r, pollInterval))

      const msgResult = await opencodeClient.session.message({
        path: { id: sessionId, messageID: messageId }
      })
      const msg = msgResult?.data

      // Try multiple paths for parts (SDK response structure varies)
      const parts = (msg as any)?.parts || (msg as any)?.info?.parts || []

      let responseText = ""
      for (const part of parts) {
        if (part.type === "text" && part.text) responseText += part.text
      }

      // Check completion status
      const info = (msg as any)?.info || msg
      const isCompleted = info?.time?.completed || info?.status === "completed"
      const hasError = info?.error

      // If we got text, return it immediately
      if (responseText) {
        sendResponse(correlationId, responseText)
        sendStatus("idle")
        return
      }

      // If completed or errored WITHOUT text, stop polling
      if (isCompleted || hasError) {
        const errorMsg = hasError ? `Model error: ${JSON.stringify(info.error)}` : "(empty response - model completed without output)"
        sendResponse(correlationId, errorMsg)
        sendStatus("idle")
        return
      }
    }

    // Timeout
    sendError(correlationId, "Query timed out waiting for OpenCode response")
  } catch (e) {
    const errorMsg = e instanceof Error ? e.message : String(e)
    console.error(`[repowire] Query failed: ${errorMsg}`)
    sendError(correlationId, errorMsg)
  } finally {
    sendStatus("idle")
  }
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
export const RepowirePlugin: Plugin = async ({ client, directory }) => {
  peerName = sanitizePeerName(directory.split("/").pop() || "unknown")
  projectPath = directory
  opencodeClient = client  // Store client for later use

  // Connect to daemon via WebSocket
  connectWebSocket()

  // Note: We track activeSessionId via the event hook instead of listing sessions
  // This avoids potential issues with client.session.list() at startup

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
    // Event hook to track session changes
    event: async ({ event }) => {
      const typedEvent = event as EventWithProperties
      if (typedEvent.type === "session.updated") {
        const info = typedEvent.properties?.info as SessionEventInfo | undefined
        if (info?.id) {
          activeSessionId = info.id
          if (!stableNameSet) {
            stableNameSet = true
            const stableName = sanitizePeerName(info.id.startsWith("ses") ? info.id.slice(3, 11) : info.id.slice(0, 8))
            if (stableName !== peerName) {
              peerName = stableName
              if (ws?.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: "update_display_name", display_name: stableName }))
              } else {
                ws?.close()  // fallback: reconnect will use the new name
              }
            }
          }
        }
      } else if (typedEvent.type === "message.updated") {
        const info = typedEvent.properties?.info as MessageEventInfo | undefined
        if (info?.role === "assistant" && info?.sessionID) {
          if (info.sessionID !== activeSessionId) {
            activeSessionId = info.sessionID
          }
          sendStatus("busy")
        }
      } else if (typedEvent.type === "session.idle") {
        sendStatus("idle")
      } else if (typedEvent.type === "session.deleted") {
        const info = typedEvent.properties?.info as SessionEventInfo | undefined
        if (info?.id === activeSessionId) {
          activeSessionId = null
          sendStatus("idle")
        }
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
