"use client";

import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import Image from "next/image";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import {
  AlertCircle,
  Bot,
  Check,
  Copy,
  Paperclip,
  Play,
  Plus,
  RefreshCw,
  Search,
  Send,
  Settings,
  X,
} from "lucide-react";
import { cn, shortPath, statusDot } from "./lib/utils";
import type { Event, Peer } from "./types";
import { peerLabel } from "./types";

const API_BASE = process.env.NEXT_PUBLIC_API_BASE ?? "http://localhost:8377";

type MobileTab = "peers" | "mesh";
type ComposeMode = "notify" | "ask";

interface SpawnConfig {
  enabled: boolean;
  allowed_commands: string[];
  allowed_paths: string[];
}

export default function Dashboard() {
  const [peers, setPeers] = useState<Peer[]>([]);
  const [events, setEvents] = useState<Event[]>([]);
  const [isConnected, setIsConnected] = useState(false);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [selectedPeerId, setSelectedPeerId] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [mobileTab, setMobileTab] = useState<MobileTab>("peers");
  const [showSpawn, setShowSpawn] = useState(false);
  const [showSettings, setShowSettings] = useState(false);
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
        eventIdsRef.current = new Set(data.map((event) => event.id));
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
  }, [fetchEvents, fetchPeers]);

  useEffect(() => {
    queueMicrotask(() => {
      fetchPeers();
      fetchEvents();
    });

    const eventSource = new EventSource(`${API_BASE}/events/stream`);
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

    return () => eventSource.close();
  }, [fetchEvents, fetchPeers]);

  const selectedPeer = useMemo(
    () => (selectedPeerId ? peers.find((peer) => peer.peer_id === selectedPeerId) ?? null : null),
    [peers, selectedPeerId]
  );

  const visiblePeers = useMemo(
    () => peers.filter((peer) => peer.role !== "service"),
    [peers]
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
    setMobileTab("mesh");
  }, []);

  const closePeer = useCallback(() => {
    setSelectedPeerId(null);
    setMobileTab("peers");
  }, []);

  return (
    <div className="h-dvh overflow-hidden bg-surface text-on-surface font-body mesh-bg">
      <div className="grid h-full grid-rows-[48px_1fr_56px] md:grid-cols-[420px_1fr] md:grid-rows-[52px_1fr]">
        <TopBar
          counts={counts}
          isConnected={isConnected}
          isRefreshing={isRefreshing}
          onRefresh={refreshData}
          onSpawn={() => setShowSpawn(true)}
          onSettings={() => setShowSettings(true)}
        />

        <div className={cn("min-h-0 md:block", selectedPeer || mobileTab === "mesh" ? "hidden" : "block")}>
          <PeerRoster
            peers={filteredPeers}
            allCount={visiblePeers.length}
            selectedPeerId={selectedPeerId}
            filter={filter}
            onFilter={setFilter}
            onSelectPeer={selectPeer}
          />
        </div>

        <main className={cn("relative min-h-0 overflow-hidden border-l border-border-faint bg-surface-dim", !selectedPeer && mobileTab === "peers" ? "hidden md:flex" : "flex", "flex-col")}>
          <WireTrace active={Boolean(selectedPeer)} />
          {selectedPeer ? (
            <PeerView
              peer={selectedPeer}
              events={events}
              apiBase={API_BASE}
              onClose={closePeer}
              onSent={refreshData}
            />
          ) : (
            <MeshFeed events={events} peers={visiblePeers} onPickPeer={selectPeer} />
          )}
        </main>

        {!selectedPeer && (
          <MobileTabs
            activeTab={mobileTab}
            counts={counts}
            eventCount={events.length}
            onChange={setMobileTab}
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
          isConnected={isConnected}
          peers={peers}
          onClose={() => setShowSettings(false)}
        />
      )}
    </div>
  );
}

