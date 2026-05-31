import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { BeadsBoard } from "./BeadsBoard";

function group(items: unknown[], total?: number, truncated = false) {
  return { items, total: total ?? items.length, truncated };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("BeadsBoard", () => {
  it("renders the four groups with counts and a compact assignee label", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            available: true,
            ready: group([
              { id: "repo-1", title: "do a thing", status: "open", priority: 2, issue_type: "task", assignee: "me@x.io" },
            ]),
            in_progress: group([]),
            blocked: group([]),
            recently_closed: group([
              { id: "repo-9", title: "done", status: "closed", priority: 1, issue_type: "bug", assignee: null },
            ]),
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );

    render(<BeadsBoard apiBase="http://daemon.test" />);

    expect(await screen.findByText("repo-1")).toBeInTheDocument();
    expect(screen.getByText("do a thing")).toBeInTheDocument();
    // Email assignee shown as localpart.
    expect(screen.getByText("me")).toBeInTheDocument();
    // Null assignee → Unassigned.
    expect(screen.getByText("Unassigned")).toBeInTheDocument();
    expect(screen.getByText("repo-9")).toBeInTheDocument();
  });

  it("shows a truncation hint when a group is capped", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            available: true,
            ready: group(
              [{ id: "r1", title: "t", status: "open", priority: 2, issue_type: "task", assignee: null }],
              51,
              true,
            ),
            in_progress: group([]),
            blocked: group([]),
            recently_closed: group([]),
          }),
          { status: 200 },
        ),
      ),
    );

    render(<BeadsBoard apiBase="http://daemon.test" />);
    expect(await screen.findByText("+50 more")).toBeInTheDocument();
  });

  it("renders a quiet unavailable state when Beads is not available", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            available: false,
            ready: group([]),
            in_progress: group([]),
            blocked: group([]),
            recently_closed: group([]),
          }),
          { status: 200 },
        ),
      ),
    );

    render(<BeadsBoard apiBase="http://daemon.test" />);
    expect(await screen.findByText("Beads board unavailable.")).toBeInTheDocument();
  });
});
