import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { JobsView } from "./JobsView";
import type { JobsResponse } from "../types";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const QUEUED_JOB = {
  job_id: "work-1",
  title: "Audit docs",
  kind: "review",
  state: "queued",
  circle: "default",
  created_at: "2026-05-01T10:00:00Z",
  updated_at: "2026-05-01T10:05:00Z",
  due_at: "2026-05-01T11:00:00Z",
  execution: {
    prompt: { body: "Review the docs IA." },
    target: { path: "/repo/repowire", backend: "codex", profile: "fast" },
    delivery: { kind: "ask" },
  },
};

const RECURRING_JOB = {
  calendar_id: "cal-1",
  title: "Nightly maintenance",
  kind: "maintenance",
  state: "active",
  cron: "@daily",
  circle: "default",
  created_at: "2026-05-01T09:00:00Z",
  updated_at: "2026-05-01T09:00:00Z",
  next_due_at: "2026-05-02T09:00:00Z",
  execution: {
    prompt: { body: "Run the maintenance pass." },
    target: { path: "/repo/repowire", backend: "claude-code" },
    delivery: { kind: "notify" },
  },
};

const FAILED_JOB = {
  ...QUEUED_JOB,
  job_id: "work-failed",
  title: "Fix CI",
  state: "failed",
  result_summary: "pytest failed" as string | null,
  progress_events: [{ at: "2026-05-01T10:10:00Z", note: "runner failed" }],
};

const UNAVAILABLE_JOB = {
  ...QUEUED_JOB,
  job_id: "work-unavailable",
  title: "Recover peer",
  state: "unavailable",
  result_summary: "peer offline",
};

const COMPLETED_JOB = {
  ...QUEUED_JOB,
  job_id: "work-completed",
  title: "Ship release",
  state: "completed",
  result_summary: "done",
};

const CANCELLED_RECURRING_JOB = {
  ...RECURRING_JOB,
  calendar_id: "cal-cancelled",
  title: "Old schedule",
  state: "cancelled",
};

