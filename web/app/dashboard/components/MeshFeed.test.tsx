import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
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
});