function TopBar({
  counts,
  isConnected,
  isRefreshing,
  onRefresh,
  onSpawn,
  onSettings,
}: {
  counts: Record<"online" | "busy" | "offline", number>;
  isConnected: boolean;
  isRefreshing: boolean;
  onRefresh: () => void;
  onSpawn: () => void;
  onSettings: () => void;
}) {
  return (
    <header className="col-span-full flex h-12 items-center gap-3 border-b border-border-faint bg-surface-dim px-3 md:h-[52px] md:px-5">
      <div className="flex min-w-0 items-center gap-3 md:w-[397px]">
        <Image src="/brand/logo-mark-copper.svg" alt="" width={22} height={24} priority />
        <span className="font-headline text-xs font-bold tracking-[0.2em] text-on-surface">REPOWIRE</span>
        <span className="hidden font-mono text-[10px] font-semibold tracking-[0.18em] text-outline md:inline">DASH</span>
      </div>

      <div className="hidden flex-1 items-center gap-5 md:flex">
        <CountPill label="ONLINE" value={counts.online} tone="online" />
        <CountPill label="BUSY" value={counts.busy} tone="busy" />
        <CountPill label="OFFLINE" value={counts.offline} tone="offline" />
        <span className="ml-auto font-mono text-[11px] text-outline">daemon {">"} 127.0.0.1:8377</span>
      </div>

      <div
        className={cn(
          "ml-auto flex items-center gap-2 border px-2.5 py-1 font-mono text-[10px] font-semibold tracking-[0.16em] md:ml-0",
          isConnected
            ? "border-secondary/25 bg-secondary/10 text-secondary"
            : "border-error/25 bg-error/10 text-error"
        )}
      >
        <span className={cn("h-2 w-2 rounded-full", isConnected ? "bg-secondary pulse-online" : "bg-error")} />
        <span className="hidden sm:inline">{isConnected ? "MESH CONNECTED" : "DISCONNECTED"}</span>
        <span className="sm:hidden">{isConnected ? "LIVE" : "DOWN"}</span>
      </div>

      <button
        onClick={onSpawn}
        className="inline-flex h-8 items-center gap-1.5 rounded bg-primary px-2.5 font-mono text-[10px] font-bold uppercase tracking-[0.12em] text-on-primary transition-[filter,transform] hover:brightness-110 active:scale-[0.98] md:px-3"
      >
        <Plus className="h-3.5 w-3.5" />
        <span className="hidden md:inline">Spawn peer</span>
      </button>
      <button
        onClick={onRefresh}
        aria-label="Refresh"
        className="hidden h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface md:inline-flex"
      >
        <RefreshCw className={cn("h-3.5 w-3.5", isRefreshing && "animate-spin")} />
      </button>
      <button
        onClick={onSettings}
        aria-label="Open settings"
        className="h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface inline-flex"
      >
        <Settings className="h-3.5 w-3.5" />
      </button>
    </header>
  );
}

function CountPill({ label, value, tone }: { label: string; value: number; tone: "online" | "busy" | "offline" }) {
  const color = tone === "online" ? "text-secondary" : tone === "busy" ? "text-tertiary-fixed-dim" : "text-outline";
  const dot = tone === "online" ? "bg-secondary pulse-online" : tone === "busy" ? "bg-tertiary-fixed-dim glow-busy" : "bg-outline";
  return (
    <span className="inline-flex items-baseline gap-2">
      <span className={cn("h-2 w-2 rounded-full", dot)} />
      <span className={cn("font-mono text-sm font-bold tabular-nums", color)}>{value}</span>
      <span className="font-mono text-[9px] font-semibold tracking-[0.18em] text-outline">{label}</span>
    </span>
  );
}

function PeerRoster({
  peers,
  allCount,
  selectedPeerId,
  filter,
  onFilter,
  onSelectPeer,
}: {
  peers: Peer[];
  allCount: number;
  selectedPeerId: string | null;
  filter: string;
  onFilter: (value: string) => void;
  onSelectPeer: (peer: Peer) => void;
}) {
  const byCircle = useMemo(() => {
    const grouped = new Map<string, Peer[]>();
    for (const peer of peers) {
      const circle = peer.circle || "default";
      grouped.set(circle, [...(grouped.get(circle) ?? []), peer]);
    }
    for (const list of grouped.values()) {
      list.sort((a, b) => statusRank(a.status) - statusRank(b.status) || peerLabel(a).localeCompare(peerLabel(b)));
    }
    return Array.from(grouped.entries()).sort(([a], [b]) => a.localeCompare(b));
  }, [peers]);

  return (
    <aside className="flex h-full min-h-0 flex-col overflow-hidden bg-surface-dim">
      <div className="flex items-center gap-2 border-b border-border-faint px-3 py-2">
        <Search className="h-3.5 w-3.5 shrink-0 text-outline" />
        <input
          value={filter}
          onChange={(event) => onFilter(event.target.value)}
          placeholder="filter peers, circles, paths..."
          className="min-w-0 flex-1 bg-transparent font-mono text-xs text-on-surface outline-none placeholder:text-outline"
        />
        <span className="font-mono text-[10px] text-outline tabular-nums">{peers.length}/{allCount}</span>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">
        {byCircle.map(([circle, list]) => (
          <section key={circle}>
            <div className="flex items-baseline justify-between px-3.5 pb-1.5 pt-3 font-mono text-[9px] font-semibold uppercase tracking-[0.2em] text-outline">
              <span>circle / {circle}</span>
              <span>{list.length}</span>
            </div>
            {list.map((peer) => (
              <PeerRow
                key={peer.peer_id}
                peer={peer}
                active={peer.peer_id === selectedPeerId}
                onClick={() => onSelectPeer(peer)}
              />
            ))}
          </section>
        ))}
        {peers.length === 0 && (
          <div className="px-4 py-12 text-center font-mono text-xs leading-6 text-outline">
            <div className="mb-1 text-on-surface-variant">&gt; no peers match</div>
            try a wider filter or start an agent session
          </div>
        )}
      </div>
    </aside>
  );
}

