"use client";

import React, { useState, useEffect, useCallback, useMemo } from "react";
import { RefreshCw, Wifi, WifiOff } from "lucide-react";
import { CircleGroup } from "./components/CircleGroup";
import { ActivityFeed } from "./components/ActivityFeed";
import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

interface Peer {
  name: string;
  status: "online" | "busy" | "offline";
  machine: string;
  path: string;
  tmux_session?: string;
  circle: string;
  last_seen?: string;
  metadata?: {
    branch?: string;
    [key: string]: unknown;
  };
}

interface Event {
  id: string;
  type: "query" | "response" | "notification" | "broadcast" | "status_change";
  timestamp: string;
  from?: string;
  to?: string;
  text: string;
  status?: "pending" | "success" | "error";
  peer?: string;
  new_status?: "online" | "busy" | "offline";
  query_id?: string;
}

interface Conversation {
  id: string;
  from: string;
  to: string;
  query: Event;
  response?: Event;
  timestamp: string;
  status: "pending" | "success" | "error";
}

const API_BASE = process.env.NEXT_PUBLIC_API_BASE || "http://localhost:8377";

export default function Dashboard() {
  const [peers, setPeers] = useState<Peer[]>([]);
  const [events, setEvents] = useState<Event[]>([]);
  const [isConnected, setIsConnected] = useState(false);
  const [isRefreshing, setIsRefreshing] = useState(false);

  // Fetch peers via REST
  const fetchPeers = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/peers`);
      if (res.ok) {
        const data = await res.json();
        setPeers(data.peers || data);
        setIsConnected(true);
      }
    } catch (error) {
      console.error("Failed to fetch peers:", error);
    }
  }, []);

  // Fetch initial events via REST
  const fetchEvents = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/events`);
      if (res.ok) {
        const data = await res.json();
        setEvents(data);
      }
    } catch (error) {
      console.error("Failed to fetch events:", error);
    }
  }, []);

  // Manual refresh
  const refreshData = useCallback(async () => {
    setIsRefreshing(true);
    await Promise.all([fetchPeers(), fetchEvents()]);
    setIsRefreshing(false);
  }, [fetchPeers, fetchEvents]);

  // SSE for real-time event streaming
  useEffect(() => {
    fetchPeers();
    fetchEvents();

    const eventSource = new EventSource(`${API_BASE}/events/stream`);

    eventSource.onopen = () => {
      setIsConnected(true);
    };

    eventSource.onmessage = (e) => {
      try {
        const event = JSON.parse(e.data);
        setEvents((prev) => {
          if (prev.some((existing) => existing.id === event.id)) {
            return prev;
          }
          return [...prev, event];
        });
      } catch (error) {
        console.error("Failed to parse SSE event:", error);
      }
    };

    eventSource.onerror = () => {
      setIsConnected(false);
    };

    const peersInterval = setInterval(fetchPeers, 10000);

    return () => {
      eventSource.close();
      clearInterval(peersInterval);
    };
  }, [fetchPeers, fetchEvents]);

  // Group events into conversations
  const conversations: Conversation[] = useMemo(() => {
    const convos: Conversation[] = [];
    const queryEvents = events.filter((e) => e.type === "query");
    const responseEvents = events.filter((e) => e.type === "response");

    for (const query of queryEvents) {
      const response = responseEvents.find((r) => r.query_id === query.id);

      convos.push({
        id: query.id,
        from: query.from || "unknown",
        to: query.to || "unknown",
        query,
        response,
        timestamp: query.timestamp,
        status: query.status === "error" ? "error" : response ? "success" : "pending",
      });
    }

    return convos.sort(
      (a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime()
    );
  }, [events]);

  // Filter to online/busy peers only, group by circle
  const circleGroups = useMemo(() => {
    const onlinePeers = peers.filter((p) => p.status === "online" || p.status === "busy");
    return onlinePeers.reduce((acc, peer) => {
      const circle = peer.circle || "global";
      if (!acc[circle]) acc[circle] = [];
      acc[circle].push(peer);
      return acc;
    }, {} as Record<string, Peer[]>);
  }, [peers]);

  const onlineCount = peers.filter((p) => p.status === "online" || p.status === "busy").length;

  return (
    <div className="min-h-screen bg-zinc-950 text-zinc-400 font-sans p-6">
      {/* Header */}
      <header className="flex items-center justify-between mb-8">
        <div className="flex items-center gap-3">
          <img src="/logo-dark.webp" alt="Repowire" className="w-8 h-8 rounded-lg" />
          <span className="text-white font-bold tracking-tight text-lg">REPOWIRE</span>
        </div>

        <div className="flex items-center gap-4">
          <div
            className={cn(
              "flex items-center gap-2 px-3 py-1 rounded-full text-xs font-medium",
              isConnected ? "text-emerald-500" : "text-red-500"
            )}
          >
            {isConnected ? <Wifi className="w-3.5 h-3.5" /> : <WifiOff className="w-3.5 h-3.5" />}
            <span className="tabular-nums">{onlineCount} peers online</span>
          </div>
          <button
            onClick={refreshData}
            className="p-2 hover:bg-zinc-800 rounded-lg transition-colors"
          >
            <RefreshCw className={cn("w-4 h-4", isRefreshing && "animate-spin")} />
          </button>
        </div>
      </header>

      {/* Peer Circles - horizontal flex with wrap */}
      <section className="mb-8">
        {Object.keys(circleGroups).length === 0 ? (
          <div className="border border-zinc-800 rounded-lg p-8 text-center">
            <p className="text-zinc-500 text-sm">No online peers</p>
            <p className="text-zinc-600 text-xs mt-1">
              Start Claude sessions in tmux to see peers here
            </p>
          </div>
        ) : (
          <div className="flex flex-wrap gap-4">
            {Object.entries(circleGroups).map(([circle, circlePeers]) => (
              <CircleGroup key={circle} name={circle} peers={circlePeers} />
            ))}
          </div>
        )}
      </section>

      {/* Conversations - with subtle background differentiation */}
      <section className="bg-zinc-900/40 rounded-xl p-4 border border-zinc-800/50">
        <div className="flex items-center justify-between mb-4">
          <span className="text-xs font-mono text-zinc-500 uppercase tracking-wider">Conversations</span>
          <div className="flex items-center gap-2">
            <span className="w-2 h-2 rounded-full bg-emerald-500 animate-pulse" />
            <span className="text-xs text-zinc-600">live</span>
          </div>
        </div>
        <div className="max-h-[500px] overflow-y-auto">
          <ActivityFeed events={events} conversations={conversations} />
        </div>
      </section>
    </div>
  );
}
