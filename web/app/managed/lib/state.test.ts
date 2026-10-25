import { describe, expect, it } from "vitest";
import type { Agent, Client, Run } from "./api";
import { fixtures } from "./fixtures";
import { EMPTY_LIVE, agentSubline, applyFrame, connectedDaemon, formatAgo, formatDuration, formatUsd, runStatus, sortAgents, stateCounts, visibleAgents } from "./state";

const NOW = Date.parse("2026-10-25T14:05:00Z");
const f = fixtures(NOW);
const byId = (id: string) => f.agents.find((a) => a.id === id)!;

describe("agent ordering", () => {
  it("sorts failed, working, parked, idle, retired", () => {
    expect(sortAgents(f.agents).map((a) => a.state)).toEqual(["failed", "working", "parked", "idle", "idle", "retired"]);
  });

  it("puts the most recent change first within a state", () => {
    const idle = sortAgents(f.agents).filter((a) => a.state === "idle").map((a) => a.id);
    expect(idle).toEqual(["reviewer-hosted", "haiku-bot"]);
  });

  it("hides retired agents unless asked", () => {
    expect(visibleAgents(f.agents, false).some((a) => a.state === "retired")).toBe(false);
    expect(visibleAgents(f.agents, true)).toHaveLength(f.agents.length);
  });

  it("counts states", () => {
    expect(stateCounts(f.agents)).toEqual({ failed: 1, working: 1, parked: 1, idle: 2, retired: 1 });
  });
});

describe("agentSubline", () => {
  it("shows a running clock for working agents", () => {
    expect(agentSubline(byId("helper"), NOW)).toBe("run 0m 42s");
  });
  it("shows the reason and age for failures", () => {
    expect(agentSubline(byId("nightly-digest"), NOW)).toBe("run_failed: model error 429 · 12m ago");
  });
  it("shows last activity for idle agents (no sleeping state)", () => {
    expect(agentSubline(byId("reviewer-hosted"), NOW)).toBe("last active 2m ago");
  });
  it("shows what a parked agent waits on", () => {
    expect(agentSubline(byId("scratch"), NOW)).toBe("waiting on ask #a81f · since 41m ago");
  });
});

describe("runStatus", () => {
  it("reads aborted runs as stopped, not failed", () => {
    expect(runStatus({ ...f.runs[2], reason: "aborted" })).toMatchObject({ label: "ABORTED", failure: false });
    expect(runStatus(f.runs[2])).toMatchObject({ label: "UNANSWERED", failure: true });
    expect(runStatus(f.runs[1])).toMatchObject({ label: "ANSWERED", failure: false });
    expect(runStatus(f.runs[0])).toMatchObject({ label: "WORKING", failure: false });
  });
});

describe("applyFrame", () => {
  const snapshot = applyFrame(EMPTY_LIVE, { type: "snapshot", agents: f.agents, workspaces: f.workspaces, clients: f.clients.gateway, outbox: 2, at: "t" });

  it("replaces state from a snapshot", () => {
    const next = applyFrame(snapshot, { type: "snapshot", agents: [byId("helper")], workspaces: [], clients: [], outbox: 0, at: "t2" });
    expect(next.agents.map((a) => a.id)).toEqual(["helper"]);
    expect(next.outbox).toBe(0);
    expect(next.at).toBe("t2");
  });

  it("upserts agents by id", () => {
    const settled: Agent = { ...byId("helper"), state: "idle" };
    const next = applyFrame(snapshot, { type: "agent", agent: settled });
    expect(next.agents).toHaveLength(f.agents.length);
    expect(next.agents.find((a) => a.id === "helper")?.state).toBe("idle");
    const created = applyFrame(snapshot, { type: "agent", agent: { ...settled, id: "new-one" } });
    expect(created.agents[0].id).toBe("new-one");
  });

  it("keeps run frames only for agents whose runs are loaded", () => {
    const run: Run = { ...f.runs[0], status: "done", settled_at: "x" };
    expect(applyFrame(snapshot, { type: "run", agent: "helper", run })).toBe(snapshot);
    const loaded = applyFrame(snapshot, { type: "runs", agent: "helper", runs: f.runs });
    const next = applyFrame(loaded, { type: "run", agent: "helper", run });
    expect(next.runs.helper).toHaveLength(f.runs.length);
    expect(next.runs.helper[0].status).toBe("done");
    const fresh = applyFrame(loaded, { type: "run", agent: "helper", run: { ...run, op: "web-new" } });
    expect(fresh.runs.helper[0].op).toBe("web-new");
  });

  it("tracks outbox depth and daemon connection", () => {
    expect(applyFrame(snapshot, { type: "outbox", depth: 5 }).outbox).toBe(5);
    expect(connectedDaemon(snapshot.clients)?.machine).toBe("prass-mbp");
    const gone: Client = { ...f.clients.gateway[0], connected: false, disconnected_at: "z" };
    const next = applyFrame(snapshot, { type: "client", client: gone });
    expect(next.clients).toHaveLength(1);
    expect(connectedDaemon(next.clients)).toBeNull();
  });
});

describe("formatting", () => {
  it("formats durations", () => {
    expect(formatDuration(42_000)).toBe("0m 42s");
    expect(formatDuration(3_725_000)).toBe("1h 2m");
    expect(formatDuration(-5)).toBe("0m 00s");
  });
  it("formats ages", () => {
    expect(formatAgo(new Date(NOW - 30_000).toISOString(), NOW)).toBe("30s ago");
    expect(formatAgo(new Date(NOW - 3 * 86_400_000).toISOString(), NOW)).toBe("3d ago");
    expect(formatAgo(null, NOW)).toBe("never");
  });
  it("formats money", () => {
    expect(formatUsd(0.004)).toBe("<$0.01");
    expect(formatUsd(0)).toBe("$0.00");
    expect(formatUsd(11.844)).toBe("$11.84");
  });
});
