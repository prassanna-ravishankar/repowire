import type { Peer } from "../types";

export function activeRuntimeCircles(peers: Peer[]): string[] {
  return Array.from(
    new Set(
      peers
        .filter(
          (peer) =>
            peer.role !== "service" &&
            peer.role !== "human" &&
            (peer.status === "online" || peer.status === "busy"),
        )
        .map((peer) => peer.circle || "default"),
    ),
  ).sort((a, b) => a.localeCompare(b));
}