function PeerRow({ peer, active, onClick }: { peer: Peer; active: boolean; onClick: () => void }) {
  const { folder, parent } = peer.path ? shortPath(peer.path) : { folder: "", parent: "" };
  return (
    <button
      onClick={onClick}
      aria-pressed={active}
      className={cn(
        "block w-full border-b border-border-faint border-l-2 px-3 py-2.5 text-left transition-colors",
        active
          ? "border-l-primary bg-primary/10 text-primary-fixed"
          : "border-l-transparent text-on-surface hover:bg-surface-container"
      )}
    >
      <div className="mb-1 flex min-w-0 items-center gap-2.5">
        <span className={cn("h-2 w-2 shrink-0 rounded-full", statusDot(peer.status))} />
        <span className="min-w-0 flex-1 truncate font-mono text-[13px] font-semibold">{peerLabel(peer)}</span>
        <StatusLabel status={peer.status} />
      </div>
      <div className="ml-[18px] truncate font-mono text-[11px] leading-5 text-outline">
        {peer.backend || "agent"} · {peer.metadata?.branch ? String(peer.metadata.branch) : peer.circle}
      </div>
      {peer.path && (
        <div className="ml-[18px] truncate font-mono text-[11px] leading-5 text-outline">
          {parent}<span className="text-on-surface-variant">{folder}</span>
        </div>
      )}
      {peer.description && (
        <div className="ml-[18px] truncate font-mono text-[11px] leading-5 text-tertiary-fixed-dim">
          ↳ {peer.description}
        </div>
      )}
    </button>
  );
}

function MeshFeed({ events, peers, onPickPeer }: { events: Event[]; peers: Peer[]; onPickPeer: (peer: Peer) => void }) {
  const scrollerRef = useRef<HTMLDivElement>(null);
  const feedEvents = useMemo(
    () => events.filter((event) => event.type !== "chat_turn").sort((a, b) => a.timestamp.localeCompare(b.timestamp)),
    [events]
  );

  useEffect(() => {
    const scroller = scrollerRef.current;
    if (scroller) scroller.scrollTop = scroller.scrollHeight;
  }, [feedEvents.length]);

  const pickPeerByName = (name?: string) => {
    if (!name) return;
    const normalized = name.replace(/^@/, "");
    const peer = peers.find((item) => item.name === normalized || peerLabel(item) === normalized || `@${item.name}` === name);
    if (peer) onPickPeer(peer);
  };

  return (
    <>
      <div className="flex items-baseline justify-between border-b border-border-faint px-4 py-3 md:px-6">
        <div>
          <div className="font-mono text-[10px] font-semibold uppercase tracking-[0.22em] text-primary">LIVE / mesh.log</div>
          <h1 className="mt-1 font-headline text-2xl font-bold text-on-surface">tail -f</h1>
        </div>
        <div className="text-right font-mono text-[11px] leading-5 text-outline">
          {feedEvents.length} events<br />
          <span className="text-outline">select a peer to chat ↳</span>
        </div>
      </div>
      <div ref={scrollerRef} className="min-h-0 flex-1 overflow-y-auto bg-surface-dim px-4 py-3 md:px-5">
        {feedEvents.length === 0 ? (
          <div className="py-14 text-center font-mono text-xs leading-6 text-outline">
            <div className="text-on-surface-variant">&gt; no mesh events yet</div>
            send a message to start the log
          </div>
        ) : (
          feedEvents.map((event) => (
            <EventRow key={event.id} event={event} onPickPeer={pickPeerByName} />
          ))
        )}
      </div>
    </>
  );
}

