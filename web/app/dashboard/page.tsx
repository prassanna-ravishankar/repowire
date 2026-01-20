"use client";

import React, { useState, useEffect, useCallback } from "react";
import { 
  Activity, 
  Users, 
  MessageSquare, 
  Settings, 
  RefreshCw, 
  Terminal, 
  Shield, 
  Wifi, 
  WifiOff,
  Search,
  ChevronRight,
  Clock
} from "lucide-react";
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
  last_seen?: string;
}

interface Event {
  id: string;
  type: "query" | "response" | "notification" | "broadcast";
  timestamp: string;
  from?: string;
  to?: string;
  text: string;
  status?: "pending" | "success" | "error";
}

const API_BASE = "http://localhost:8377";

export default function Dashboard() {
  const [peers, setPeers] = useState<Peer[]>([]);
  const [events, setEvents] = useState<Event[]>([]);
  const [isConnected, setIsConnected] = useState(false);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [activeTab, setActiveTab] = useState<"events" | "peers">("events");
  const [searchQuery, setSearchQuery] = useState("");

  const fetchData = useCallback(async () => {
    setIsRefreshing(true);
    try {
      const [peersRes, eventsRes] = await Promise.all([
        fetch(`${API_BASE}/peers`),
        fetch(`${API_BASE}/events`)
      ]);

      if (peersRes.ok && eventsRes.ok) {
        const peersData = await peersRes.json();
        const eventsData = await eventsRes.json();
        setPeers(peersData);
        setEvents(eventsData.reverse()); // Show newest first
        setIsConnected(true);
      } else {
        setIsConnected(false);
      }
    } catch (error) {
      console.error("Failed to fetch dashboard data:", error);
      setIsConnected(false);
    } finally {
      setIsRefreshing(false);
    }
  }, []);

  useEffect(() => {
    fetchData();
    const interval = setInterval(fetchData, 3000); // Poll every 3 seconds
    return () => clearInterval(interval);
  }, [fetchData]);

  const filteredEvents = events.filter(e => 
    e.text.toLowerCase().includes(searchQuery.toLowerCase()) ||
    e.from?.toLowerCase().includes(searchQuery.toLowerCase()) ||
    e.to?.toLowerCase().includes(searchQuery.toLowerCase())
  );

  return (
    <div className="min-h-screen bg-[#0a0a0a] text-zinc-400 font-sans selection:bg-blue-500/30">
      {/* Header */}
      <header className="border-b border-zinc-800/50 bg-[#0a0a0a]/80 backdrop-blur-md sticky top-0 z-50">
        <div className="max-w-7xl mx-auto px-4 h-16 flex items-center justify-between">
          <div className="flex items-center gap-4">
            <div className="flex items-center gap-2">
              <div className="w-8 h-8 bg-blue-600 rounded-lg flex items-center justify-center">
                <Activity className="w-5 h-5 text-white" />
              </div>
              <span className="text-white font-bold tracking-tight text-lg">REPOWIRE</span>
              <span className="text-zinc-600 font-mono text-xs border border-zinc-800 px-1.5 py-0.5 rounded uppercase">Control Plane</span>
            </div>
          </div>

          <div className="flex items-center gap-4">
            <div className={cn(
              "flex items-center gap-2 px-3 py-1 rounded-full text-xs font-medium border transition-colors",
              isConnected 
                ? "bg-emerald-500/10 border-emerald-500/20 text-emerald-500" 
                : "bg-red-500/10 border-red-500/20 text-red-500"
            )}>
              {isConnected ? <Wifi className="w-3.5 h-3.5" /> : <WifiOff className="w-3.5 h-3.5" />}
              {isConnected ? "Local Daemon Connected" : "Connection Lost"}
            </div>
            <button 
              onClick={fetchData}
              className="p-2 hover:bg-zinc-800 rounded-lg transition-colors group"
            >
              <RefreshCw className={cn("w-4 h-4 transition-transform", isRefreshing && "animate-spin")} />
            </button>
            <div className="w-px h-4 bg-zinc-800" />
            <button className="p-2 hover:bg-zinc-800 rounded-lg transition-colors">
              <Settings className="w-4 h-4" />
            </button>
          </div>
        </div>
      </header>

      <main className="max-w-7xl mx-auto px-4 py-8">
        <div className="grid grid-cols-12 gap-8">
          {/* Sidebar Stats */}
          <div className="col-span-12 lg:col-span-3 space-y-6">
            <div className="grid grid-cols-1 gap-4">
              <StatCard 
                label="Active Peers" 
                value={peers.filter(p => p.status === 'online').length.toString()} 
                icon={<Users className="w-4 h-4" />}
                trend={`${peers.length} Total`}
              />
              <StatCard 
                label="Messages Today" 
                value={events.length.toString()} 
                icon={<MessageSquare className="w-4 h-4" />}
                trend="Live Updates"
              />
            </div>

            <div className="bg-zinc-900/50 border border-zinc-800/50 rounded-xl overflow-hidden">
              <div className="px-4 py-3 border-b border-zinc-800/50 flex items-center justify-between">
                <h3 className="text-xs font-bold uppercase tracking-wider text-zinc-500">Live Peers</h3>
                <span className="flex h-2 w-2 rounded-full bg-emerald-500 animate-pulse" />
              </div>
              <div className="p-2 space-y-1">
                {peers.map(peer => (
                  <div key={peer.name} className="flex items-center justify-between p-2 rounded-lg hover:bg-zinc-800/50 transition-colors group">
                    <div className="flex items-center gap-3">
                      <div className={cn(
                        "w-2 h-2 rounded-full",
                        peer.status === 'online' ? "bg-emerald-500 shadow-[0_0_8px_rgba(16,185,129,0.5)]" : 
                        peer.status === 'busy' ? "bg-amber-500" : "bg-zinc-700"
                      )} />
                      <span className="text-sm font-medium text-zinc-300">{peer.name}</span>
                    </div>
                    <ChevronRight className="w-3.5 h-3.5 opacity-0 group-hover:opacity-100 transition-opacity" />
                  </div>
                ))}
                {peers.length === 0 && (
                  <div className="p-4 text-center text-xs text-zinc-600 italic">No peers registered</div>
                )}
              </div>
            </div>

            <div className="bg-blue-600/5 border border-blue-500/10 rounded-xl p-4">
              <div className="flex items-center gap-2 mb-2 text-blue-400">
                <Shield className="w-4 h-4" />
                <span className="text-xs font-bold uppercase tracking-wider">Relay Mode</span>
              </div>
              <p className="text-xs leading-relaxed text-zinc-500 mb-3">
                Current session is running in local mode. Connect a secret key to enable multi-machine mesh.
              </p>
              
              <div className="space-y-2">
                <input 
                  type="password"
                  placeholder="Enter Relay API Key..."
                  className="w-full bg-black/40 border border-zinc-800 rounded-lg px-3 py-2 text-xs focus:outline-none focus:border-blue-500/50 transition-all"
                  onChange={(e) => {
                    // In a real app, we'd save this to localStorage or a state management store
                    console.log("API Key updated");
                  }}
                />
                <button className="w-full py-2 bg-blue-600 hover:bg-blue-500 text-white text-xs font-bold rounded-lg transition-colors">
                  Enable Remote Relay
                </button>
              </div>
            </div>
          </div>

          {/* Main Content */}
          <div className="col-span-12 lg:col-span-9 space-y-6">
            <div className="flex items-center justify-between gap-4">
              <div className="flex bg-zinc-900/50 border border-zinc-800/50 p-1 rounded-xl">
                <button 
                  onClick={() => setActiveTab("events")}
                  className={cn(
                    "px-4 py-2 rounded-lg text-sm font-medium transition-all",
                    activeTab === "events" ? "bg-zinc-800 text-white shadow-sm" : "hover:text-zinc-300"
                  )}
                >
                  Live Transcript
                </button>
                <button 
                  onClick={() => setActiveTab("peers")}
                  className={cn(
                    "px-4 py-2 rounded-lg text-sm font-medium transition-all",
                    activeTab === "peers" ? "bg-zinc-800 text-white shadow-sm" : "hover:text-zinc-300"
                  )}
                >
                  Network Graph
                </button>
              </div>

              <div className="relative flex-1 max-w-sm">
                <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-zinc-600" />
                <input 
                  type="text" 
                  placeholder="Filter events..."
                  value={searchQuery}
                  onChange={(e) => setSearchQuery(e.target.value)}
                  className="w-full bg-zinc-900/50 border border-zinc-800/50 rounded-xl py-2 pl-10 pr-4 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500/20 focus:border-blue-500/50 transition-all"
                />
              </div>
            </div>

            <div className="bg-zinc-900/50 border border-zinc-800/50 rounded-2xl overflow-hidden min-h-[600px] flex flex-col">
              {activeTab === "events" ? (
                <div className="flex-1 overflow-y-auto p-6 space-y-4">
                  {filteredEvents.map((event, i) => (
                    <EventCard key={event.id || i} event={event} />
                  ))}
                  {filteredEvents.length === 0 && (
                    <div className="h-full flex flex-col items-center justify-center text-zinc-600 space-y-4 py-20">
                      <Terminal className="w-12 h-12 opacity-20" />
                      <p className="text-sm italic">Waiting for agent communication...</p>
                    </div>
                  )}
                </div>
              ) : (
                <div className="flex-1 flex flex-col items-center justify-center text-zinc-600 space-y-4 p-6">
                  <div className="w-24 h-24 rounded-full border-2 border-dashed border-zinc-800 flex items-center justify-center animate-spin-slow">
                    <RefreshCw className="w-8 h-8 opacity-20" />
                  </div>
                  <p className="text-sm font-medium">Network Visualization coming soon</p>
                </div>
              )}
            </div>
          </div>
        </div>
      </main>
    </div>
  );
}

