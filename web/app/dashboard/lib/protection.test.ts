import { afterEach, describe, expect, it } from "vitest";
import {
  __resetProtectionForTests,
  clearProtected,
  getFrozenThread,
  getProtectionSources,
  isProtected,
  markProtected,
  registerSnapshotProvider,
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

  it("captures a snapshot via the registered provider on first source", () => {
    registerSnapshotProvider<{ id: string }>("p1", () => [{ id: "e1" }, { id: "e2" }]);
    expect(getFrozenThread<{ id: string }>("p1")).toBeNull();

    markProtected("p1", "compose");
    expect(getFrozenThread<{ id: string }>("p1")).toEqual([{ id: "e1" }, { id: "e2" }]);
  });

  it("frozen thread snapshot survives until the last source clears", () => {
    registerSnapshotProvider<{ id: string }>("p1", () => [{ id: "e1" }]);
    markProtected("p1", "compose");
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

  it("does not re-capture on a second protection cycle without a new provider value", () => {
    let counter = 0;
    registerSnapshotProvider("p1", () => [{ id: `gen-${++counter}` }]);
    markProtected("p1", "compose");
    expect(getFrozenThread<{ id: string }>("p1")).toEqual([{ id: "gen-1" }]);
    // Mark a second time without clearing — must not re-invoke provider.
    markProtected("p1", "editor");
    expect(getFrozenThread<{ id: string }>("p1")).toEqual([{ id: "gen-1" }]);
    // Full clear releases.
    clearProtected("p1", "compose");
    clearProtected("p1", "editor");
    expect(getFrozenThread<{ id: string }>("p1")).toBeNull();
    // New cycle invokes provider again.
    markProtected("p1", "compose");
    expect(getFrozenThread<{ id: string }>("p1")).toEqual([{ id: "gen-2" }]);
  });
});