function EventRow({ event, onPickPeer }: { event: Event; onPickPeer: (name?: string) => void }) {
  const route = event.type === "broadcast" ? "=>" : event.type === "response" ? "↳" : "->";
  const color =
    event.status === "error"
      ? "text-error"
      : event.type === "query"
      ? "text-primary-fixed"
      : event.type === "response"
      ? "text-secondary"
      : event.type === "notification"
      ? "text-tertiary-fixed-dim"
      : "text-accent";
  const to = event.type === "broadcast" ? "* (all)" : event.to || "—";

  if (event.type === "status_change") {
    return (
      <div className="grid grid-cols-[62px_1fr] gap-3 border-b border-border-faint/70 py-1.5 font-mono text-xs leading-5">
        <span className="text-outline tabular-nums">{formatTime(event.timestamp)}</span>
        <span className="truncate text-outline">
          status {event.peer || event.peer_id || "peer"} {">"} <span className="text-on-surface-variant">{event.new_status}</span>
        </span>
      </div>
    );
  }

  return (
    <div className="grid grid-cols-[62px_minmax(70px,120px)_18px_minmax(70px,120px)_1fr] gap-2 border-b border-border-faint/70 py-1.5 font-mono text-xs leading-5 md:gap-3">
      <span className="text-outline tabular-nums">{formatTime(event.timestamp)}</span>
      <button onClick={() => onPickPeer(event.from)} className={cn("truncate text-left", color)}>
        {event.from || "unknown"}
      </button>
      <span className="text-center text-outline">{route}</span>
      <button onClick={() => onPickPeer(event.to)} className="truncate text-left text-primary-fixed">
        {to}
      </button>
      <span className={cn("min-w-0 break-words", event.status === "error" ? "text-error" : "text-on-surface-variant")}>
        {event.text}
      </span>
    </div>
  );
}

function PeerView({
  peer,
  events,
  apiBase,
  onClose,
  onSent,
}: {
  peer: Peer;
  events: Event[];
  apiBase: string;
  onClose: () => void;
  onSent: () => void;
}) {
  const scrollerRef = useRef<HTMLDivElement>(null);
  const thread = useMemo(() => {
    const id = peer.peer_id;
    return events
      .filter((event) => {
        if (event.type === "chat_turn") return event.peer_id === id;
        return event.from_peer_id === id || event.to_peer_id === id;
      })
      .sort((a, b) => a.timestamp.localeCompare(b.timestamp));
  }, [events, peer.peer_id]);

  useEffect(() => {
    const scroller = scrollerRef.current;
    if (scroller) scroller.scrollTop = scroller.scrollHeight;
  }, [thread.length]);

  const { folder, parent } = peer.path ? shortPath(peer.path) : { folder: "", parent: "" };

  return (
    <>
      <div className="flex items-center gap-3 border-b border-border-faint px-4 py-3 md:px-6">
        <span className={cn("h-2.5 w-2.5 rounded-full", statusDot(peer.status))} />
        <div className="min-w-0 flex-1">
          <h1 className="truncate font-headline text-lg font-bold text-on-surface">{peerLabel(peer)}</h1>
          <div className="mt-1 truncate font-mono text-[11px] text-outline">
            {peer.backend || "agent"} · {peer.metadata?.branch ? String(peer.metadata.branch) : peer.circle}
            {peer.path ? <> · {parent}<span className="text-on-surface-variant">{folder}</span></> : null}
          </div>
        </div>
        <StatusLabel status={peer.status} />
        <CopyPeerName peer={peer} />
        <button
          onClick={onClose}
          aria-label="Close peer"
          className="flex h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface"
        >
          <X className="h-4 w-4" />
        </button>
      </div>

      {peer.description && (
        <div className="border-b border-border-faint px-4 py-2 font-mono text-xs text-outline md:px-6">
          <span className="text-primary/70">&gt;</span> {peer.description}
        </div>
      )}

      <div ref={scrollerRef} className="min-h-0 flex-1 overflow-y-auto px-4 py-4 md:px-6">
        {thread.length === 0 ? (
          <div className="py-10 font-mono text-xs leading-6 text-outline">
            &gt; no messages with {peerLabel(peer)}.<br />
            <span>send one to begin a query.</span>
          </div>
        ) : (
          thread.map((event) => <ThreadItem key={event.id} event={event} peer={peer} />)
        )}
      </div>

      <ComposeBar peer={peer} apiBase={apiBase} onSent={onSent} />
    </>
  );
}

