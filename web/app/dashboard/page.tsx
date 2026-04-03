"use client";

import { useState, useEffect, useCallback, useMemo, useRef } from "react";
import Image from "next/image";
import { RefreshCw } from "lucide-react";
import { cn } from "./lib/utils";
import { OverviewGrid } from "./components/OverviewGrid";
import { PeerHeader } from "./components/PeerHeader";
import { ChatPanel } from "./components/ChatPanel";
import { ComposeBar } from "./components/ComposeBar";
import { ActivityFeed } from "./components/ActivityFeed";
import { AppNav, type NavTab } from "./components/AppNav";
import { SettingsPanel } from "./components/SettingsPanel";
import { SpawnDialog } from "./components/SpawnDialog";
import type { Peer, Event } from "./types";

const API_BASE = process.env.NEXT_PUBLIC_API_BASE ?? "http://localhost:8377";

export default function Dashboard() {
  const [peers, setPeers] = useState<Peer[]>([]);
  const [events, setEvents] = useState<Event[]>([]);
  const [isConnected, setIsConnected] = useState(false);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [selectedPeerId, setSelectedPeerId] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<"chat" | "activity">("chat");
  const [activeNavTab, setActiveNavTab] = useState<NavTab>("dash");
  const [showSpawn, setShowSpawn] = useState(false);
  const [circleFilter, setCircleFilter] = useState<string | null>(null);
  const eventSourceRef = useRef<EventSource | null>(null);
  const eventIdsRef = useRef<Set<string>>(new Set());

  const fetchPeers = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/peers`);
      if (res.ok) {
        const data = await res.json();
        setPeers(data.peers || data);
      }
    } catch (error) {
      console.error("Failed to fetch peers:", error);
    }
  }, []);

  const fetchEvents = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/events`);
      if (res.ok) {
        const data: Event[] = await res.json();
        eventIdsRef.current = new Set(data.map((e) => e.id));
        setEvents(data);
      }
    } catch (error) {
      console.error("Failed to fetch events:", error);
    }
  }, []);

  const refreshData = useCallback(async () => {
    setIsRefreshing(true);
    await Promise.all([fetchPeers(), fetchEvents()]);
    setIsRefreshing(false);
  }, [fetchPeers, fetchEvents]);

  useEffect(() => {
    fetchPeers();
    fetchEvents();

    const eventSource = new EventSource(`${API_BASE}/events/stream`);
    eventSourceRef.current = eventSource;

    eventSource.onopen = () => setIsConnected(true);

    eventSource.onmessage = (e) => {
      try {
        const parsed: unknown = JSON.parse(e.data);
        if (
          typeof parsed === "object" &&
          parsed !== null &&
          "id" in parsed &&
          "type" in parsed &&
          "timestamp" in parsed &&
          typeof (parsed as Record<string, unknown>).id === "string" &&
          typeof (parsed as Record<string, unknown>).type === "string" &&
          typeof (parsed as Record<string, unknown>).timestamp === "string"
        ) {
          const event = parsed as Event;
          if (eventIdsRef.current.has(event.id)) return;
          eventIdsRef.current.add(event.id);
          setEvents((prev) => {
            const next = [...prev, event];
            return next.length > 500 ? next.slice(-500) : next;
          });
          if (event.type === "status_change") fetchPeers();
        }
      } catch (error) {
        console.error("Failed to parse SSE event:", error);
      }
    };

    eventSource.onerror = () => setIsConnected(false);

    const peersInterval = setInterval(fetchPeers, 10000);

    return () => {
      eventSource.close();
      eventSourceRef.current = null;
      clearInterval(peersInterval);
    };
  }, [fetchPeers, fetchEvents]);

  const selectedPeer = useMemo(
    () => (selectedPeerId ? peers.find((p) => p.peer_id === selectedPeerId) ?? null : null),
    [peers, selectedPeerId]
  );

  // Unique circles derived from peers (excluding service peers)
  const circles = useMemo(() => {
    const set = new Set<string>();
    for (const p of peers) {
      if (p.role !== "service" && p.circle) set.add(p.circle);
    }
    return Array.from(set).sort();
  }, [peers]);

  const handleSelectPeer = useCallback((peer: Peer) => {
    setSelectedPeerId(peer.peer_id);
    setActiveTab("chat");
  }, []);

  const handleClosePeer = useCallback(() => {
    setSelectedPeerId(null);
  }, []);

  const handleNavTabChange = useCallback((tab: NavTab) => {
    setActiveNavTab(tab);
    setSelectedPeerId(null);
  }, []);

  const handleSpawn = useCallback(() => setShowSpawn(true), []);

  return (
    <div className="h-dvh bg-surface text-on-surface font-body mesh-bg flex flex-col overflow-hidden">
      {/* Navigation: side rail on desktop, bottom tabs on mobile */}
      <AppNav
        activeTab={activeNavTab}
        onTabChange={handleNavTabChange}
        onSpawn={handleSpawn}
      />

      {/* Mobile Top App Bar (hidden on desktop) */}
      <header className="md:hidden fixed top-0 left-0 w-full z-50 flex justify-between items-center px-6 h-16 bg-surface">
        <div className="flex items-center gap-3">
          <button onClick={handleClosePeer} className="flex items-center gap-3 hover:opacity-80 transition-opacity">
            <Image src="/logo-cyan.svg" alt="Repowire" width={28} height={28} />
            <h1 className="text-xl font-bold tracking-widest text-cyan-400 font-headline uppercase">
              REPOWIRE
            </h1>
          </button>
        </div>
        <div className="flex items-center gap-4">
          <div
            className={cn(
              "flex items-center gap-2 bg-surface-container-low px-3 py-1 rounded shadow-inner",
              isConnected ? "text-secondary" : "text-error"
            )}
          >
            <span className={cn("w-2 h-2 rounded-full", isConnected ? "bg-secondary pulse-online" : "bg-error")} />
            <span className="text-[10px] font-headline font-bold uppercase tracking-widest">
              {isConnected ? "Mesh Connected" : "Disconnected"}
            </span>
          </div>
        </div>
      </header>

      {/* Desktop Top Bar (hidden on mobile) */}
      <header className="hidden md:flex fixed top-0 left-64 right-0 z-40 justify-between items-center px-8 h-16 bg-surface/80 backdrop-blur-md border-b border-outline-variant/10">
        {/* Circle filter tabs (only on Dash view) */}
        <div className="flex items-center gap-6">
          {activeNavTab === "dash" && !selectedPeer && circles.length > 1 && (
            <nav className="flex items-center gap-6">
              <button
                onClick={() => setCircleFilter(null)}
                className={cn(
                  "text-xs uppercase tracking-widest font-bold pb-1 transition-all",
                  circleFilter === null
                    ? "text-primary border-b-2 border-primary"
                    : "text-slate-400 hover:text-cyan-300"
                )}
              >
                All Circles
              </button>
              {circles.map((circle) => (
                <button
                  key={circle}
                  onClick={() => setCircleFilter(circle)}
                  className={cn(
                    "text-xs uppercase tracking-widest font-medium transition-all",
                    circleFilter === circle
                      ? "text-primary border-b-2 border-primary pb-1"
                      : "text-slate-400 hover:text-cyan-300"
                  )}
                >
                  {circle}
                </button>
              ))}
            </nav>
          )}
        </div>
        <div className="flex items-center gap-4">
          <div
            className={cn(
              "flex items-center gap-2 bg-surface-container-low px-3 py-1 rounded shadow-inner",
              isConnected ? "text-secondary" : "text-error"
            )}
          >
            <span className={cn("w-2 h-2 rounded-full", isConnected ? "bg-secondary pulse-online" : "bg-error")} />
            <span className="text-[10px] font-headline font-bold uppercase tracking-widest">
              {isConnected ? "Mesh Connected" : "Disconnected"}
            </span>
          </div>
          <button
            onClick={refreshData}
            className="w-8 h-8 rounded flex items-center justify-center hover:bg-surface-container-high transition-colors"
          >
            <RefreshCw className={cn("w-4 h-4 text-on-surface-variant", isRefreshing && "animate-spin")} />
          </button>
        </div>
      </header>

      {/* Mobile header separator */}
      <div className="md:hidden fixed top-16 left-0 w-full z-40 bg-surface-container-low h-[2px]" />

      {/* Main Content */}
      <main className="flex-1 pt-[68px] md:pt-16 pb-24 md:pb-0 md:pl-64 overflow-y-auto">
        {selectedPeer ? (
          /* Peer Detail View */
          <div className="flex flex-col h-full">
            <PeerHeader peer={selectedPeer} onClose={handleClosePeer} />

            {/* Chat/Activity Tabs */}
            <div className="flex items-center gap-1 px-4 pt-2 pb-0 shrink-0">
              {(["chat", "activity"] as const).map((tab) => (
                <button
                  key={tab}
                  onClick={() => setActiveTab(tab)}
                  className={cn(
                    "px-3 py-2 text-[10px] font-headline font-bold uppercase tracking-widest transition-colors border-b-2 -mb-px",
                    activeTab === tab
                      ? "border-primary text-primary"
                      : "border-transparent text-outline hover:text-on-surface-variant"
                  )}
                >
                  {tab}
                </button>
              ))}
              {isConnected && (
                <div className="ml-auto flex items-center gap-2 pb-2">
                  <span className="w-2 h-2 rounded-full bg-secondary pulse-online" />
                  <span className="text-[10px] font-mono text-outline uppercase tracking-widest">live</span>
                </div>
              )}
            </div>

            {activeTab === "chat" ? (
              <div className="flex-1 flex flex-col overflow-hidden">
                <div className="flex-1 overflow-y-auto">
                  <ChatPanel peer={selectedPeer} events={events} />
                </div>
                <ComposeBar key={selectedPeer.peer_id} peer={selectedPeer} apiBase={API_BASE} onSent={refreshData} />
              </div>
            ) : (
              <div className="flex-1 overflow-y-auto px-4 py-4">
                <ActivityFeed events={events} peerFilter={selectedPeer.peer_id} peerName={selectedPeer.name} />
              </div>
            )}
          </div>
        ) : (
          /* Tab Content */
          <>
            {activeNavTab === "dash" && (
              <OverviewGrid
                peers={peers}
                events={events}
                onSelectPeer={handleSelectPeer}
                circleFilter={circleFilter}
              />
            )}
            {activeNavTab === "logs" && (
              <div className="px-4 max-w-2xl md:max-w-4xl mx-auto">
                <ActivityFeed events={events} peers={peers} />
              </div>
            )}
            {activeNavTab === "config" && (
              <SettingsPanel apiBase={API_BASE} isConnected={isConnected} peers={peers} />
            )}
          </>
        )}
      </main>

      {/* Spawn Dialog */}
      {showSpawn && (
        <SpawnDialog
          apiBase={API_BASE}
          onClose={() => setShowSpawn(false)}
          onSpawned={refreshData}
        />
      )}
    </div>
  );
}
