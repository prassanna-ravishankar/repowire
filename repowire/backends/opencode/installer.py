"""OpenCode plugin installer."""

from __future__ import annotations

from pathlib import Path

PLUGIN_CONTENT = """import type { Plugin } from "@opencode-ai/plugin"
import { tool } from "@opencode-ai/plugin"

const DAEMON_URL = process.env.REPOWIRE_DAEMON_URL || "http://127.0.0.1:8377"

async function daemon(path: string, body?: object) {
  const res = await fetch(`${DAEMON_URL}${path}`, {
    method: body ? "POST" : "GET",
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) throw new Error(`Daemon error: ${res.status}`)
  return res.json()
}

export const RepowirePlugin: Plugin = async ({ directory }) => {
  const peerName = directory.split("/").pop() || "unknown"

  await daemon("/peer/register", {
    name: peerName,
    path: directory,
    opencode_url: `http://127.0.0.1:${process.env.OPENCODE_PORT || 4096}`,
  })

  return {
    tool: {
      ask_peer: tool({
        description: "Ask another peer a question and wait for their response",
        args: {
          peer_name: tool.schema.string().describe("Name of the peer to ask"),
          query: tool.schema.string().describe("The question to ask"),
        },
        async execute({ peer_name, query }) {
          const result = await daemon("/query", { to_peer: peer_name, text: query })
          if (result.error) throw new Error(result.error)
          return result.text
        },
      }),
      notify_peer: tool({
        description: "Send a notification to another peer",
        args: {
          peer_name: tool.schema.string().describe("Name of the peer"),
          message: tool.schema.string().describe("The message to send"),
        },
        async execute({ peer_name, message }) {
          await daemon("/notify", { to_peer: peer_name, text: message })
          return "Notification sent"
        },
      }),
      list_peers: tool({
        description: "List all available peers in the mesh network",
        args: {},
        async execute() {
          const result = await daemon("/peers")
          return JSON.stringify(result.peers, null, 2)
        },
      }),
      broadcast: tool({
        description: "Broadcast a message to all peers",
        args: {
          message: tool.schema.string().describe("Message to broadcast"),
        },
        async execute({ message }) {
          const result = await daemon("/broadcast", { text: message })
          return `Broadcast sent to: ${result.sent_to?.join(", ") || "no peers"}`
        },
      }),
    },
    event: async ({ event }) => {
      if (event.type === "session.created") {
        await daemon("/session/update", {
          name: peerName,
          session_id: event.properties.session.id,
        })
      }
    },
  }
}
"""

# Plugin file locations
GLOBAL_PLUGIN_DIR = Path.home() / ".config" / "opencode" / "plugin"
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
        global_install: If True, uninstall from ~/.config/opencode/plugin/
                       If False, uninstall from .opencode/plugin/

    Returns:
        True if uninstallation successful
    """
    plugin_path = _get_plugin_path(global_install)
    if plugin_path.exists():
        plugin_path.unlink()
    return True


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