function ThreadItem({ event, peer }: { event: Event; peer: Peer }) {
  if (event.type === "chat_turn") {
    const isUser = event.role === "user";
    return (
      <div className={cn("mb-4 flex flex-col", isUser ? "items-end" : "items-start")}>
        <div className="mb-1 font-mono text-[10px] font-medium uppercase tracking-[0.14em] text-outline">
          {isUser ? "@dashboard" : peerLabel(peer)} · {formatTime(event.timestamp)}
        </div>
        <div
          className={cn(
            "max-w-[82%] rounded p-3 font-mono text-[13px] leading-6 text-on-surface",
            isUser
              ? "border-r-2 border-primary bg-primary/10"
              : "border-l-2 border-primary/70 bg-surface-container-high"
          )}
        >
          {isUser ? (
            <p className="whitespace-pre-wrap">{event.text}</p>
          ) : (
            <div className="prose prose-invert prose-sm max-w-none">
              <ReactMarkdown remarkPlugins={[remarkGfm]}>{event.text}</ReactMarkdown>
            </div>
          )}
        </div>
        {!isUser && event.tool_calls && event.tool_calls.length > 0 && (
          <ToolCallBlock toolCalls={event.tool_calls} />
        )}
      </div>
    );
  }

  const label =
    event.type === "query"
      ? `query ${event.from} -> ${event.to}`
      : event.type === "response"
      ? `response ${event.from} -> ${event.to}`
      : event.type === "notification"
      ? `notify ${event.from} -> ${event.to}`
      : `broadcast from ${event.from}`;

  return (
    <div className="mb-2 flex items-start gap-2 font-mono text-xs text-outline">
      <span className="shrink-0 tabular-nums">{formatTime(event.timestamp)}</span>
      <span className="text-on-surface-variant">{label}</span>
      <span className="truncate">{event.text}</span>
    </div>
  );
}

