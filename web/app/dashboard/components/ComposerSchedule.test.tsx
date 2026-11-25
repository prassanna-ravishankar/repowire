import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { PeerSchedules, scheduleBody, ScheduleControls, type ScheduleChoice } from "./ComposerSchedule";
import type { Peer } from "../types";

const PEER: Peer = { peer_id: "repow-dev-bbbb", name: "beta", display_name: "beta", status: "offline", machine: "m", path: "/tmp/beta", circle: "dev" };
const base = { to_peer: PEER.peer_id, text: "check CI", kind: "notify" as const };

afterEach(() => vi.unstubAllGlobals());

describe("scheduleBody", () => {
  const now = new Date("2026-11-25T12:00:00Z");
  it("sends a one-shot as fire_at in UTC", () => {
    const local = new Date("2026-11-25T13:30:00Z");
    const at = `${local.getFullYear()}-${String(local.getMonth() + 1).padStart(2, "0")}-${String(local.getDate()).padStart(2, "0")}T${String(local.getHours()).padStart(2, "0")}:${String(local.getMinutes()).padStart(2, "0")}`;
    const out = scheduleBody({ when: "at", at, cron: "" }, base, now);
    expect(out).toEqual({ body: { from_peer: "dashboard", ...base, fire_at: "2026-11-25T13:30:00.000Z" } });
  });
  it("refuses a past or missing time and a malformed cron", () => {
    expect(scheduleBody({ when: "at", at: "", cron: "" }, base, now)).toEqual({ error: "Pick a date and time" });
    expect(scheduleBody({ when: "at", at: "2020-01-01T00:00", cron: "" }, base, now)).toEqual({ error: "That time has passed" });
    expect("error" in scheduleBody({ when: "repeat", at: "", cron: "0 9 * *" }, base, now)).toBe(true);
  });
  it("sends a repeat as cron", () => {
    expect(scheduleBody({ when: "repeat", at: "", cron: " 0 9 * * 1-5 " }, { ...base, kind: "ask" }, now)).toEqual({
      body: { from_peer: "dashboard", ...base, kind: "ask", cron: "0 9 * * 1-5" },
    });
  });
});

describe("ScheduleControls", () => {
  it("warns that a scheduled ask is not queued, and says cron is UTC", () => {
    let choice: ScheduleChoice = { when: "now", at: "", cron: "0 * * * *" };
    const { rerender } = render(<ScheduleControls choice={choice} onChange={(c) => (choice = c)} mode="ask" peer={PEER} />);
    expect(screen.queryByTestId("scheduled-ask-note")).toBeNull();
    fireEvent.click(screen.getByTestId("when-repeat"));
    rerender(<ScheduleControls choice={choice} onChange={(c) => (choice = c)} mode="ask" peer={PEER} />);
    expect(screen.getByTestId("scheduled-ask-note").textContent).toContain("not queued");
    expect(screen.getByText("cron runs in UTC")).toBeTruthy();
    rerender(<ScheduleControls choice={choice} onChange={(c) => (choice = c)} mode="notify" peer={PEER} />);
    expect(screen.queryByTestId("scheduled-ask-note")).toBeNull();
  });
});

describe("PeerSchedules", () => {
  it("lists only this peer's schedules from the dashboard, and cancels one", async () => {
    const schedules = [
      { schedule_id: "s1", from_peer: "dashboard", to_peer: PEER.peer_id, text: "daily standup", kind: "ask", fire_at: "2026-11-26T09:00:00Z", cron: "0 9 * * *" },
      { schedule_id: "s2", from_peer: "dashboard", to_peer: "repow-other", text: "not mine", kind: "notify", fire_at: "2026-11-26T09:00:00Z", cron: null },
      { schedule_id: "s3", from_peer: "dashboard", to_peer: PEER.peer_id, text: "already fired", kind: "notify", fire_at: "2020-01-01T00:00:00Z", cron: null },
    ];
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (init?.method === "DELETE") {
        schedules.splice(0, 1);
        return Promise.resolve(new Response("{}", { status: 200 }));
      }
      expect(url).toContain("from_peer=dashboard");
      return Promise.resolve(new Response(JSON.stringify({ schedules }), { status: 200 }));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<PeerSchedules peer={PEER} apiBase="/api" refresh={0} />);
    expect(await screen.findByText("daily standup")).toBeTruthy();
    expect(screen.queryByText("not mine")).toBeNull();
    expect(screen.queryByText("already fired")).toBeNull(); // a past one-shot has fired
    fireEvent.click(screen.getByLabelText("Cancel scheduled ask"));
    await waitFor(() => expect(screen.queryByText("daily standup")).toBeNull());
    expect(fetchMock.mock.calls.some((c) => c[0] === "/api/schedules/s1" && (c[1] as RequestInit)?.method === "DELETE")).toBe(true);
  });
});
