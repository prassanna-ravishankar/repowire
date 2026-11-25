import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { BroadcastDialog, broadcastAudience } from "./BroadcastDialog";
import type { Peer } from "../types";

const peer = (over: Partial<Peer>): Peer => ({ peer_id: "p", name: "p", display_name: "p", status: "online", machine: "m", path: "/p", circle: "dev", role: "agent", ...over });
const PEERS: Peer[] = [
  peer({ peer_id: "a", display_name: "alpha", circle: "dev" }),
  peer({ peer_id: "b", display_name: "beta", circle: "ops", status: "busy" }),
  peer({ peer_id: "c", display_name: "gone", status: "offline" }),
  peer({ peer_id: "h", display_name: "prass", role: "human" }),
  peer({ peer_id: "t", display_name: "telegram", role: "service" }),
  peer({ peer_id: "s", display_name: "sub", addressable: false }),
];

afterEach(() => vi.unstubAllGlobals());

describe("BroadcastDialog", () => {
  it("reaches online, addressable agents in every circle, never the human or surfaces", () => {
    expect(broadcastAudience(PEERS).map((p) => p.peer_id)).toEqual(["a", "b"]);
  });

  it("names the audience, asks for its count, and posts one broadcast from the dashboard", async () => {
    const fetchMock = vi.fn((_url: string, _init?: RequestInit) =>
      Promise.resolve(new Response(JSON.stringify({ ok: true, sent_to: ["alpha", "beta"], failed: [], remote_sent: ["gamma@laptop"], remote_failed: ["delta@box"], partial: ["box2"] }), { status: 200 })),
    );
    vi.stubGlobal("fetch", fetchMock);
    render(<BroadcastDialog apiBase="/api" peers={PEERS} onClose={() => {}} />);
    expect(screen.getByText(/2 peers in 2 circles/)).toBeTruthy();
    const button = screen.getByRole("button", { name: /Broadcast to 2 peers here/ });
    expect((button as HTMLButtonElement).disabled).toBe(true); // nothing typed yet
    fireEvent.change(screen.getByLabelText("broadcast message"), { target: { value: "freeze merges" } });
    fireEvent.click(button);
    expect(await screen.findByTestId("broadcast-sent-to")).toBeTruthy();
    // Peers on other machines are named in the result, failures in red.
    expect(screen.getByTestId("broadcast-remote-on").textContent).toContain("gamma@laptop");
    expect(screen.getByTestId("broadcast-remote-failed").textContent).toContain("delta@box");
    expect(screen.getByTestId("broadcast-partial").textContent).toContain("box2");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/broadcast");
    expect(JSON.parse(String(init?.body))).toEqual({ from_peer: "dashboard", text: "freeze merges" });
  });

  it("cannot send when no agent is online", () => {
    render(<BroadcastDialog apiBase="/api" peers={[PEERS[2]]} onClose={() => {}} />);
    expect((screen.getByRole("button", { name: "No one to reach here" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