function ToolCallBlock({ toolCalls }: { toolCalls: { name: string; input: string }[] }) {
  const [expanded, setExpanded] = useState(false);
  return (
    <div className="mt-2 w-full max-w-[82%]">
      <button
        onClick={() => setExpanded((value) => !value)}
        className="border border-border-faint bg-surface-container-low px-2.5 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.14em] text-primary"
      >
        {toolCalls.length} tool call{toolCalls.length === 1 ? "" : "s"}
      </button>
      {expanded && (
        <div className="mt-1 space-y-2 border border-border-faint bg-surface-dim p-3 font-mono text-xs">
          {toolCalls.map((toolCall, index) => (
            <div key={`${toolCall.name}-${index}`}>
              <div><span className="text-secondary">invoke</span> <span className="text-primary-fixed">{toolCall.name}</span></div>
              <div className="truncate text-outline">{toolCall.input}</div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function ComposeBar({ peer, apiBase, onSent }: { peer: Peer; apiBase: string; onSent?: () => void }) {
  const [text, setText] = useState("");
  const [mode, setMode] = useState<ComposeMode>("notify");
  const [isPending, setIsPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [response, setResponse] = useState<string | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 120)}px`;
  }, [text]);

  const uploadFile = async (upload: File): Promise<string | null> => {
    const formData = new FormData();
    formData.append("file", upload);
    try {
      const res = await fetch(`${apiBase}/attachments`, {
        method: "POST",
        body: formData,
        credentials: "include",
      });
      if (!res.ok) return null;
      const data = await res.json();
      return data.path as string;
    } catch {
      return null;
    }
  };

  const submit = async () => {
    if ((!text.trim() && !file) || isPending) return;
    setError(null);
    setResponse(null);
    setIsPending(true);

    try {
      let msg = text.trim();
      const hint = "\n(from @dashboard - reply naturally, dashboard sees your response automatically)";
      if (file) {
        const path = await uploadFile(file);
        if (!path) {
          setError("Failed to upload file");
          return;
        }
        msg = msg ? `${msg}\n[Attachment: ${path}]` : `[Attachment: ${path}]`;
      }

      const endpoint = mode === "notify" ? "notify" : "query";
      const res = await fetch(`${apiBase}/${endpoint}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          from_peer: "dashboard",
          to_peer: peer.name,
          text: msg + hint,
          bypass_circle: true,
        }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || data.error) {
        setError(data.detail || data.error || `Error ${res.status}`);
        return;
      }

      if (mode === "ask") setResponse(data.text ?? null);
      setText("");
      setFile(null);
      if (onSent) setTimeout(onSent, 1000);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed");
    } finally {
      setIsPending(false);
    }
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      submit();
    }
  };

  return (
    <div className="border-t border-border-faint bg-surface-dim p-3 md:p-4">
      <div className="mb-2 flex items-center gap-2">
        <div className="inline-flex border border-border-faint bg-surface-container-lowest p-1">
          {(["notify", "ask"] as const).map((item) => (
            <button
              key={item}
              onClick={() => setMode(item)}
              className={cn(
                "px-3 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.12em] transition-colors",
                mode === item ? "bg-primary text-on-primary" : "text-outline hover:text-on-surface"
              )}
            >
              {item === "notify" ? "Notify" : "Query"}
            </button>
          ))}
        </div>
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-outline">to {peerLabel(peer)}</span>
      </div>

      {file && (
        <div className="mb-2 flex items-center gap-2 border border-border-faint bg-surface-container-lowest px-2 py-1.5 font-mono text-xs text-on-surface-variant">
          <Paperclip className="h-3.5 w-3.5" />
          <span className="min-w-0 flex-1 truncate">{file.name}</span>
          <span className="text-outline">{(file.size / 1024).toFixed(0)}KB</span>
          <button onClick={() => setFile(null)} aria-label="Remove attachment" className="text-outline hover:text-on-surface">
            <X className="h-3.5 w-3.5" />
          </button>
        </div>
      )}

      <div className="flex items-end gap-3">
        <button
          onClick={() => fileRef.current?.click()}
          aria-label="Attach file"
          className="flex h-10 w-10 shrink-0 items-center justify-center rounded text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface"
        >
          <Paperclip className="h-4 w-4" />
        </button>
        <input
          ref={fileRef}
          type="file"
          accept="image/*,.pdf,.txt,.json,.csv,.md"
          className="hidden"
          onChange={(event) => {
            if (event.target.files?.[0]) setFile(event.target.files[0]);
            event.target.value = "";
          }}
        />
        <textarea
          ref={textareaRef}
          value={text}
          onChange={(event) => setText(event.target.value)}
          onKeyDown={onKeyDown}
          placeholder={`ask ${peerLabel(peer)} something...`}
          rows={1}
          className="max-h-32 min-h-10 flex-1 resize-none rounded border border-border-faint bg-surface-container-lowest px-3 py-2.5 font-mono text-sm text-on-surface outline-none placeholder:text-outline focus:border-primary focus:ring-1 focus:ring-primary"
        />
        <button
          onClick={submit}
          disabled={(!text.trim() && !file) || isPending}
          aria-label={mode === "notify" ? "Send message" : "Ask peer"}
          className={cn(
            "flex h-10 w-10 shrink-0 items-center justify-center rounded transition-[filter,transform] active:scale-[0.98]",
            text.trim() || file ? "bg-primary text-on-primary hover:brightness-110" : "bg-surface-container-high text-outline"
          )}
        >
          {isPending ? <RefreshCw className="h-4 w-4 animate-spin" /> : <Send className="h-4 w-4" />}
        </button>
      </div>

      {error && (
        <div className="mt-2 flex items-center gap-2 px-1 font-mono text-xs text-error">
          <AlertCircle className="h-3.5 w-3.5 shrink-0" />
          <span className="flex-1">{error}</span>
          <button onClick={submit} className="border border-error/30 px-2 py-0.5 text-[10px] uppercase">Retry</button>
        </div>
      )}
      {response && (
        <div className="mt-2 max-h-24 overflow-y-auto border border-border-faint bg-surface-container-lowest p-2 font-mono text-xs whitespace-pre-wrap text-on-surface-variant">
          {response}
        </div>
      )}
    </div>
  );
}

function SpawnDialog({ apiBase, onClose, onSpawned }: { apiBase: string; onClose: () => void; onSpawned: () => void }) {
  const [config, setConfig] = useState<SpawnConfig | null>(null);
  const [loading, setLoading] = useState(true);
  const [path, setPath] = useState("");
  const [command, setCommand] = useState("");
  const [circle, setCircle] = useState("default");
  const [error, setError] = useState<string | null>(null);
  const [spawning, setSpawning] = useState(false);

  useEffect(() => {
    fetch(`${apiBase}/spawn/config`)
      .then((res) => {
        if (!res.ok) throw new Error(`${res.status}`);
        return res.json();
      })
      .then((data: SpawnConfig) => {
        setConfig(data);
        if (data.allowed_commands.length > 0) setCommand(data.allowed_commands[0]);
        setLoading(false);
      })
      .catch(() => {
        setConfig({ enabled: false, allowed_commands: [], allowed_paths: [] });
        setLoading(false);
      });
  }, [apiBase]);

  const handleSpawn = async () => {
    if (!path.trim() || !command || spawning) return;
    setError(null);
    setSpawning(true);
    try {
      const res = await fetch(`${apiBase}/spawn`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: path.trim(), command, circle }),
      });
      const data = await res.json();
      if (!res.ok) setError(data.detail || `Error ${res.status}`);
      else {
        onSpawned();
        onClose();
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Spawn failed");
    } finally {
      setSpawning(false);
    }
  };

  return (
    <Modal title="Spawn new peer" onClose={onClose}>
      {loading ? (
        <div className="flex items-center justify-center py-8 font-mono text-sm text-outline">
          <RefreshCw className="mr-2 h-4 w-4 animate-spin" /> Loading config...
        </div>
      ) : config && !config.enabled ? (
        <div className="space-y-2 py-4 text-sm text-outline">
          <p className="text-on-surface-variant">Spawn is disabled.</p>
          <p className="font-mono text-xs">Set daemon.spawn.allowed_commands and daemon.spawn.allowed_paths in ~/.repowire/config.yaml</p>
        </div>
      ) : (
        <div className="space-y-4">
          <Field label="Project path">
            <input value={path} onChange={(event) => setPath(event.target.value)} placeholder="~/git/my-project" className={inputClass} />
          </Field>
          <Field label="Command">
            <select value={command} onChange={(event) => setCommand(event.target.value)} className={inputClass}>
              {config?.allowed_commands.map((cmd) => <option key={cmd} value={cmd}>{cmd}</option>)}
            </select>
          </Field>
          <Field label="Circle">
            <input value={circle} onChange={(event) => setCircle(event.target.value)} placeholder="default" className={inputClass} />
          </Field>
          {config && config.allowed_paths.length > 0 && (
            <p className="font-mono text-[10px] text-outline">Allowed: {config.allowed_paths.join(", ")}</p>
          )}
        </div>
      )}
      {error && <p className="mt-3 font-mono text-xs text-error">{error}</p>}
      {config?.enabled && (
        <div className="mt-5 flex justify-end">
          <button
            onClick={handleSpawn}
            disabled={!path.trim() || !command || spawning}
            className="inline-flex items-center gap-2 rounded bg-primary px-4 py-2 font-mono text-xs font-bold uppercase tracking-[0.12em] text-on-primary transition-[filter,transform] hover:brightness-110 active:scale-[0.98] disabled:opacity-40"
          >
            {spawning ? <RefreshCw className="h-4 w-4 animate-spin" /> : <Play className="h-4 w-4" />}
            Spawn
          </button>
        </div>
      )}
    </Modal>
  );
}

