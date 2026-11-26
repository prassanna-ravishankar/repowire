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

  function shortcut(enabled = true) {
    return renderHook(
      ({ on }) => {
        const [open, setOpen] = useState(false);
        useCommandShortcut(setOpen, on);
        return open;
      },
      { initialProps: { on: enabled } },
    );
  }
  const press = (init: KeyboardEventInit) => act(() => void window.dispatchEvent(new KeyboardEvent("keydown", { key: "k", ...init })));

  it("toggles on Ctrl-K off macOS, and ignores Cmd-K, other chords, repeats and IME composition", () => {
    vi.spyOn(navigator, "platform", "get").mockReturnValue("Linux x86_64");
    const { result } = shortcut();
    press({ ctrlKey: true });
    expect(result.current).toBe(true);
    press({ ctrlKey: true });
    expect(result.current).toBe(false);
    press({ metaKey: true });
    press({});
    press({ ctrlKey: true, shiftKey: true });
    press({ ctrlKey: true, repeat: true });
    press({ ctrlKey: true, isComposing: true });
    expect(result.current).toBe(false);
    vi.restoreAllMocks();
  });

  // On macOS Ctrl-K is the text fields' kill-to-end-of-line: only Cmd-K opens.
  it("toggles on Cmd-K on macOS and leaves Ctrl-K to the text fields", () => {
    vi.spyOn(navigator, "platform", "get").mockReturnValue("MacIntel");
    const { result } = shortcut();
    press({ ctrlKey: true });
    expect(result.current).toBe(false);
    press({ metaKey: true });
    expect(result.current).toBe(true);
    vi.restoreAllMocks();
  });

  it("does nothing while another dialog is open", () => {
    vi.spyOn(navigator, "platform", "get").mockReturnValue("MacIntel");
    const { result, rerender } = shortcut(false);
    press({ metaKey: true });
    expect(result.current).toBe(false);
    rerender({ on: true });
    press({ metaKey: true });
    expect(result.current).toBe(true);
    vi.restoreAllMocks();
  });

  it("returns focus to where it was when it closes", () => {
    const before = document.createElement("button");
    document.body.appendChild(before);
    before.focus();
    const { unmount } = render(<CommandPalette commands={COMMANDS} onRun={() => {}} onClose={() => {}} />);
    expect(screen.getByLabelText("Search commands")).toHaveFocus();
    unmount();
    expect(before).toHaveFocus();
    before.remove();
  });
});
