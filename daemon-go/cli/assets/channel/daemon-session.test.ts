import { describe, expect, test } from "bun:test";
import { afterRefusal, connectFrame } from "./daemon-session";

const placement = { circle: "1", circleSource: "tmux" as const, pane: "%3" };
const opts = { proposedName: "channel", projectPath: "/work/app", authToken: "" };

describe("connectFrame", () => {
  test("a first connect claims nothing", () => {
    expect(connectFrame(placement, null, opts)).not.toHaveProperty("peer_id");
  });

  // #443: a reconnect claims the identity the daemon assigned, so a sibling
  // session in the same folder cannot take it by reconnecting first.
  test("a reconnect claims the assigned peer_id", () => {
    const frame = connectFrame(placement, "repow-1-abcd1234", opts);
    expect(frame).toMatchObject({ type: "connect", backend: "claude-code", path: "/work/app", peer_id: "repow-1-abcd1234", pane_id: "%3" });
  });
});

describe("afterRefusal", () => {
  test("an operator close stops reconnecting", () => {
    expect(afterRefusal({ code: "peer_retired", final: true })).toBe("stop");
  });
  test("a soft retirement drops the claim", () => {
    expect(afterRefusal({ code: "peer_retired", final: false })).toBe("forget");
  });
  test("other errors change nothing", () => {
    expect(afterRefusal({ code: "something_else" })).toBeNull();
  });
});
