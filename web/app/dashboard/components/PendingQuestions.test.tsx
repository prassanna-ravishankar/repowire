import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { PendingQuestions } from "./PendingQuestions";
import type { Event } from "../types";

function askEvent(cid: string, opts?: Partial<Event>): Event {
  return {
    id: cid,
    type: "ask",
    timestamp: "2026-05-31T00:00:00Z",
    correlation_id: cid,
    from: "agent",
    text: "run rm -rf?",
    question: {
      kind: "choice",
      options: [
        { id: "allow", title: "Allow" },
        { id: "deny", title: "Deny" },
      ],
    },
    ...opts,
  } as Event;
}

afterEach(() => vi.restoreAllMocks());

describe("PendingQuestions", () => {
  it("renders nothing when there are no open questions", () => {
    const { container } = render(<PendingQuestions events={[]} apiBase="http://x" />);
    expect(container.textContent).toBe("");
  });

  it("renders option buttons for an open choice question", () => {
    render(<PendingQuestions events={[askEvent("ask-1")]} apiBase="http://x" />);
    expect(screen.getByText("Allow")).toBeTruthy();
    expect(screen.getByText("Deny")).toBeTruthy();
    expect(screen.getByText(/1 question awaiting/)).toBeTruthy();
  });

  it("drops a question once an ack event for it arrives", () => {
    const ack: Event = {
      id: "e2", type: "ack", timestamp: "2026-05-31T00:01:00Z", correlation_id: "ask-1",
    } as Event;
    const { container } = render(
      <PendingQuestions events={[askEvent("ask-1"), ack]} apiBase="http://x" />,
    );
    expect(container.textContent).toBe("");
  });

  it("POSTs the chosen option to /answer", async () => {
    const fetchMock = vi.fn(() => Promise.resolve({ ok: true } as Response));
    vi.stubGlobal("fetch", fetchMock);
    render(<PendingQuestions events={[askEvent("ask-1")]} apiBase="http://x" />);
    fireEvent.click(screen.getByText("Deny"));
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("http://x/answer");
    expect(JSON.parse((init as RequestInit).body as string)).toEqual({
      correlation_id: "ask-1",
      option_id: "deny",
    });
  });
});
