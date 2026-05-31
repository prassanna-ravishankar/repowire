import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { OrphanPanesList, SettingsDialog } from "./DashboardDialogs";
import type { DaemonHealth, Peer } from "../types";

const SERVICE_PEER: Peer = {
  peer_id: "peer-telegram",
  name: "telegram-claude-code",
  display_name: "telegram-claude-code",
  status: "online",
  machine: "host",
  path: "/telegram",
  backend: "claude-code",
  circle: "default",
  role: "service",
};

const HEALTH: DaemonHealth = {
  status: "ok",
  version: "0.14.3",
  relay_mode: true,
  channel: {
    status: "ready",
    configured: true,
    runtime_available: true,
    last_error: null,
  },
  acp_broker: {
    status: "degraded",
    enabled: true,
    sdk_available: false,
    manager_initialized: false,
    configured_peers: 0,
    active_clients: 0,
    in_flight: 0,
    last_error: "agent-client-protocol SDK not installed",
    permissions: { pending: 2, last_error: null },
  },
};

describe("SettingsDialog", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("shows daemon health as read-only status", async () => {
    const fetchMock = vi.fn(async () => new Response(JSON.stringify(HEALTH), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    }));
    vi.stubGlobal("fetch", fetchMock);

    render(
      <SettingsDialog
        apiBase="http://daemon.test"
        isConnected
        peers={[SERVICE_PEER]}
        onClose={() => {}}
      />,
    );

    expect(await screen.findByText("0.14.3")).toBeInTheDocument();
    expect(screen.getByText("Enabled")).toBeInTheDocument();
    expect(screen.getByText("ready")).toBeInTheDocument();
    expect(screen.getByText("degraded")).toBeInTheDocument();
    expect(screen.getByText("agent-client-protocol SDK not installed")).toBeInTheDocument();
    expect(screen.getByText("2")).toBeInTheDocument();
    expect(screen.queryByRole("switch")).not.toBeInTheDocument();
    expect(screen.queryByPlaceholderText("rw_...")).not.toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledWith("http://daemon.test/health");
  });

  it("reports health fetch failures without changing connection status", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("nope", { status: 500 })));

    render(
      <SettingsDialog
        apiBase="http://daemon.test"
        isConnected
        peers={[]}
        onClose={() => {}}
      />,
    );

    expect(screen.getByText("Running")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("Unknown")).toBeInTheDocument());
    expect(screen.getByText("Error 500")).toBeInTheDocument();
  });
});

describe("OrphanPanesList", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders unlinked panes with a copyable link command", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            panes: [
              {
                pane_id: "%5",
                command: "claude",
                cwd: "/tmp/proj",
                detected_backend: "claude-code",
                confidence: "hint",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );

    render(<OrphanPanesList apiBase="http://daemon.test" />);

    expect(await screen.findByText("%5")).toBeInTheDocument();
    expect(
      screen.getByText("repowire link --pane %5 --backend claude-code"),
    ).toBeInTheDocument();
  });

  it("renders nothing when there are no orphan panes", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ panes: [] }), { status: 200 })),
    );

    const { container } = render(<OrphanPanesList apiBase="http://daemon.test" />);
    await waitFor(() => expect(container.querySelector('[data-testid="orphan-panes"]')).toBeNull());
  });

  it("uses a <backend> placeholder when detection is only a guess", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            panes: [
              {
                pane_id: "%6",
                command: "zsh",
                cwd: "/tmp/x",
                detected_backend: "unknown",
                confidence: "unknown",
              },
            ],
          }),
          { status: 200 },
        ),
      ),
    );

    render(<OrphanPanesList apiBase="http://daemon.test" />);
    expect(
      await screen.findByText("repowire link --pane %6 --backend <backend>"),
    ).toBeInTheDocument();
  });
});