function SettingsDialog({ apiBase, isConnected, peers, onClose }: { apiBase: string; isConnected: boolean; peers: Peer[]; onClose: () => void }) {
  const [relayEnabled, setRelayEnabled] = useState(false);
  const host = apiBase.replace(/^https?:\/\//, "");
  const servicePeers = peers.filter((peer) => peer.role === "service");

  return (
    <Modal title="Configuration" onClose={onClose} wide>
      <div className="space-y-5">
        <section className="border border-border-faint bg-surface-container-low p-4">
          <div className="mb-4 flex items-start justify-between gap-4">
            <div>
              <p className="mb-1 font-mono text-[10px] uppercase tracking-[0.18em] text-outline">Service identity</p>
              <h3 className="font-headline text-lg font-bold text-on-surface">Daemon status</h3>
            </div>
            <div className={cn("flex items-center gap-2 border px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.12em]", isConnected ? "border-secondary/25 bg-secondary/10 text-secondary" : "border-error/25 bg-error/10 text-error")}>
              <span className={cn("h-2 w-2 rounded-full", isConnected ? "bg-secondary pulse-online" : "bg-error")} />
              {isConnected ? "Running" : "Disconnected"}
            </div>
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <Metric label="Host address" value={host} />
            <Metric label="Status" value={isConnected ? "Active" : "Unreachable"} />
          </div>
        </section>

        <section className="border border-border-faint bg-surface-container-low p-4">
          <div className="flex items-center justify-between gap-4">
            <div>
              <h3 className="font-headline text-sm font-semibold text-on-surface">Relay enabled</h3>
              <p className="text-xs text-outline">Tunnel local nodes to the hosted relay.</p>
            </div>
            <button
              role="switch"
              aria-checked={relayEnabled}
              onClick={() => setRelayEnabled((value) => !value)}
              className={cn("relative h-6 w-11 rounded-full transition-colors", relayEnabled ? "bg-primary" : "bg-surface-container-highest")}
            >
              <span className={cn("absolute top-1 h-4 w-4 rounded-full bg-on-surface transition-transform", relayEnabled ? "translate-x-6" : "translate-x-1")} />
            </button>
          </div>
          <Field label="API key">
            <input className={inputClass} type="password" placeholder="rw_..." readOnly />
          </Field>
        </section>

        <section>
          <h3 className="mb-3 font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-outline">External integrations</h3>
          <div className="grid gap-3 sm:grid-cols-2">
            {servicePeers.length > 0 ? servicePeers.map((peer) => (
              <div key={peer.peer_id} className="border border-border-faint border-t-2 border-t-primary bg-surface-container-low p-4">
                <div className="mb-3 flex items-center justify-between">
                  <Bot className="h-5 w-5 text-on-surface-variant" />
                  <StatusLabel status={peer.status} />
                </div>
                <p className="font-headline text-sm font-bold text-on-surface">{peerLabel(peer)}</p>
                <p className="font-mono text-[10px] text-outline">{peer.backend || "service"} · {peer.circle}</p>
              </div>
            )) : (
              <div className="border border-border-faint bg-surface-container-low p-4 text-sm text-outline">No service-role peers connected</div>
            )}
          </div>
        </section>
      </div>
    </Modal>
  );
}

const inputClass = "w-full rounded border border-border-faint bg-surface-container-lowest px-3 py-2 font-mono text-sm text-on-surface outline-none placeholder:text-outline focus:border-primary focus:ring-1 focus:ring-primary";

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="mt-3 block space-y-1.5">
      <span className="font-mono text-[10px] font-semibold uppercase tracking-[0.18em] text-outline">{label}</span>
      {children}
    </label>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="border-l-2 border-primary/60 bg-surface-container-lowest p-3">
      <p className="mb-1 font-mono text-[10px] uppercase tracking-[0.14em] text-outline">{label}</p>
      <p className="font-mono text-sm text-primary-fixed">{value}</p>
    </div>
  );
}

