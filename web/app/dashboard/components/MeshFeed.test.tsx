import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MeshFeed } from "./MeshFeed";
import type { Event, Peer } from "../types";

const PEER: Peer = {
  peer_id: "peer-1",
  name: "alice",
  display_name: "alice",
  status: "online",
  machine: "host",
  path: "/tmp/alice",
  circle: "default",
};

describe("MeshFeed", () => {
  beforeEach(() => {
    Element.prototype.scrollIntoView = vi.fn();
  });

  it("uses apiBase for attachment download links", () => {
    const event: Event = {
      id: "event-1",
      type: "notification",
      timestamp: "2025-01-01T00:00:00Z",
      from: "alice",
      to: "bob",
      text: "see file",
      attachments: [{
        id: "att-123",
        filename: "diagram.png",
      }],
    };

    render(
      <MeshFeed
        events={[event]}
        peers={[PEER]}
        apiBase="http://daemon.test"
        onPickPeer={vi.fn()}
      />,
    );

    expect(screen.getByRole("link", { name: /diagram\.png/i })).toHaveAttribute(
      "href",
      "http://daemon.test/attachments/att-123",
    );
  });

  it("renders missing event actors as inert text", () => {
    const event: Event = {
      id: "event-unknown",
      type: "notification",
      timestamp: "2025-01-01T00:00:00Z",
      text: "system event",
    };

    render(
      <MeshFeed
        events={[event]}
        peers={[PEER]}
        apiBase="http://daemon.test"
        onPickPeer={vi.fn()}
      />,
    );

    expect(screen.getByText("unknown")).toBeInTheDocument();
    expect(screen.getByText("—")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "unknown" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "—" })).not.toBeInTheDocument();
  });

  it("keeps real peer actors clickable", () => {
    const event: Event = {
      id: "event-peer",
      type: "notification",
      timestamp: "2025-01-01T00:00:00Z",
      from: "alice",
      to: "bob",
      text: "hello",
    };
    const onPickPeer = vi.fn();

    render(
      <MeshFeed
        events={[event]}
        peers={[PEER]}
        apiBase="http://daemon.test"
        onPickPeer={onPickPeer}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "alice" }));

    expect(onPickPeer).toHaveBeenCalledWith(PEER);
  });

  it("renders bare ack events with a useful summary", () => {
    const event: Event = {
      id: "event-ack",
      type: "ack",
      timestamp: "2025-01-01T00:00:00Z",
      from: "alice",
      to: "bob",
      correlation_id: "ask-123",
      status: "success",
      delivered: false,
      has_message: false,
      has_attachments: false,
    };

    render(
      <MeshFeed
        events={[event]}
        peers={[PEER]}
        apiBase="http://daemon.test"
        onPickPeer={vi.fn()}
      />,
    );

    expect(screen.getByText("ack #ask-123 closed")).toBeInTheDocument();
    expect(screen.getByText("ack")).toBeInTheDocument();
  });

  it("renders ack reply event text when present", () => {
    const event: Event = {
      id: "event-ack-reply",
      type: "ack",
      timestamp: "2025-01-01T00:00:00Z",
      from: "alice",
      to: "bob",
      correlation_id: "ask-456",
      text: "fixed in commit abc",
      status: "success",
      delivered: true,
      has_message: true,
      has_attachments: false,
    };

    render(
      <MeshFeed
        events={[event]}
        peers={[PEER]}
        apiBase="http://daemon.test"
        onPickPeer={vi.fn()}
      />,
    );

    expect(screen.getByText("fixed in commit abc")).toBeInTheDocument();
  });

  it("renders lifecycle events as a verb plus the peer name, never as an unknown route", () => {
    const events: Event[] = [
      { id: "e-online", type: "peer_online", timestamp: "2025-01-01T00:00:01Z", peer_id: "peer-1", peer_name: "alice" },
      { id: "e-status", type: "peer_status", timestamp: "2025-01-01T00:00:02Z", peer_id: "peer-1", peer_name: "alice", status: "busy" },
      { id: "e-offline", type: "peer_offline", timestamp: "2025-01-01T00:00:03Z", peer_id: "peer-1", peer_name: "alice", reason: "no_websocket_no_pane" },
      { id: "e-contra", type: "peer_contradiction", timestamp: "2025-01-01T00:00:04Z", peer_id: "peer-1", peer_name: "alice", severity: "warn", code: "stale_pane", detail: "pane gone" },
      { id: "e-demote", type: "peer_not_addressable", timestamp: "2025-01-01T00:00:05Z", peer_id: "peer-1", peer_name: "alice", reason: "subagent_direct_input_denied", asks_closed: 2, deliveries_dropped: 1 },
    ];
    const onPickPeer = vi.fn();

    render(
      <MeshFeed
        events={events}
        peers={[PEER]}
        apiBase="http://daemon.test"
        onPickPeer={onPickPeer}
      />,
    );

    expect(screen.getByText(/^online/)).toBeInTheDocument();
    expect(screen.getByText(/· busy/)).toBeInTheDocument();
    expect(screen.getByText(/· no_websocket_no_pane/)).toBeInTheDocument();
    expect(screen.getByText(/· warn · stale_pane · pane gone/)).toBeInTheDocument();
    expect(screen.getByText(/· subagent_direct_input_denied · 2 ask\(s\) closed · 1 queued dropped/)).toBeInTheDocument();
    expect(screen.queryByText("unknown")).not.toBeInTheDocument();
    expect(screen.queryByText("—")).not.toBeInTheDocument();

    fireEvent.click(screen.getAllByRole("button", { name: "alice" })[0]);
    expect(onPickPeer).toHaveBeenCalledWith(PEER);
  });

  it("renders peer reaped events without unknown actor buttons", () => {
    const event: Event = {
      id: "event-reaped",
      type: "peer_reaped",
      timestamp: "2025-01-01T00:00:00Z",
      peer_id: "peer-old",
      display_name: "old-codex",
      backend: "codex",
      path: "/repo/old",
      reason: "offline_ttl",
    };

    render(
      <MeshFeed
        events={[event]}
        peers={[PEER]}
        apiBase="http://daemon.test"
        onPickPeer={vi.fn()}
      />,
    );

    expect(screen.getByText(/reaped/)).toBeInTheDocument();
    expect(screen.getByText("old-codex")).toBeInTheDocument();
    expect(screen.getByText(/codex .* offline_ttl/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /unknown/i })).not.toBeInTheDocument();
  });
});
