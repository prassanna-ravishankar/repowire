#!/usr/bin/env bun
/**
 * Repowire Channel — Native Claude Code transport.
 *
 * Replaces hooks + tmux injection with a direct MCP channel.
 * Connects to the repowire daemon via WebSocket and delivers
 * messages to Claude Code natively via channel notifications.
 *
 * Claude replies via the `reply` tool instead of transcript scraping.
 */

import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import {
  ListToolsRequestSchema,
  CallToolRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import WebSocket from "ws";

// -- Config --

const DAEMON_URL = process.env.REPOWIRE_DAEMON_URL ?? "ws://127.0.0.1:8377";
// Proposed name for initial connect; daemon assigns the canonical display_name
const PROPOSED_NAME = process.env.REPOWIRE_DISPLAY_NAME ?? "channel";
const CIRCLE = process.env.REPOWIRE_CIRCLE ?? "default";
const PROJECT_PATH = process.cwd();

// -- Daemon WebSocket --

let ws: WebSocket | null = null;
let sessionId: string | null = null;
let displayName: string = PROPOSED_NAME;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
const pendingCorrelations = new Map<string, string>(); // correlation_id -> from_peer

function connectDaemon(mcp: Server): void {
  const url = `${DAEMON_URL.replace("http://", "ws://").replace("https://", "wss://")}/ws`;

  ws = new WebSocket(url);

  ws.on("open", () => {
    ws!.send(
      JSON.stringify({
        type: "connect",
        display_name: PROPOSED_NAME,
        circle: CIRCLE,
        backend: "claude-code",
        path: PROJECT_PATH,
      })
    );
  });

  ws.on("message", async (data: WebSocket.Data) => {
    let msg: Record<string, any>;
    try {
      msg = JSON.parse(data.toString());
    } catch {
      console.error("repowire: invalid JSON from daemon");
      return;
    }

    if (msg.type === "connected") {
      sessionId = msg.session_id;
      if (msg.display_name) displayName = msg.display_name;
      console.error(`repowire: connected as ${displayName} (${sessionId})`);
      return;
    }

    if (msg.type === "ping") {
      ws?.send(JSON.stringify({ type: "pong" }));
      return;
    }

    // Deliver message to Claude via channel notification
    if (
      msg.type === "query" ||
      msg.type === "ask" ||
      msg.type === "notify" ||
      msg.type === "broadcast"
    ) {
      const meta: Record<string, string> = {
        from_peer: msg.from_peer ?? "unknown",
        msg_type: msg.type,
      };

      if ((msg.type === "query" || msg.type === "ask") && msg.correlation_id) {
        meta.correlation_id = msg.correlation_id;
        pendingCorrelations.set(msg.correlation_id, msg.from_peer);
      }
      if (msg.type === "ask" && msg.reply_to) {
        meta.reply_to = msg.reply_to;
      }

      await mcp.notification({
        method: "notifications/claude/channel",
        params: {
          content: msg.text ?? "",
          meta,
        },
      });

      // Transport-side pickup: tell the daemon this ask was delivered so it
      // can snapshot turn_seq for the grace-window check. Channel uses HTTP
      // base URL (DAEMON_URL is a WebSocket-flavored value).
      if (msg.type === "ask" && msg.correlation_id) {
        const httpUrl = DAEMON_URL.replace("ws://", "http://").replace(
          "wss://",
          "https://"
        );
        try {
          await fetch(
            `${httpUrl}/asks/${encodeURIComponent(msg.correlation_id)}/picked_up`,
            {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ correlation_id: msg.correlation_id }),
            }
          );
        } catch (e) {
          console.error(
            `repowire: failed to post ask pickup for ${msg.correlation_id}: ${e}`
          );
        }
      }
    }
  });

  ws.on("close", () => {
    console.error("repowire: daemon connection closed, reconnecting...");
    ws = null;
    scheduleReconnect(mcp);
  });

  ws.on("error", (err: Error) => {
    console.error(`repowire: ws error: ${err.message}`);
  });
}

function scheduleReconnect(mcp: Server): void {
  if (reconnectTimer) return;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    connectDaemon(mcp);
  }, 2000);
}

// -- Fetch peer list for context --

