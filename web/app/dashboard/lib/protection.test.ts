import { afterEach, describe, expect, it } from "vitest";
import {
  __resetProtectionForTests,
  clearProtected,
  getFrozenThread,
  getProtectionSources,
  isProtected,
  markProtected,
  setFrozenThread,
} from "./protection";

afterEach(() => {
  __resetProtectionForTests();
});

describe("protection registry", () => {
  it("starts unprotected", () => {
    expect(isProtected("p1")).toBe(false);
    expect(getProtectionSources("p1")).toEqual([]);
  });

  it("marks and clears a single source", () => {
    markProtected("p1", "compose");
    expect(isProtected("p1")).toBe(true);
    clearProtected("p1", "compose");
    expect(isProtected("p1")).toBe(false);
  });

  it("stays protected until every source clears", () => {
    markProtected("p1", "compose");
    markProtected("p1", "editor");
    expect(isProtected("p1")).toBe(true);
    clearProtected("p1", "compose");
    expect(isProtected("p1")).toBe(true);
    clearProtected("p1", "editor");
    expect(isProtected("p1")).toBe(false);
  });

  it("isolates peers", () => {
    markProtected("p1", "compose");
    expect(isProtected("p1")).toBe(true);
    expect(isProtected("p2")).toBe(false);
  });

  it("is idempotent", () => {
    markProtected("p1", "compose");
    markProtected("p1", "compose");
    clearProtected("p1", "compose");
    expect(isProtected("p1")).toBe(false);
  });

  it("frozen thread snapshot survives until the last source clears", () => {
    markProtected("p1", "compose");
    setFrozenThread("p1", [{ id: "e1" }]);
    expect(getFrozenThread<{ id: string }>("p1")).toEqual([{ id: "e1" }]);

    // Adding a second source doesn't disturb the snapshot.
    markProtected("p1", "editor");
    expect(getFrozenThread<{ id: string }>("p1")).toEqual([{ id: "e1" }]);

    // Clearing one source leaves the snapshot intact.
    clearProtected("p1", "compose");
    expect(getFrozenThread<{ id: string }>("p1")).toEqual([{ id: "e1" }]);

    // Clearing the last source drops the snapshot.
    clearProtected("p1", "editor");
    expect(getFrozenThread<{ id: string }>("p1")).toBeNull();
  });

  it("frozen snapshots are per-peer", () => {
    setFrozenThread("p1", [{ id: "a" }]);
    setFrozenThread("p2", [{ id: "b" }]);
    expect(getFrozenThread<{ id: string }>("p1")).toEqual([{ id: "a" }]);
    expect(getFrozenThread<{ id: string }>("p2")).toEqual([{ id: "b" }]);
  });
});
