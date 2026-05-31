"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { cn } from "./lib/utils";
import { useEventStream } from "./lib/useEventStream";
import { SettingsDialog, SpawnDialog } from "./components/DashboardDialogs";
import { BeadsBoard } from "./components/BeadsBoard";
import { JobsView } from "./components/JobsView";
import { MeshFeed } from "./components/MeshFeed";
import { MobileTabs, type MobileTab } from "./components/MobileTabs";
import { PeerRoster } from "./components/PeerRoster";
import { PeerView } from "./components/PeerView";
import { PendingQuestions } from "./components/PendingQuestions";
import { TopBar } from "./components/TopBar";
import { WireTrace } from "./components/WireTrace";
import type { Event, OrchestratorStatus, Peer } from "./types";

const API_BASE = process.env.NEXT_PUBLIC_API_BASE ?? "http://localhost:8377";

const RELAY_DASHBOARD_URL = "https://relay.repowire.io/dashboard";

export default function Dashboard() {
  const [redirecting] = useState(
    () => typeof window !== "undefined" && window.location.hostname === "repowire.io",
  );

  useEffect(() => {
    if (redirecting) {
      window.location.replace(RELAY_DASHBOARD_URL);
    }
  }, [redirecting]);

  if (redirecting) return null;
  return <DashboardInner />;
}

