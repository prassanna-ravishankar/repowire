import { act, fireEvent, render, renderHook, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { CommandPalette, matches, useCommandShortcut, type Command } from "./CommandPalette";

const COMMANDS: Command[] = [
  { id: "broadcast", label: "Broadcast to the mesh…" },
  { id: "spawn", label: "Spawn a peer…" },
  { id: "peer:a", label: "Open @alpha", hint: "work · online" },
  { id: "peer:b", label: "Open @beta", hint: "ops · offline" },
];

describe("CommandPalette", () => {
  it("matches every word, in any order, case-insensitively", () => {
    expect(matches("Open @alpha work · online", "alpha OPEN")).toBe(true);
    expect(matches("Open @alpha work · online", "alpha ops")).toBe(false);
    expect(matches("anything", "")).toBe(true);
  });

  it("filters as you type and runs the highlighted command on Enter", () => {
    const onRun = vi.fn();
    const onClose = vi.fn();
    render(<CommandPalette commands={COMMANDS} onRun={onRun} onClose={onClose} />);
    const input = screen.getByLabelText("Search commands");
    expect(input).toHaveFocus();
    fireEvent.change(input, { target: { value: "ops" } });
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual([expect.stringContaining("@beta")]);
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onRun).toHaveBeenCalledWith("peer:b");
    expect(onClose).toHaveBeenCalled();
  });

  it("moves the highlight with the arrow keys, runs a clicked command, and closes on Escape", () => {
    const onRun = vi.fn();
    const onClose = vi.fn();
    render(<CommandPalette commands={COMMANDS} onRun={onRun} onClose={onClose} />);
    const input = screen.getByLabelText("Search commands");
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "ArrowUp" });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onRun).toHaveBeenLastCalledWith("spawn");
    fireEvent.click(screen.getByText("Open @alpha"));
    expect(onRun).toHaveBeenLastCalledWith("peer:a");
    onClose.mockClear();
    fireEvent.keyDown(input, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("says so when nothing matches, and Enter then runs nothing", () => {
    const onRun = vi.fn();
    render(<CommandPalette commands={COMMANDS} onRun={onRun} onClose={() => {}} />);
    const input = screen.getByLabelText("Search commands");
    fireEvent.change(input, { target: { value: "zzz" } });
    expect(screen.getByText("No command matches.")).toBeInTheDocument();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onRun).not.toHaveBeenCalled();
  });

  it("toggles on Cmd-K and Ctrl-K, and ignores other chords", () => {
    const { result } = renderHook(() => {
      const [open, setOpen] = useState(false);
      useCommandShortcut(setOpen);
      return open;
    });
    act(() => void window.dispatchEvent(new KeyboardEvent("keydown", { key: "k", metaKey: true })));
    expect(result.current).toBe(true);
    act(() => void window.dispatchEvent(new KeyboardEvent("keydown", { key: "k", ctrlKey: true })));
    expect(result.current).toBe(false);
    act(() => void window.dispatchEvent(new KeyboardEvent("keydown", { key: "k" })));
    act(() => void window.dispatchEvent(new KeyboardEvent("keydown", { key: "k", ctrlKey: true, shiftKey: true })));
    expect(result.current).toBe(false);
  });
});
