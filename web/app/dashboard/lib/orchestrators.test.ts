import { describe, expect, it } from "vitest";
import type { Peer } from "../types";
import { activeRuntimeCircles } from "./orchestrators";

const peer = (overrides: Partial<Peer>): Peer => ({
  peer_id: "peer-1",
  name: "peer",
  display_name: "peer",
  status: "online",
  machine: "local",
  path: "/work",
  circle: "0",
  role: "agent",
  ...overrides,
});

describe("activeRuntimeCircles", () => {
  it("does not require an orchestrator for a service-only circle", () => {
    expect(
      activeRuntimeCircles([
        peer({ circle: "default", role: "service", display_name: "telegram" }),
        peer({ circle: "0", role: "orchestrator" }),
      ]),
    ).toEqual(["0"]);
  });

  it("ignores circles containing only offline runtime peers", () => {
    expect(activeRuntimeCircles([peer({ circle: "dormant", status: "offline" })])).toEqual([]);
  });

  it("returns each active runtime circle once in display order", () => {
    expect(
      activeRuntimeCircles([
        peer({ circle: "beta" }),
        peer({ circle: "alpha", status: "busy" }),
        peer({ circle: "beta", role: "orchestrator" }),
      ]),
    ).toEqual(["alpha", "beta"]);
  });
});