function StatCard({ label, value, icon, trend }: { label: string, value: string, icon: React.ReactNode, trend: string }) {
  return (
    <div className="bg-zinc-900/50 border border-zinc-800/50 rounded-xl p-4">
      <div className="flex items-center gap-3 mb-3">
        <div className="p-2 bg-zinc-800 rounded-lg text-zinc-400">
          {icon}
        </div>
        <span className="text-xs font-bold uppercase tracking-wider text-zinc-500">{label}</span>
      </div>
      <div className="flex items-baseline gap-2">
        <span className="text-2xl font-bold text-white">{value}</span>
        <span className="text-[10px] text-zinc-600 font-mono uppercase">{trend}</span>
      </div>
    </div>
  );
}

function EventCard({ event }: { event: Event }) {
  const isResponse = event.type === 'response';
  const isQuery = event.type === 'query';
  
  return (
    <div className={cn(
      "group relative flex gap-4 p-4 rounded-xl border transition-all",
      isResponse ? "bg-emerald-500/[0.02] border-emerald-500/10" : "bg-zinc-800/20 border-zinc-800/50 hover:border-zinc-700"
    )}>
      <div className="flex flex-col items-center gap-2">
        <div className={cn(
          "w-8 h-8 rounded-lg flex items-center justify-center shrink-0",
          event.type === 'query' ? "bg-blue-500/10 text-blue-500" :
          event.type === 'response' ? "bg-emerald-500/10 text-emerald-500" :
          event.type === 'broadcast' ? "bg-purple-500/10 text-purple-500" :
          "bg-zinc-800 text-zinc-400"
        )}>
          {event.type === 'query' ? <Search className="w-4 h-4" /> :
           event.type === 'response' ? <MessageSquare className="w-4 h-4" /> :
           event.type === 'broadcast' ? <Activity className="w-4 h-4" /> :
           <Terminal className="w-4 h-4" />}
        </div>
        <div className="w-px flex-1 bg-zinc-800" />
      </div>

      <div className="flex-1 min-w-0 space-y-2">
        <div className="flex items-center justify-between gap-4">
          <div className="flex items-center gap-2 text-xs">
            {event.from && (
              <>
                <span className="font-bold text-zinc-300 px-1.5 py-0.5 bg-zinc-800 rounded">@{event.from}</span>
                <ChevronRight className="w-3 h-3 text-zinc-600" />
              </>
            )}
            {event.to ? (
              <span className="font-bold text-zinc-300 px-1.5 py-0.5 bg-zinc-800 rounded">@{event.to}</span>
            ) : (
              <span className="text-zinc-600 uppercase font-mono tracking-widest">Broadcast</span>
            )}
          </div>
          <div className="flex items-center gap-2 text-[10px] text-zinc-600 font-mono">
            <Clock className="w-3 h-3" />
            {new Date(event.timestamp).toLocaleTimeString()}
          </div>
        </div>

        <div className="bg-[#050505] border border-zinc-800/50 rounded-lg p-3 font-mono text-sm overflow-hidden relative">
          <div className="flex items-center gap-2 mb-1.5 opacity-50">
             <div className="w-2 h-2 rounded-full bg-zinc-800" />
             <div className="w-2 h-2 rounded-full bg-zinc-800" />
             <div className="w-2 h-2 rounded-full bg-zinc-800" />
             <span className="text-[10px] uppercase tracking-tighter ml-1">{event.type}</span>
          </div>
          <p className={cn(
            "leading-relaxed whitespace-pre-wrap",
            event.status === 'error' ? "text-red-400" : "text-zinc-300"
          )}>
            {event.text}
          </p>
          
          {isQuery && event.status === 'pending' && (
            <div className="mt-2 flex items-center gap-2 text-blue-400 text-[10px] uppercase font-bold animate-pulse">
              <RefreshCw className="w-3 h-3 animate-spin" />
              Awaiting Agent Response...
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
