import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { TurnStateHint } from "./status";

describe("TurnStateHint", () => {
  it("surfaces pending_first_turn as a needs-prompt hint", () => {
    render(<TurnStateHint turnState="pending_first_turn" />);
    expect(screen.getByText("needs prompt")).toBeTruthy();
  });

  it("surfaces awaiting_input", () => {
    render(<TurnStateHint turnState="awaiting_input" />);
    expect(screen.getByText("awaiting input")).toBeTruthy();
  });

  it("surfaces working", () => {
    render(<TurnStateHint turnState="working" />);
    expect(screen.getByText("working")).toBeTruthy();
  });

  it("renders nothing for idle", () => {
    const { container } = render(<TurnStateHint turnState="idle" />);
    expect(container.textContent).toBe("");
  });

  it("renders nothing when turn_state is absent", () => {
    const { container } = render(<TurnStateHint turnState={undefined} />);
    expect(container.textContent).toBe("");
  });
});
