import { afterEach, describe, expect, it } from "vitest";
import { HISTORY_CAP, __resetHistoryForTests, getHistory, pushHistory } from "./history";

afterEach(() => {
  __resetHistoryForTests();
});

describe("composer command history", () => {
  it("starts empty per peer", () => {
    expect(getHistory("p1")).toEqual([]);
  });

  it("records sends oldest-first and is keyed per peer", () => {
    pushHistory("p1", "first");
    pushHistory("p1", "second");
    pushHistory("p2", "other");
    expect(getHistory("p1")).toEqual(["first", "second"]);
    expect(getHistory("p2")).toEqual(["other"]);
  });

  it("ignores empty / whitespace-only sends", () => {
    pushHistory("p1", "");
    pushHistory("p1", "   ");
    pushHistory("p1", "\n\t");
    expect(getHistory("p1")).toEqual([]);
  });

  it("trims stored entries", () => {
    pushHistory("p1", "  spaced  ");
    expect(getHistory("p1")).toEqual(["spaced"]);
  });

  it("dedupes consecutive identical sends", () => {
    pushHistory("p1", "ls");
    pushHistory("p1", "ls");
    pushHistory("p1", "pwd");
    pushHistory("p1", "ls"); // non-consecutive duplicate is kept
    expect(getHistory("p1")).toEqual(["ls", "pwd", "ls"]);
  });

  it("bounds the ring to HISTORY_CAP, dropping oldest", () => {
    for (let i = 0; i < HISTORY_CAP + 10; i++) pushHistory("p1", `cmd-${i}`);
    const entries = getHistory("p1");
    expect(entries).toHaveLength(HISTORY_CAP);
    expect(entries[0]).toBe("cmd-10"); // first 10 fell off
    expect(entries[entries.length - 1]).toBe(`cmd-${HISTORY_CAP + 9}`);
  });

  it("ignores a falsy peer id", () => {
    pushHistory("", "x");
    expect(getHistory("")).toEqual([]);
  });
});
