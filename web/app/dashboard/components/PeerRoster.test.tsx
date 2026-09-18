import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { PeerRoster } from "./PeerRoster";
import type { Peer } from "../types";

const BASE: Peer = {
  peer_id: "repow-1-parent",
  name: "app-codex",
  display_name: "app-codex",
  status: "online",
  machine: "host",
  path: "/work/app",
  circle: "1",
  backend: "codex",
  source: "codex-app-server",
  addressable: true,
};

describe("PeerRoster provenance", () => {
  it("badges non-addressable sub-agent threads with their nickname and keeps plain peers unbadged", () => {
    const child: Peer = {
      ...BASE,
      peer_id: "repow-1-child",
      name: "app-2-codex",
      display_name: "app-2-codex",
      source: "codex-app-server",
      parent_peer_id: "repow-1-parent",
      addressable: false,
      addressable_reason: "subagent_direct_input_denied",
      metadata: { agent_nickname: "Pasteur" },
    };
    const hook: Peer = { ...BASE, peer_id: "repow-1-cc", name: "app-claude-code", display_name: "app-claude-code", backend: "claude-code", source: "hook" };

    render(
      <PeerRoster peers={[BASE, child, hook]} allCount={3} selectedPeerId={null} filter="" onFilter={vi.fn()} onSelectPeer={vi.fn()} />,
    );

    expect(screen.getByText("no input · Pasteur")).toBeInTheDocument();
    expect(screen.getByText("app-server")).toBeInTheDocument();
    const childRow = screen.getByRole("button", { name: /app-2-codex/ });
    expect(childRow).toHaveAttribute("title", expect.stringContaining("subagent_direct_input_denied"));
    expect(screen.getByRole("button", { name: /app-claude-code/ })).not.toHaveAttribute("title");
  });

  it("nests sub-agents under a live parent and dims system threads and orphans", () => {
    const pascal: Peer = {
      ...BASE, peer_id: "repow-1-pascal", name: "app-3-codex", display_name: "app-3-codex",
      initiator: "agent", parent_runtime_id: "thread-parent", parent_peer_id: "repow-1-parent", metadata: { agent_nickname: "Pascal" },
    };
    const system: Peer = { ...BASE, peer_id: "repow-1-sys", name: "app-4-codex", display_name: "app-4-codex", initiator: "system", ephemeral: true };
    const orphan: Peer = {
      ...BASE, peer_id: "repow-1-orphan", name: "app-5-codex", display_name: "app-5-codex",
      initiator: "agent", parent_runtime_id: "thread-gone", parent_peer_id: null, metadata: { agent_nickname: "Nash" },
    };

    render(
      <PeerRoster peers={[system, orphan, pascal, { ...BASE, initiator: "user" }]} allCount={4} selectedPeerId={null} filter="" onFilter={vi.fn()} onSelectPeer={vi.fn()} />,
    );

    const names = screen.getAllByRole("button", { pressed: false }).map((button) => button.textContent ?? "");
    const order = names.map((text) => text.match(/app(-\d)?-codex/)?.[0]);
    expect(order.indexOf("app-3-codex")).toBe(order.indexOf("app-codex") + 1);
    expect(screen.getByRole("button", { name: /app-3-codex/ }).className).toContain("pl-7");
    expect(screen.getByRole("button", { name: /app-3-codex/ }).className).not.toContain("opacity-60");
    expect(screen.getByText("system")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /app-4-codex/ }).className).toContain("opacity-60");
    expect(screen.getByRole("button", { name: /app-5-codex/ })).toHaveAttribute("title", expect.stringContaining("parent is offline"));
  });
});