function DashboardInner() {
  const [peers, setPeers] = useState<Peer[]>([]);
  const [events, setEvents] = useState<Event[]>([]);
  const [orchestrators, setOrchestrators] = useState<OrchestratorStatus[]>([]);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [selectedPeerId, setSelectedPeerId] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [mobileTab, setMobileTab] = useState<MobileTab>("peers");
  const [mainView, setMainView] = useState<"mesh" | "jobs" | "beads">("mesh");
  const [refreshSignal, setRefreshSignal] = useState(0);
  const [showSpawn, setShowSpawn] = useState(false);
  const [showSettings, setShowSettings] = useState(false);
  const eventIdsRef = useRef<Set<string>>(new Set());
  const lastSeenIdRef = useRef<string | null>(null);
  const peersRef = useRef<Peer[]>([]);

  const fetchOrchestrators = useCallback(async (nextPeers?: Peer[]) => {
    const sourcePeers = nextPeers ?? peersRef.current;
    const circles = Array.from(
      new Set(sourcePeers.map((peer) => peer.circle || "default"))
    ).sort((a, b) => a.localeCompare(b));

    if (circles.length === 0) {
      setOrchestrators([]);
      return;
    }

    const statuses = await Promise.all(
      circles.map(async (circle): Promise<OrchestratorStatus> => {
        try {
          const res = await fetch(`${API_BASE}/circles/${encodeURIComponent(circle)}/orchestrator`);
          if (res.ok) {
            const data = await res.json();
            return { circle, ...data };
          }
        } catch (error) {
          console.debug("Falling back to peer-list orchestrator status:", error);
        }

        const peer = sourcePeers.find(
          (candidate) =>
            (candidate.circle || "default") === circle &&
            candidate.role === "orchestrator" &&
            (candidate.status === "online" || candidate.status === "busy")
        );
        return {
          circle,
          present: Boolean(peer),
          peer_id: peer?.peer_id ?? null,
          peer_name: peer?.name ?? null,
          display_name: peer?.display_name ?? null,
          last_seen: peer?.last_seen ?? null,
          stale_after: null,
        };
      })
    );
    setOrchestrators(statuses);
  }, []);

  const fetchPeers = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/peers`);
      if (res.ok) {
        const data = await res.json();
        const nextPeers: Peer[] = data.peers || data;
        peersRef.current = nextPeers;
        setPeers(nextPeers);
        await fetchOrchestrators(nextPeers);
      }
    } catch (error) {
      console.error("Failed to fetch peers:", error);
    }
  }, [fetchOrchestrators]);

  const fetchEvents = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/events`);
      if (res.ok) {
        const data: Event[] = await res.json();
        eventIdsRef.current = new Set(data.map((event) => event.id));
        if (data.length > 0) lastSeenIdRef.current = data[data.length - 1].id;
        setEvents(data);
      }
    } catch (error) {
      console.error("Failed to fetch events:", error);
    }
  }, []);

  const ingestEvent = useCallback(
    (event: Event) => {
      if (eventIdsRef.current.has(event.id)) return;
      eventIdsRef.current.add(event.id);
      lastSeenIdRef.current = event.id;
      setEvents((prev) => {
        const next = [...prev, event];
        return next.length > 500 ? next.slice(-500) : next;
      });
      if (event.type === "status_change") void fetchPeers();
    },
    [fetchPeers],
  );

  const connection = useEventStream({
    apiBase: API_BASE,
    onEvent: ingestEvent,
    getLastSeenId: useCallback(() => lastSeenIdRef.current, []),
  });

  const refreshData = useCallback(async () => {
    setIsRefreshing(true);
    await Promise.all([fetchPeers(), fetchEvents()]);
    setRefreshSignal((value) => value + 1);
    setIsRefreshing(false);
  }, [fetchEvents, fetchPeers]);

  useEffect(() => {
    queueMicrotask(() => {
      void Promise.all([fetchPeers(), fetchEvents()]);
    });
  }, [fetchEvents, fetchPeers]);

  const selectedPeer = useMemo(
    () => (selectedPeerId ? peers.find((peer) => peer.peer_id === selectedPeerId) ?? null : null),
    [peers, selectedPeerId]
  );

  const visiblePeers = useMemo(
    () => peers.filter((peer) => peer.role !== "service"),
    [peers]
  );

  useEffect(() => {
    const timer = window.setInterval(() => {
      void fetchOrchestrators();
    }, 30_000);
    return () => window.clearInterval(timer);
  }, [fetchOrchestrators]);

  const missingOrchestrators = useMemo(
    () => orchestrators.filter((status) => !status.present),
    [orchestrators]
  );

  const filteredPeers = useMemo(() => {
    const term = filter.trim().toLowerCase();
    if (!term) return visiblePeers;
    return visiblePeers.filter((peer) => {
      const haystack = [
        peer.name,
        peer.display_name,
        peer.circle,
        peer.backend,
        peer.path,
        peer.description,
        String(peer.metadata?.branch ?? ""),
      ]
        .filter(Boolean)
        .join(" ")
        .toLowerCase();
      return haystack.includes(term);
    });
  }, [filter, visiblePeers]);

  const counts = useMemo(() => {
    const count = { online: 0, busy: 0, offline: 0 };
    for (const peer of visiblePeers) count[peer.status] += 1;
    return count;
  }, [visiblePeers]);

  const selectPeer = useCallback((peer: Peer) => {
    setSelectedPeerId(peer.peer_id);
    setMainView("mesh");
    setMobileTab("mesh");
  }, []);

  const closePeer = useCallback(() => {
    setSelectedPeerId(null);
    setMainView("mesh");
    setMobileTab("peers");
  }, []);

  const openJobs = useCallback(() => {
    setSelectedPeerId(null);
    setMainView("jobs");
    setMobileTab("jobs");
  }, []);

  const openMesh = useCallback(() => {
    setSelectedPeerId(null);
    setMainView("mesh");
    setMobileTab("mesh");
  }, []);

  const openBeads = useCallback(() => {
    setSelectedPeerId(null);
    setMainView("beads");
  }, []);

  const handleMobileTab = useCallback((tab: MobileTab) => {
    setMobileTab(tab);
    if (tab === "mesh" || tab === "jobs") {
      setSelectedPeerId(null);
      setMainView(tab);
    }
  }, []);

  return (
    <div className="bg-surface text-on-surface font-body mesh-bg md:h-dvh md:overflow-hidden">
      <div className="flex min-h-dvh flex-col md:grid md:h-full md:min-h-0 md:grid-cols-[420px_1fr] md:grid-rows-[52px_auto_1fr]">
        <TopBar
          counts={counts}
          connection={connection}
          isRefreshing={isRefreshing}
          activeView={mainView}
          onRefresh={refreshData}
          onMesh={openMesh}
          onJobs={openJobs}
          onBeads={openBeads}
          onSpawn={() => setShowSpawn(true)}
          onSettings={() => setShowSettings(true)}
        />

        <PendingQuestions events={events} apiBase={API_BASE} />

        {missingOrchestrators.length > 0 && (
          <div className="col-span-full border-b border-error/30 bg-error-container px-3 py-2 text-on-error-container md:px-5">
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 font-mono text-[11px] leading-5">
              <span className="font-bold uppercase tracking-[0.16em]">orchestrator offline</span>
              {missingOrchestrators.map((status) => (
                <span key={status.circle} className="rounded border border-error/35 bg-error/15 px-2 py-0.5">
                  circle / {status.circle}
                </span>
              ))}
              <span className="text-on-error-container/85">
                spawn one with <code className="font-bold">repowire orchestrator start</code>
              </span>
            </div>
          </div>
        )}

        <div className={cn("min-h-0 flex-1 md:block", selectedPeer || mobileTab !== "peers" ? "hidden" : "block")}>
          <PeerRoster
            peers={filteredPeers}
            allCount={visiblePeers.length}
            selectedPeerId={selectedPeerId}
            filter={filter}
            onFilter={setFilter}
            onSelectPeer={selectPeer}
          />
        </div>

        <main className={cn("relative min-h-0 flex-1 border-l border-border-faint bg-surface-dim md:overflow-hidden", !selectedPeer && mobileTab === "peers" ? "hidden md:flex" : "flex", "flex-col")}>
          <WireTrace active={Boolean(selectedPeer)} />
          {selectedPeer ? (
            <PeerView
              peer={selectedPeer}
              events={events}
              apiBase={API_BASE}
              onClose={closePeer}
              onSent={refreshData}
            />
          ) : mainView === "jobs" ? (
            <JobsView
              apiBase={API_BASE}
              refreshSignal={refreshSignal}
            />
          ) : mainView === "beads" ? (
            <BeadsBoard apiBase={API_BASE} />
          ) : (
            <MeshFeed
              events={events}
              peers={visiblePeers}
              apiBase={API_BASE}
              onPickPeer={selectPeer}
            />
          )}
        </main>

        {!selectedPeer && (
          <MobileTabs
            activeTab={mobileTab}
            counts={counts}
            eventCount={events.length}
            onChange={handleMobileTab}
          />
        )}
      </div>

      {showSpawn && (
        <SpawnDialog
          apiBase={API_BASE}
          onClose={() => setShowSpawn(false)}
          onSpawned={refreshData}
        />
      )}
      {showSettings && (
        <SettingsDialog
          apiBase={API_BASE}
          isConnected={connection.status === "live"}
          peers={peers}
          onClose={() => setShowSettings(false)}
        />
      )}
    </div>
  );
}
