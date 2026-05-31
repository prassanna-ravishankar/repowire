import { afterEach, describe, expect, it, vi } from "vitest";
import {
  HISTORY_CAP,
  MAX_PEERS,
  __resetHistoryForTests,
  __simulateReloadForTests,
  clearHistory,
  getHistory,
  pushHistory,
} from "./history";

const STORAGE_KEY = "repowire:composer-history";

afterEach(() => {
  __resetHistoryForTests();
  try {
    window.localStorage.clear();
  } catch {
    /* ignore */
  }
  vi.unstubAllGlobals();
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
    expect(getHistory("p1")).toEqual([]);
  });

  it("dedupes consecutive identical sends", () => {
    pushHistory("p1", "ls");
    pushHistory("p1", "ls");
    pushHistory("p1", "pwd");
    pushHistory("p1", "ls");
    expect(getHistory("p1")).toEqual(["ls", "pwd", "ls"]);
  });

  it("bounds the ring to HISTORY_CAP, dropping oldest", () => {
    for (let i = 0; i < HISTORY_CAP + 10; i++) pushHistory("p1", `cmd-${i}`);
    const entries = getHistory("p1");
    expect(entries).toHaveLength(HISTORY_CAP);
    expect(entries[0]).toBe("cmd-10");
  });

  it("ignores a falsy peer id", () => {
    pushHistory("", "x");
    expect(getHistory("")).toEqual([]);
  });

  it("clears one peer's history", () => {
    pushHistory("p1", "a");
    pushHistory("p2", "b");
    clearHistory("p1");
    expect(getHistory("p1")).toEqual([]);
    expect(getHistory("p2")).toEqual(["b"]);
  });
});

describe("composer command history — persistence", () => {
  it("persists across a reset by hydrating from localStorage", () => {
    pushHistory("p1", "remembered");
    __simulateReloadForTests(); // memory cleared, storage kept
    expect(getHistory("p1")).toEqual(["remembered"]);
  });

  it("starts empty on corrupt stored JSON without throwing", () => {
    window.localStorage.setItem(STORAGE_KEY, "{not json");
    expect(getHistory("p1")).toEqual([]);
  });

  it("discards a blob with an unknown schema version", () => {
    window.localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({ version: 999, peers: { p1: { entries: ["x"], updatedAt: 1 } } }),
    );
    expect(getHistory("p1")).toEqual([]);
  });

  it("ignores bad per-peer records but keeps good ones", () => {
    window.localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({
        version: 1,
        peers: {
          good: { entries: ["ok"], updatedAt: 1 },
          bad: { entries: "not-an-array", updatedAt: 1 },
        },
      }),
    );
    expect(getHistory("good")).toEqual(["ok"]);
    expect(getHistory("bad")).toEqual([]);
  });

  it("evicts the least-recently-written peer beyond MAX_PEERS", () => {
    // Write MAX_PEERS+1 peers with increasing timestamps; the oldest is evicted.
    for (let i = 0; i <= MAX_PEERS; i++) {
      pushHistory(`peer-${i}`, `cmd`, i + 1);
    }
    __simulateReloadForTests(); // force re-hydrate from what was persisted
    expect(getHistory("peer-0")).toEqual([]); // oldest evicted
    expect(getHistory(`peer-${MAX_PEERS}`)).toEqual(["cmd"]); // newest kept
  });

  it("keeps history in-memory even when localStorage writes fail", () => {
    const realSetItem = window.localStorage.setItem.bind(window.localStorage);
    vi.spyOn(window.localStorage.__proto__, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceeded");
    });
    pushHistory("p1", "survives");
    expect(getHistory("p1")).toEqual(["survives"]); // in-memory authoritative
    // restore so afterEach clear works
    vi.mocked(window.localStorage.setItem).mockImplementation(realSetItem);
  });
});
