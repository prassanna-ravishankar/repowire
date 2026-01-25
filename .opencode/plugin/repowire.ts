import type { Plugin, PluginClient } from "@opencode-ai/plugin"
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
const DAEMON_WS_URL = process.env.REPOWIRE_DAEMON_WS_URL || "ws://127.0.0.1:8377/ws/plugin"

// State
let ws: WebSocket | null = null
let peerName: string = "unknown"
let projectPath: string = ""
let activeSessionId: string | null = null
let reconnectTimeout: ReturnType<typeof setTimeout> | null = null
let reconnectAttempts: number = 0
let opencodeClient: PluginClient | null = null

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
    ws?.send(JSON.stringify({
      type: "register",
      peer_name: peerName,
      path: projectPath,
      metadata: { branch: process.env.GIT_BRANCH || "unknown" }
    }))
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

function scheduleReconnect() {
  if (reconnectTimeout) clearTimeout(reconnectTimeout)
  reconnectAttempts++
  // Exponential backoff: 3s, 6s, 12s, 24s, max 60s
  const delay = Math.min(3000 * Math.pow(2, reconnectAttempts - 1), 60000)
  reconnectTimeout = setTimeout(() => {
    connectWebSocket()
  }, delay)
}

function sendStatus(status: "busy" | "idle" | "offline") {
  if (ws?.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: "status", status }))
  }
}

function sendSession(sessionId: string) {
  if (ws?.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: "session", session_id: sessionId }))
  }
}

function sendResponse(correlationId: string, text: string) {
  if (ws?.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: "response", correlation_id: correlationId, text }))
  }
}

function sendError(correlationId: string, error: string) {
  if (ws?.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: "error", correlation_id: correlationId, error }))
  }
}

// Handle messages from daemon
async function handleDaemonMessage(data: Record<string, unknown>) {
  const msgType = data.type as string

  if (msgType === "registered") {
    sendStatus("idle")
  } else if (msgType === "query") {
    const correlationId = data.correlation_id as string
    const fromPeer = data.from_peer as string
    const text = data.text as string
    await handleIncomingQuery(correlationId, fromPeer, text)
  } else if (msgType === "notify" || msgType === "broadcast") {
    const text = data.text as string
    // Fire-and-forget - inject if we have a session
    if (activeSessionId && opencodeClient) {
      try {
        await opencodeClient.session.prompt({
          path: { id: activeSessionId },
          body: { parts: [{ type: "text", text }] }
        })
      } catch (e) {
        console.error(`[repowire] Failed to inject ${msgType}:`, e)
      }
    }
  }
}

// Handle incoming query - inject into session and return response
async function handleIncomingQuery(correlationId: string, fromPeer: string, text: string) {
  if (!activeSessionId) {
    sendError(correlationId, "No active session - please start a conversation in OpenCode first")
    return
  }

  if (!opencodeClient) {
    sendError(correlationId, "OpenCode client not available")
    return
  }

  sendStatus("busy")

  try {
    // Use the OpenCode SDK client to inject the prompt
    const result = await opencodeClient.session.prompt({
      path: { id: activeSessionId },
      body: {
        parts: [{ type: "text", text }]
      }
    })

    // Extract text from response
    let responseText = ""
    const data = result?.data
    if (data?.parts) {
      for (const part of data.parts) {
        if (part.type === "text" && part.text) {
          responseText += part.text
        }
      }
    }

    sendResponse(correlationId, responseText || "(empty response)")
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

// Sanitize peer name to match daemon validation (alphanumeric, underscore, hyphen)
function sanitizePeerName(name: string): string {
  return name.replace(/[^a-zA-Z0-9_-]/g, "_") || "unknown"
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
          return JSON.stringify(result.peers, null, 2)
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
          return JSON.stringify({
            name: peerName,
            path: projectPath,
            activeSession: activeSessionId,
            daemonUrl: DAEMON_URL,
          }, null, 2)
        },
      }),
      set_circle: tool({
        description: "Join a named circle to communicate with peers in that circle. Use this to communicate with peers from different backends (e.g., Claude Code sessions in tmux).",
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
          sendSession(activeSessionId)
        }
      } else if (typedEvent.type === "message.updated") {
        const info = typedEvent.properties?.info as MessageEventInfo | undefined
        if (info?.role === "assistant" && info?.sessionID) {
          if (info.sessionID !== activeSessionId) {
            activeSessionId = info.sessionID
            sendSession(activeSessionId)
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
          ).join("\n")

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
