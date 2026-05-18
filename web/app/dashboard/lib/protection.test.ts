import { afterEach, describe, expect, it } from "vitest";
import {
  __resetProtectionForTests,
  clearProtected,
  getProtectionSources,
  isProtected,
  markProtected,
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
});