async function fetchPeerContext(): Promise<string> {
  try {
    const httpUrl = DAEMON_URL.replace("ws://", "http://").replace(
      "wss://",
      "https://"
    );
    const resp = await fetch(`${httpUrl}/peers`);
    const data = (await resp.json()) as { peers?: Array<Record<string, any>> };
    const peers = data.peers ?? [];
    const online = peers.filter(
      (p) =>
        p.status === "online" || p.status === "busy"
    );

    if (online.length === 0) return "";

    const lines = online
      .filter((p) => p.display_name !== displayName)
      .map((p) => {
        const name = p.display_name ?? p.name ?? "?";
        const folder = (p.path ?? "").split("/").pop() || name;
        const desc = p.description ? ` — ${p.description}` : "";
        return `  - ${name} (${folder})${desc}`;
      });

    if (lines.length === 0) return "";

    return [
      "\n[Repowire Mesh] Connected peers:",
      ...lines,
      "",
      "Use ask() to open a non-blocking thread (returns corr_id; peer responds via ack(corr_id) or ack(corr_id, message)). Use notify_peer() for fire-and-forget.",
      "Messages from @dashboard or @telegram are from the human user.",
      'Call set_description("task summary") so peers know what you\'re working on.',
    ].join("\n");
  } catch {
    return "";
  }
}

// -- MCP Server --

const peerContext = await fetchPeerContext();

const mcp = new Server(
  { name: "repowire", version: "0.6.0" },
  {
    capabilities: {
      experimental: {
        "claude/channel": {},
        "claude/channel/permission": {},
      },
      tools: {},
    },
    instructions: [
      "Repowire mesh messages arrive as <channel source=\"repowire\" from_peer=\"...\" msg_type=\"...\">.",
      "For queries (msg_type=\"query\"), reply using the reply tool with the correlation_id from the tag.",
      "For asks (msg_type=\"ask\"), the tag carries correlation_id. Use the ack tool: ack(correlation_id) for bare close, ack(correlation_id, message) to deliver a reply to the original asker.",
      "For notifications (msg_type=\"notify\"), act on them directly.",
      "Messages from @dashboard or @telegram are from the human user — treat as direct instructions.",
      peerContext,
    ]
      .filter(Boolean)
      .join("\n"),
  }
);

// -- Reply tool --

mcp.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: "reply",
      description:
        "Reply to a repowire query. Pass the correlation_id from the <channel> tag.",
      inputSchema: {
        type: "object" as const,
        properties: {
          correlation_id: {
            type: "string",
            description: "The correlation_id from the query's <channel> tag",
          },
          text: {
            type: "string",
            description: "Your response text",
          },
        },
        required: ["correlation_id", "text"],
      },
    },
  ],
}));

const ReplyArgs = z.object({
  correlation_id: z.string(),
  text: z.string(),
});

mcp.setRequestHandler(CallToolRequestSchema, async (req) => {
  if (req.params.name === "reply") {
    const { correlation_id, text } = ReplyArgs.parse(req.params.arguments);

    if (ws?.readyState === WebSocket.OPEN) {
      ws.send(
        JSON.stringify({
          type: "response",
          correlation_id,
          text,
        })
      );
      pendingCorrelations.delete(correlation_id);
      return { content: [{ type: "text" as const, text: "Reply sent." }] };
    }
    return {
      content: [
        { type: "text" as const, text: "Error: not connected to daemon." },
      ],
    };
  }
  throw new Error(`Unknown tool: ${req.params.name}`);
});

// -- Permission relay --

const PermissionRequestSchema = z.object({
  method: z.literal("notifications/claude/channel/permission_request"),
  params: z.object({
    request_id: z.string(),
    tool_name: z.string(),
    description: z.string(),
    input_preview: z.string(),
  }),
});

mcp.setNotificationHandler(PermissionRequestSchema, async ({ params }) => {
  // Forward permission prompt to daemon for relay to Telegram/dashboard
  if (ws?.readyState === WebSocket.OPEN) {
    ws.send(
      JSON.stringify({
        type: "notify",
        from_peer: displayName,
        text:
          `🔐 Permission request: ${params.tool_name}\n` +
          `${params.description}\n\n` +
          `Reply "yes ${params.request_id}" or "no ${params.request_id}"`,
      })
    );
  }
});

// -- Connect --

connectDaemon(mcp);
await mcp.connect(new StdioServerTransport());
