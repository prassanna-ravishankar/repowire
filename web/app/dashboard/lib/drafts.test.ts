import { afterEach, describe, expect, it } from "vitest";
import {
  __resetDraftsForTests,
  clearDraft,
  getDraftFile,
  getDraftText,
  setDraftFile,
  setDraftText,
} from "./drafts";
import { __resetProtectionForTests, isProtected } from "./protection";

afterEach(() => {
  __resetDraftsForTests();
  __resetProtectionForTests();
});

describe("drafts store", () => {
  it("starts empty per peer", () => {
    expect(getDraftText("p1")).toBe("");
    expect(getDraftFile("p1")).toBeNull();
    expect(isProtected("p1")).toBe(false);
  });

  it("synchronously marks protection on dirty text", () => {
    setDraftText("p1", "hi");
    expect(getDraftText("p1")).toBe("hi");
    expect(isProtected("p1")).toBe(true);
  });

  it("synchronously clears protection on empty / whitespace text", () => {
    setDraftText("p1", "hello");
    expect(isProtected("p1")).toBe(true);
    setDraftText("p1", "   ");
    expect(isProtected("p1")).toBe(false);
    setDraftText("p1", "");
    expect(isProtected("p1")).toBe(false);
  });

  it("treats an attachment as dirty", () => {
    const f = new File(["x"], "x.txt", { type: "text/plain" });
    setDraftFile("p1", f);
    expect(isProtected("p1")).toBe(true);
    setDraftFile("p1", null);
    expect(isProtected("p1")).toBe(false);
  });

  it("keeps drafts isolated per peer", () => {
    setDraftText("p1", "alice draft");
    setDraftText("p2", "bob draft");
    expect(getDraftText("p1")).toBe("alice draft");
    expect(getDraftText("p2")).toBe("bob draft");
    expect(isProtected("p1")).toBe(true);
    expect(isProtected("p2")).toBe(true);
  });

  it("clearDraft wipes both fields and releases protection", () => {
    const f = new File(["x"], "x.txt");
    setDraftText("p1", "x");
    setDraftFile("p1", f);
    expect(isProtected("p1")).toBe(true);
    clearDraft("p1");
    expect(getDraftText("p1")).toBe("");
    expect(getDraftFile("p1")).toBeNull();
    expect(isProtected("p1")).toBe(false);
  });
});
