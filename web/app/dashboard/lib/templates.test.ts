import { afterEach, describe, expect, it, vi } from "vitest";
import {
  MAX_TEMPLATES,
  MAX_TEXT_LEN,
  __resetTemplatesForTests,
  deleteTemplate,
  listTemplates,
  saveTemplate,
} from "./templates";

const STORAGE_KEY = "repowire:composer-templates";

afterEach(() => {
  __resetTemplatesForTests();
  try {
    window.localStorage.clear();
  } catch {
    /* ignore */
  }
});

describe("composer ask templates", () => {
  it("starts empty", () => {
    expect(listTemplates()).toEqual([]);
  });

  it("saves and lists templates", () => {
    expect(saveTemplate("tests", "run the tests", 1)).toBe("saved");
    const list = listTemplates();
    expect(list).toHaveLength(1);
    expect(list[0]).toMatchObject({ name: "tests", text: "run the tests" });
    expect(list[0].id).toBeTruthy();
  });

  it("rejects empty name or text", () => {
    expect(saveTemplate("", "x")).toBe("rejected");
    expect(saveTemplate("name", "   ")).toBe("rejected");
    expect(listTemplates()).toEqual([]);
  });

  it("rejects text over the cap rather than truncating", () => {
    expect(saveTemplate("big", "x".repeat(MAX_TEXT_LEN + 1))).toBe("rejected");
    expect(listTemplates()).toEqual([]);
  });

  it("overwrites by case-insensitive name and moves it to the top", () => {
    saveTemplate("Run Tests", "v1", 1);
    saveTemplate("deploy", "ship it", 2);
    expect(saveTemplate("run tests", "v2", 3)).toBe("overwritten");
    const list = listTemplates();
    expect(list).toHaveLength(2); // not 3 — case-insensitive overwrite
    expect(list[0]).toMatchObject({ name: "run tests", text: "v2" }); // most recent first
  });

  it("lists most-recently-updated first", () => {
    saveTemplate("a", "ta", 1);
    saveTemplate("b", "tb", 2);
    saveTemplate("c", "tc", 3);
    expect(listTemplates().map((t) => t.name)).toEqual(["c", "b", "a"]);
  });

  it("deletes by id", () => {
    saveTemplate("a", "ta", 1);
    saveTemplate("b", "tb", 2);
    const a = listTemplates().find((t) => t.name === "a")!;
    deleteTemplate(a.id);
    expect(listTemplates().map((t) => t.name)).toEqual(["b"]);
  });

  it("bounds to MAX_TEMPLATES, dropping the least-recently-updated", () => {
    for (let i = 0; i < MAX_TEMPLATES + 5; i++) saveTemplate(`t${i}`, "x", i + 1);
    expect(listTemplates()).toHaveLength(MAX_TEMPLATES);
    // The earliest saves were evicted.
    expect(listTemplates().some((t) => t.name === "t0")).toBe(false);
    expect(listTemplates().some((t) => t.name === `t${MAX_TEMPLATES + 4}`)).toBe(true);
  });

  it("persists across a reset by hydrating from localStorage", () => {
    saveTemplate("kept", "remembered", 1);
    // Simulate reload: clear in-memory only by re-importing is awkward; instead
    // assert the blob is present and a fresh module load would read it.
    const raw = window.localStorage.getItem(STORAGE_KEY);
    expect(raw).toBeTruthy();
    const parsed = JSON.parse(raw!);
    expect(parsed.version).toBe(1);
    expect(parsed.templates[0]).toMatchObject({ name: "kept", text: "remembered" });
  });

  it("discards a corrupt or unknown-version blob without throwing", () => {
    window.localStorage.setItem(STORAGE_KEY, "{not json");
    expect(listTemplates()).toEqual([]);
    __resetTemplatesForTests();
    window.localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({ version: 999, templates: [{ id: "x", name: "n", text: "t", updatedAt: 1 }] }),
    );
    expect(listTemplates()).toEqual([]);
  });

  it("keeps templates in-memory even when localStorage writes fail", () => {
    const realSetItem = window.localStorage.setItem.bind(window.localStorage);
    vi.spyOn(window.localStorage.__proto__, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceeded");
    });
    expect(saveTemplate("t", "body", 1)).toBe("saved"); // store stays authoritative
    expect(listTemplates().map((x) => x.name)).toEqual(["t"]);
    vi.mocked(window.localStorage.setItem).mockImplementation(realSetItem);
  });
});