describe("JobsView", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders daemon jobs and recurring templates", async () => {
    const payload: JobsResponse = {
      work: [{ ...QUEUED_JOB, execution: { target: QUEUED_JOB.execution.target, delivery: QUEUED_JOB.execution.delivery } }],
      recurring: [{ ...RECURRING_JOB, execution: { target: RECURRING_JOB.execution.target, delivery: RECURRING_JOB.execution.delivery } }],
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/jobs?view=summary")) return jsonResponse(payload);
      if (url.endsWith("/jobs/work-1/status")) return jsonResponse({ status: QUEUED_JOB });
      if (url.endsWith("/jobs/cal-1/status")) return jsonResponse({ status: RECURRING_JOB });
      return jsonResponse({ detail: "not found" }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<JobsView apiBase="http://daemon.test" refreshSignal={0} />);

    expect((await screen.findAllByText("Audit docs")).length).toBeGreaterThan(0);
    expect(screen.getByText("Nightly maintenance")).toBeInTheDocument();
    expect(await screen.findByText("Review the docs IA.")).toBeInTheDocument();
    expect(screen.getAllByText("active").length).toBeGreaterThan(0);
    expect(fetchMock).toHaveBeenCalledWith("http://daemon.test/jobs?view=summary");
    expect(fetchMock).toHaveBeenCalledWith(
      "http://daemon.test/jobs/work-1/status",
      expect.any(Object),
    );

    fireEvent.click(screen.getByText("Nightly maintenance"));
    expect(await screen.findByText("Run the maintenance pass.")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledWith(
      "http://daemon.test/jobs/cal-1/status",
      expect.any(Object),
    );
  });

  it("cancels a recurring template through the jobs control route", async () => {
    let current = RECURRING_JOB;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/jobs?view=summary")) return jsonResponse({ work: [], recurring: [current] });
      if (url.endsWith("/jobs/cal-1/status")) return jsonResponse({ status: current });
      if (url.endsWith("/jobs/cal-1/cancel") && init?.method === "POST") {
        current = { ...RECURRING_JOB, state: "cancelled" };
        return jsonResponse({ status: current });
      }
      return jsonResponse({ detail: "not found" }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<JobsView apiBase="http://daemon.test" refreshSignal={0} />);

    fireEvent.click(await screen.findByRole("button", { name: /cancel/i }));

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        "http://daemon.test/jobs/cal-1/cancel",
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({ reason: "dashboard" }),
        }),
      );
    });
    await waitFor(() => expect(screen.getAllByText("cancelled").length).toBeGreaterThan(0));
  });

  it("offers retry and run controls for failed work jobs", async () => {
    let current = FAILED_JOB;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/jobs?view=summary")) return jsonResponse({ work: [current], recurring: [] });
      if (url.endsWith("/jobs/work-failed/status")) return jsonResponse({ status: current });
      if (url.endsWith("/jobs/work-failed/retry") && init?.method === "POST") {
        current = { ...FAILED_JOB, state: "queued", result_summary: null };
        return jsonResponse({ status: current });
      }
      return jsonResponse({ detail: "not found" }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<JobsView apiBase="http://daemon.test" refreshSignal={0} />);

    expect(await screen.findByRole("button", { name: /retry/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /run now/i })).toBeInTheDocument();
    expect(screen.getByText("runner failed")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /retry/i }));

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        "http://daemon.test/jobs/work-failed/retry",
        expect.objectContaining({ method: "POST" }),
      );
    });
    await waitFor(() => expect(screen.getAllByText("queued").length).toBeGreaterThan(0));
  });

  it("separates retryable terminal work from closed jobs", async () => {
    const payload: JobsResponse = {
      work: [FAILED_JOB, COMPLETED_JOB],
      recurring: [],
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/jobs?view=summary")) return jsonResponse(payload);
      if (url.endsWith("/jobs/work-failed/status")) return jsonResponse({ status: FAILED_JOB });
      if (url.endsWith("/jobs/work-completed/status")) return jsonResponse({ status: COMPLETED_JOB });
      return jsonResponse({ detail: "not found" }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<JobsView apiBase="http://daemon.test" refreshSignal={0} />);

    expect(await screen.findByText("needs attention")).toBeInTheDocument();
    expect(screen.getAllByText("closed").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Fix CI").length).toBeGreaterThan(0);
    expect(screen.getByText("Ship release")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
  });

  it("shows an explicit detail error while keeping summary rows usable", async () => {
    const payload: JobsResponse = {
      work: [QUEUED_JOB],
      recurring: [RECURRING_JOB],
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/jobs?view=summary")) return jsonResponse(payload);
      if (url.endsWith("/jobs/work-1/status")) return jsonResponse({ detail: "stale daemon" }, 500);
      if (url.endsWith("/jobs/cal-1/status")) return jsonResponse({ status: RECURRING_JOB });
      return jsonResponse({ detail: "not found" }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<JobsView apiBase="http://daemon.test" refreshSignal={0} />);

    await waitFor(() => expect(screen.getAllByText("Audit docs").length).toBeGreaterThan(0));
    expect(await screen.findByText("detail unavailable")).toBeInTheDocument();
    expect(screen.getByText(/The list is still loaded from the jobs summary view/)).toBeInTheDocument();

    fireEvent.click(screen.getByText("Nightly maintenance"));

    expect(await screen.findByText("Run the maintenance pass.")).toBeInTheDocument();
  });

  it("filters jobs by attention and recurring rows", async () => {
    const payload: JobsResponse = {
      work: [QUEUED_JOB, FAILED_JOB, UNAVAILABLE_JOB, COMPLETED_JOB],
      recurring: [RECURRING_JOB, CANCELLED_RECURRING_JOB],
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/jobs?view=summary")) return jsonResponse(payload);
      if (url.endsWith("/jobs/work-1/status")) return jsonResponse({ status: QUEUED_JOB });
      if (url.endsWith("/jobs/work-failed/status")) return jsonResponse({ status: FAILED_JOB });
      if (url.endsWith("/jobs/work-unavailable/status")) return jsonResponse({ status: UNAVAILABLE_JOB });
      if (url.endsWith("/jobs/work-completed/status")) return jsonResponse({ status: COMPLETED_JOB });
      if (url.endsWith("/jobs/cal-1/status")) return jsonResponse({ status: RECURRING_JOB });
      if (url.endsWith("/jobs/cal-cancelled/status")) return jsonResponse({ status: CANCELLED_RECURRING_JOB });
      return jsonResponse({ detail: "not found" }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<JobsView apiBase="http://daemon.test" refreshSignal={0} />);

    await waitFor(() => expect(screen.getAllByText("Audit docs").length).toBeGreaterThan(0));
    fireEvent.click(screen.getByRole("button", { name: "Attention" }));

    expect(screen.getAllByText("Fix CI").length).toBeGreaterThan(0);
    expect(screen.getByText("Recover peer")).toBeInTheDocument();
    expect(screen.queryByText("Audit docs")).not.toBeInTheDocument();
    expect(screen.queryByText("Ship release")).not.toBeInTheDocument();
    expect(await screen.findByText("runner failed")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledWith(
      "http://daemon.test/jobs/work-failed/status",
      expect.any(Object),
    );

    fireEvent.click(screen.getByRole("button", { name: "Recurring" }));

    expect(screen.getAllByText("Nightly maintenance").length).toBeGreaterThan(0);
    expect(screen.getByText("Old schedule")).toBeInTheDocument();
    expect(screen.queryByText("Fix CI")).not.toBeInTheDocument();
    expect(await screen.findByText("Run the maintenance pass.")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledWith(
      "http://daemon.test/jobs/cal-1/status",
      expect.any(Object),
    );
  });

  it("clears detail when a filter has no matching jobs", async () => {
    const payload: JobsResponse = {
      work: [QUEUED_JOB],
      recurring: [],
    };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/jobs?view=summary")) return jsonResponse(payload);
      if (url.endsWith("/jobs/work-1/status")) return jsonResponse({ status: QUEUED_JOB });
      return jsonResponse({ detail: "not found" }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<JobsView apiBase="http://daemon.test" refreshSignal={0} />);

    expect(await screen.findByText("Review the docs IA.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Attention" }));

    expect(screen.getByText("no matching jobs")).toBeInTheDocument();
    expect(screen.getByText("select a job")).toBeInTheDocument();
    expect(screen.queryByText("Review the docs IA.")).not.toBeInTheDocument();
  });
});