function Modal({ title, onClose, children, wide }: { title: string; onClose: () => void; children: ReactNode; wide?: boolean }) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4" onClick={onClose}>
      <div
        className={cn("max-h-[90vh] w-full overflow-y-auto border border-border bg-surface-container-low shadow-[var(--shadow-3)]", wide ? "max-w-2xl" : "max-w-md")}
        onClick={(event) => event.stopPropagation()}
      >
        <div className="flex items-center justify-between border-b border-border-faint px-5 py-4">
          <h2 className="font-mono text-xs font-bold uppercase tracking-[0.2em] text-primary">{title}</h2>
          <button onClick={onClose} className="text-outline transition-colors hover:text-on-surface" aria-label="Close">
            <X className="h-4 w-4" />
          </button>
        </div>
        <div className="p-5">{children}</div>
      </div>
    </div>
  );
}

function MobileTabs({
  activeTab,
  counts,
  eventCount,
  onChange,
}: {
  activeTab: MobileTab;
  counts: Record<"online" | "busy" | "offline", number>;
  eventCount: number;
  onChange: (tab: MobileTab) => void;
}) {
  return (
    <nav className="col-span-full flex border-t border-border-faint bg-surface-dim md:hidden">
      <MobileTabButton active={activeTab === "peers"} label="PEERS" sub={`${counts.online} online · ${counts.busy} busy`} onClick={() => onChange("peers")} />
      <MobileTabButton active={activeTab === "mesh"} label="MESH" sub={`${eventCount} events`} onClick={() => onChange("mesh")} />
    </nav>
  );
}

function MobileTabButton({ active, label, sub, onClick }: { active: boolean; label: string; sub: string; onClick: () => void }) {
  return (
    <button onClick={onClick} className={cn("flex flex-1 flex-col items-center gap-1 border-t-2 px-3 py-2", active ? "border-primary text-primary-fixed" : "border-transparent text-outline")}>
      <span className="font-mono text-[11px] font-semibold tracking-[0.18em]">{label}</span>
      <span className="font-mono text-[10px]">{sub}</span>
    </button>
  );
}

function WireTrace({ active }: { active: boolean }) {
  return (
    <div className="pointer-events-none absolute -left-[1px] top-0 hidden h-full w-[1px] bg-primary/45 md:block">
      {active && <div className="absolute left-[-2px] top-0 h-1.5 w-1.5 animate-pulse rounded-full bg-primary shadow-[var(--glow-copper)]" />}
    </div>
  );
}

function CopyPeerName({ peer }: { peer: Peer }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      onClick={() => {
        navigator.clipboard.writeText(peer.name);
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      }}
      title="Copy peer name"
      className="hidden h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface sm:inline-flex"
    >
      {copied ? <Check className="h-3.5 w-3.5 text-secondary" /> : <Copy className="h-3.5 w-3.5" />}
    </button>
  );
}

function StatusLabel({ status }: { status: Peer["status"] }) {
  const text = status === "online" ? "text-secondary" : status === "busy" ? "text-tertiary-fixed-dim" : "text-outline";
  return <span className={cn("font-mono text-[9px] font-semibold uppercase tracking-[0.16em]", text)}>{status}</span>;
}

function statusRank(status: Peer["status"]) {
  if (status === "online") return 0;
  if (status === "busy") return 1;
  return 2;
}

function formatTime(timestamp: string) {
  return new Date(timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}
