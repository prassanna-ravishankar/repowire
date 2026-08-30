import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { AlertCircle, Bell, Check, Clock, Copy, Paperclip, Play, RefreshCw, RotateCcw, Search, Send, Trash2, X } from "lucide-react";
import { cn, shortPath, statusDot } from "../lib/utils";
import { registerSnapshotProvider, useFrozenThread, useIsPeerProtected } from "../lib/protection";
import { clearDraft, setDraftFile, setDraftText, useDraftFile, useDraftText } from "../lib/drafts";
import { clearHistory, getHistory, pushHistory } from "../lib/history";
import {
  type Template,
  deleteTemplate,
  listTemplates,
  saveTemplate,
} from "../lib/templates";
import type { AttachmentRef, Event, Peer } from "../types";
import { peerLabel } from "../types";
import { formatTime, StatusLabel } from "./status";

interface TranscriptTurn {
  role: "user" | "assistant";
  text: string;
  timestamp: string;
  session_id: string;
  turn_id: string;
  tool_calls: { name: string; input: string }[];
}

interface PendingAsk {
  correlation_id: string;
  to_peer_id: string;
  preview: string;
  sent_at: number;
  state: "pending" | "delivered" | "timed_out";
  reply?: string;
  reply_from?: string;
  attachments?: AttachmentRef[];
}

interface SessionResumeControlResponse {
  ok: boolean;
  repowire_session_id: string;
  session_status: string;
  status: "active_executor" | "resume_available" | "unsupported" | "binding_unavailable";
  capability: "active_executor" | "supported" | "unsupported" | "unavailable";
  message: string;
  backend: string;
  runtime_session_id?: string | null;
  runtime_source_uri?: string | null;
  executor_peer_id?: string | null;
  executor_peer_name?: string | null;
  resume_capability: Record<string, unknown>;
  action?: "inspect" | "spawned";
  spawned_display_name?: string | null;
  tmux_session?: string | null;
  pane_id?: string | null;
}

interface SessionNotifyControlResponse {
  ok: boolean;
  delivery_state: "delivered" | "queued";
  reason: string;
}

interface TimelineSearchResult {
  cursor: string;
  target_id: string;
  item: {
    id: string;
    kind: "turn" | "delta_group";
    source: "history" | "realtime";
    timestamp: string;
    session_id: string;
    turn_id: string;
    role: "user" | "assistant";
    text: string;
    tool_calls: { name: string; input: string }[];
  };
  match: {
    start: number;
    end: number;
    snippet: string;
  };
}

interface TimelineSearchResponse {
  degraded: boolean;
  degradation_message: string;
  repowire_session_id?: string | null;
  results: TimelineSearchResult[];
}

interface TranscriptResponse {
  turns: TranscriptTurn[];
  next_before: string | null;
  repowire_session_id?: string | null;
}

const ACK_FRAME_RE = /^\[ack #([^\]\s]+) from @([^\]\s]+)\]\s?([\s\S]*)$/;
const BARE_ACK_TIMEOUT_MS = 120_000;

type PeerTab = "chat" | "mcp" | "history";

/** Pending-turn group built from chat_turn_delta events. Synthetic client-side
 * shape — not a wire event — carrying a discriminator so the thread renderer
 * can branch without runtime probing. Declared up here (rather than next to
 * the renderer) because session-protection generics need it at component
 * scope. */
interface ChatTurnDeltaGroup {
  type: "chat_turn_delta_group";
  id: string;
  session_id?: string;
  turn_id: string;
  peer_id?: string;
  timestamp: string;
  text: string;
  tool_calls: { name: string; input: string }[];
}

interface HistoryTimelineTurn extends TranscriptTurn {
  type: "history_turn";
  id: string;
}

type ThreadItemEntry = Event | ChatTurnDeltaGroup | HistoryTimelineTurn;

export function PeerView({
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
  const bottomRef = useRef<HTMLDivElement>(null);
  const [activeTab, setActiveTab] = useState<PeerTab>("chat");
  const metadataSession = typeof peer.metadata?.hook_session_id === "string"
    ? peer.metadata.hook_session_id
    : null;
  const [activeSessionState, setActiveSessionState] = useState<{ peerId: string; sessionId: string | null }>({
    peerId: peer.peer_id,
    sessionId: metadataSession,
  });
  const [activeRepowireSessionId, setActiveRepowireSessionId] = useState<string | null>(null);
  const activeSessionId = activeSessionState.peerId === peer.peer_id
    ? activeSessionState.sessionId
    : metadataSession;
  const activeSessionRef = useRef(activeSessionState);
  const [timelineTurns, setTimelineTurns] = useState<TranscriptTurn[]>([]);
  const [timelineNextBefore, setTimelineNextBefore] = useState<string | null>(null);
  const [timelineLoading, setTimelineLoading] = useState(false);
  const [timelineError, setTimelineError] = useState<string | null>(null);
  const [timelineInitialized, setTimelineInitialized] = useState(false);
  const [searchQuery, setSearchQuery] = useState("");
  const [searchResults, setSearchResults] = useState<TimelineSearchResult[]>([]);
  const [searchLoading, setSearchLoading] = useState(false);
  const [searchError, setSearchError] = useState<string | null>(null);
  const [searchDegradation, setSearchDegradation] = useState<string | null>(null);
  const [unregistering, setUnregistering] = useState(false);
  const [unregisterError, setUnregisterError] = useState<string | null>(null);
  const [highlightTarget, setHighlightTarget] = useState<string | null>(null);
  const [pinnedSearchTurn, setPinnedSearchTurn] = useState<TranscriptTurn | null>(null);
  const timelineTopSentinelRef = useRef<HTMLDivElement>(null);
  const timelineItemRefs = useRef(new Map<string, HTMLDivElement>());
  const lastMetadataSessionRef = useRef<string | null>(metadataSession);
  const protectedNow = useIsPeerProtected(peer.peer_id);
  useEffect(() => {
    activeSessionRef.current = activeSessionState;
  }, [activeSessionState]);
  const realtimeSessionIds = useMemo(() => {
    const ids: string[] = [];
    for (const event of events) {
      if (
        (event.type === "chat_turn" || event.type === "chat_turn_delta") &&
        event.peer_id === peer.peer_id &&
        event.session_id
      ) {
        ids.push(event.session_id);
      }
    }
    return ids;
  }, [events, peer.peer_id]);
  const liveThread = useMemo(() => {
    const id = peer.peer_id;
    const scopedEvents = events
      .filter((event) => {
        if (event.type === "chat_turn" || event.type === "chat_turn_delta") {
          if (event.peer_id !== id) return false;
          if (activeSessionId) return event.session_id === activeSessionId;
          return !event.session_id;
        }
        return event.from_peer_id === id || event.to_peer_id === id;
      })
      .sort((a, b) => a.timestamp.localeCompare(b.timestamp));
    const turns = pinnedSearchTurn ? [...timelineTurns, pinnedSearchTurn] : timelineTurns;
    return mergeTimeline(turns, scopedEvents, activeSessionId);
  }, [activeSessionId, events, peer.peer_id, pinnedSearchTurn, timelineTurns]);

  const runSearch = useCallback(async () => {
    const q = searchQuery.trim();
    if (!q || searchLoading) return;
    setSearchLoading(true);
    setSearchError(null);
    setSearchDegradation(null);
    try {
      const url = new URL(
        `${apiBase}/peers/${encodeURIComponent(peer.name)}/timeline/search`,
        window.location.origin,
      );
      url.searchParams.set("q", q);
      url.searchParams.set("limit", "20");
      const res = await fetch(url.toString().replace(window.location.origin, ""), {
        credentials: "include",
      });
      if (!res.ok) {
        setSearchError(`Error ${res.status}`);
        return;
      }
      const data = (await res.json()) as TimelineSearchResponse;
      setSearchResults(data.results);
      setSearchDegradation(data.degraded ? data.degradation_message : null);
      setActiveRepowireSessionId(data.repowire_session_id || null);
    } catch (e) {
      setSearchError(e instanceof Error ? e.message : "Request failed");
    } finally {
      setSearchLoading(false);
    }
  }, [apiBase, peer.name, searchLoading, searchQuery]);

  const jumpToSearchResult = useCallback((result: TimelineSearchResult) => {
    const targetKey = timelineKey(result.item.session_id, result.item.turn_id);
    if (!targetKey || result.item.kind !== "turn") return;
    setActiveTab("chat");
    setPinnedSearchTurn({
      role: result.item.role,
      text: result.item.text,
      timestamp: result.item.timestamp,
      session_id: result.item.session_id,
      turn_id: result.item.turn_id,
      tool_calls: result.item.tool_calls,
    });
    setHighlightTarget(targetKey);
    if (activeSessionRef.current.sessionId !== result.item.session_id) {
      setActiveSessionState({ peerId: peer.peer_id, sessionId: result.item.session_id });
    }
  }, [peer.peer_id]);

  const fetchTimelinePage = useCallback(
    async (before: string | null, sessionId: string | null) => {
      setTimelineLoading(true);
      setTimelineError(null);
      try {
        const url = new URL(
          `${apiBase}/peers/${encodeURIComponent(peer.name)}/transcript`,
          window.location.origin,
        );
        url.searchParams.set("limit", sessionId ? "50" : "1");
        if (sessionId) url.searchParams.set("session_id", sessionId);
        if (before) url.searchParams.set("before", before);
        const res = await fetch(url.toString().replace(window.location.origin, ""), {
          credentials: "include",
        });
        if (!res.ok) {
          setTimelineError(`Error ${res.status}`);
          return;
        }
        const data = (await res.json()) as TranscriptResponse;
        if (!sessionId) {
          const currentSelection = activeSessionRef.current;
          if (
            data.turns[0]?.session_id &&
            currentSelection.peerId === peer.peer_id &&
            currentSelection.sessionId === null &&
            !lastMetadataSessionRef.current
          ) {
            setActiveSessionState({ peerId: peer.peer_id, sessionId: data.turns[0].session_id });
            setActiveRepowireSessionId(data.repowire_session_id || null);
          }
          return;
        }
        const currentSelection = activeSessionRef.current;
        if (currentSelection.peerId === peer.peer_id && currentSelection.sessionId === sessionId) {
          setActiveRepowireSessionId(data.repowire_session_id || null);
        }
        setTimelineTurns((prev) => {
          const seen = new Set(prev.map((turn) => `${turn.session_id}:${turn.turn_id}`));
          const next = [...prev];
          for (const turn of data.turns) {
            const key = `${turn.session_id}:${turn.turn_id}`;
            if (!seen.has(key)) {
              seen.add(key);
              next.push(turn);
            }
          }
          return next;
        });
        setTimelineNextBefore(data.next_before);
      } catch (e) {
        setTimelineError(e instanceof Error ? e.message : "Request failed");
      } finally {
        setTimelineLoading(false);
        setTimelineInitialized(true);
      }
    },
    [apiBase, peer.name, peer.peer_id],
  );

  useEffect(() => {
    lastMetadataSessionRef.current = metadataSession;
    setActiveSessionState({ peerId: peer.peer_id, sessionId: metadataSession });
    setTimelineTurns([]);
    setPinnedSearchTurn(null);
    setTimelineNextBefore(null);
    setTimelineError(null);
    setTimelineInitialized(false);
    setActiveRepowireSessionId(null);
    setSearchQuery("");
    setSearchResults([]);
    setSearchError(null);
    setSearchDegradation(null);
    setHighlightTarget(null);
    // Sticky selection resets only on peer changes; metadata changes are handled below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [peer.peer_id]);

  useEffect(() => {
    if (!metadataSession || metadataSession === lastMetadataSessionRef.current) return;
    lastMetadataSessionRef.current = metadataSession;
    setActiveSessionState({ peerId: peer.peer_id, sessionId: metadataSession });
    setTimelineTurns([]);
    setPinnedSearchTurn(null);
    setTimelineNextBefore(null);
    setTimelineError(null);
    setTimelineInitialized(false);
    setActiveRepowireSessionId(null);
  }, [metadataSession, peer.peer_id]);

  useEffect(() => {
    if (activeSessionId || realtimeSessionIds.length === 0) return;
    setActiveSessionState({
      peerId: peer.peer_id,
      sessionId: realtimeSessionIds[realtimeSessionIds.length - 1],
    });
  }, [activeSessionId, peer.peer_id, realtimeSessionIds]);

  useEffect(() => {
    setTimelineTurns([]);
    setTimelineNextBefore(null);
    setTimelineError(null);
    setTimelineInitialized(false);
    setActiveRepowireSessionId(null);
    void fetchTimelinePage(null, activeSessionId);
  }, [activeSessionId, fetchTimelinePage]);

  useEffect(() => {
    const el = timelineTopSentinelRef.current;
    if (!el || !activeSessionId || !timelineNextBefore || timelineLoading) return;
    const obs = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting) void fetchTimelinePage(timelineNextBefore, activeSessionId);
      },
      { rootMargin: "100px" },
    );
    obs.observe(el);
    return () => obs.disconnect();
  }, [activeSessionId, fetchTimelinePage, timelineLoading, timelineNextBefore]);

  // While the peer is protected (e.g. unsubmitted compose draft), freeze the
  // rendered thread so new SSE events don't reorder/clobber it mid-compose.
  // The snapshot lives in the protection store keyed by peer_id; markProtected
  // captures it via a provider closure that we register here, so capture
  // happens on the synchronous input/store-update path (setDraftText), never
  // from this component's render. The snapshot survives peer switches and is
  // released by the store when the last protection source for this peer
  // clears.
  const frozenFromStore = useFrozenThread<ThreadItemEntry>(peer.peer_id);
  const liveThreadRef = useRef(liveThread);
  // Layout-effect timing: run before browser paint so that by the time the
  // user can interact (or any post-commit setDraftText fires), the ref and
  // provider already reflect the just-committed thread. A passive useEffect
  // here leaves a window where markProtected could capture a stale thread.
  useLayoutEffect(() => {
    liveThreadRef.current = liveThread;
  }, [liveThread]);
  useLayoutEffect(() => {
    // Register a provider closure (over the ref) so markProtected — called
    // from the synchronous setDraftText path — can capture the latest
    // liveThread without us writing to the store from this component's
    // render. Layout-effect ensures the provider is registered before paint
    // and therefore before any post-commit user input.
    return registerSnapshotProvider<ThreadItemEntry>(peer.peer_id, () => liveThreadRef.current);
  }, [peer.peer_id]);
  const thread = protectedNow && frozenFromStore ? frozenFromStore : liveThread;

  useEffect(() => {
    if (protectedNow) return;
    bottomRef.current?.scrollIntoView({ block: "end" });
  }, [thread.length, protectedNow]);

  useEffect(() => {
    if (!highlightTarget) return;
    const timeout = window.setTimeout(() => {
      const el = timelineItemRefs.current.get(highlightTarget);
      el?.scrollIntoView({ block: "center" });
    }, 0);
    return () => window.clearTimeout(timeout);
  }, [highlightTarget, thread]);

  // Reset to chat when switching peers so the tab choice doesn't leak across selections.
  useEffect(() => {
    setActiveTab("chat");
  }, [peer.peer_id]);

  const { folder, parent } = peer.path ? shortPath(peer.path) : { folder: "", parent: "" };

  const unregisterPeer = useCallback(async () => {
    if (!window.confirm(
      `Unregister @${peerLabel(peer)}? This retires the current peer identity, even if its agent process is still running.`,
    )) return;

    setUnregistering(true);
    setUnregisterError(null);
    try {
      const res = await fetch(`${apiBase}/peers/${encodeURIComponent(peer.peer_id)}`, {
        method: "DELETE",
        credentials: "include",
      });
      if (!res.ok) {
        const message = await res.text();
        throw new Error(message || `Error ${res.status}`);
      }
      onSent();
      onClose();
    } catch (error) {
      setUnregisterError(error instanceof Error ? error.message : "Failed to unregister peer");
    } finally {
      setUnregistering(false);
    }
  }, [apiBase, onClose, onSent, peer]);

  return (
    <>
      <div className="sticky top-[var(--topbar-offset)] z-10 flex items-center gap-3 border-b border-border-faint bg-surface-dim px-4 py-3 md:static md:px-6">
        <span className={cn("h-2.5 w-2.5 rounded-full", statusDot(peer.status))} />
        <div className="min-w-0 flex-1">
          <h1 className="truncate font-headline text-lg font-semibold text-on-surface">{peerLabel(peer)}</h1>
          <div className="mt-1 flex items-center gap-1.5 truncate font-mono text-[11px] text-outline">
            <span className="truncate">
              {[peer.backend || "agent", peer.model, peer.metadata?.branch ? String(peer.metadata.branch) : peer.circle].filter(Boolean).join(" · ")}
              {peer.path ? (
                <>
                  {" · "}
                  <PathCopyButton path={peer.path} parent={parent} folder={folder} />
                </>
              ) : null}
            </span>
            <GitStatusBadge status={peer.metadata?.git_status} />
          </div>
        </div>
        <StatusLabel status={peer.status} />
        <SwitchBackendControl peer={peer} apiBase={apiBase} />
        {peer.path ? <OpenInEditorButton path={peer.path} /> : null}
        <CopyPeerName peer={peer} />
        <button
          onClick={unregisterPeer}
          disabled={unregistering}
          aria-label="Unregister peer"
          title="Unregister peer"
          className="flex h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:border-error/50 hover:bg-error/10 hover:text-error disabled:cursor-wait disabled:opacity-50"
        >
          {unregistering ? <RefreshCw className="h-4 w-4 animate-spin" /> : <Trash2 className="h-4 w-4" />}
        </button>
        <button
          onClick={onClose}
          aria-label="Close peer view"
          className="flex h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface"
        >
          <X className="h-4 w-4" />
        </button>
      </div>

      {unregisterError && (
        <div role="alert" className="border-b border-error/25 bg-error/10 px-4 py-2 font-mono text-xs text-error md:px-6">
          {unregisterError}
        </div>
      )}

      {peer.description && (
        <div className="border-b border-border-faint px-4 py-2 font-mono text-xs text-outline md:px-6">
          <span className="text-primary/70">&gt;</span> {peer.description}
        </div>
      )}

      <div className="flex shrink-0 gap-0 border-b border-border-faint px-4 md:px-6" role="tablist">
        <TabButton label="chat" active={activeTab === "chat"} onClick={() => setActiveTab("chat")} />
        <TabButton label="history" active={activeTab === "history"} onClick={() => setActiveTab("history")} />
        <TabButton label="mcp" active={activeTab === "mcp"} onClick={() => setActiveTab("mcp")} />
      </div>

      {activeTab === "chat" ? (
        <>
          <div className="min-h-0 flex-1 px-4 py-4 md:overflow-y-auto md:px-6">
            {activeSessionId && timelineNextBefore && (
              <div ref={timelineTopSentinelRef} className="py-2 text-center font-mono text-[10px] text-outline">
                {timelineLoading ? "loading older..." : "scroll up for older"}
              </div>
            )}
            {timelineError && (
              <div className="mb-2 flex items-center gap-2 font-mono text-xs text-error">
                <AlertCircle className="h-3.5 w-3.5" />
                <span>{timelineError}</span>
              </div>
            )}
            <TimelineSearch
              query={searchQuery}
              results={searchResults}
              loading={searchLoading}
              error={searchError}
              degradation={searchDegradation}
              onQueryChange={setSearchQuery}
              onSearch={runSearch}
              onJump={jumpToSearchResult}
            />
            {thread.length === 0 ? (
              <div className="py-10 font-mono text-xs leading-6 text-outline">
                {timelineLoading && !timelineInitialized ? (
                  <>&gt; loading...</>
                ) : (
                  <>
                    &gt; no messages with {peerLabel(peer)}.<br />
                    <span>send one to begin a query.</span>
                  </>
                )}
              </div>
            ) : (
              thread.map((event) => {
                const targetKey = entryTargetKey(event);
                const isHighlighted = targetKey !== null && targetKey === highlightTarget;
                return (
                  <div
                    key={event.id}
                    ref={(node) => {
                      if (!targetKey) return;
                      if (node) timelineItemRefs.current.set(targetKey, node);
                      else timelineItemRefs.current.delete(targetKey);
                    }}
                    data-testid={targetKey ? `timeline-${targetKey}` : undefined}
                    className={cn(
                      "rounded-sm transition-shadow",
                      isHighlighted && "ring-2 ring-primary/70 ring-offset-2 ring-offset-surface-dim",
                    )}
                  >
                    {event.type === "chat_turn_delta_group" ? (
                      <StreamingTurnItem group={event} peer={peer} />
                    ) : event.type === "history_turn" ? (
                      <HistoryTurn turn={event} peer={peer} />
                    ) : (
                      <ThreadItem
                        event={event as Event}
                        peer={peer}
                        apiBase={apiBase}
                      />
                    )}
                  </div>
                );
              })
            )}
            <div ref={bottomRef} />
          </div>

          <SessionCommandPanel
            peer={peer}
            apiBase={apiBase}
            repowireSessionId={activeRepowireSessionId}
            onSent={onSent}
          />
          <ComposeBar peer={peer} apiBase={apiBase} events={events} onSent={onSent} />
        </>
      ) : activeTab === "history" ? (
        <HistoryPane peer={peer} apiBase={apiBase} />
      ) : (
        <McpPanel peer={peer} apiBase={apiBase} />
      )}
    </>
  );
}

function TimelineSearch({
  query,
  results,
  loading,
  error,
  degradation,
  onQueryChange,
  onSearch,
  onJump,
}: {
  query: string;
  results: TimelineSearchResult[];
  loading: boolean;
  error: string | null;
  degradation: string | null;
  onQueryChange: (value: string) => void;
  onSearch: () => void;
  onJump: (result: TimelineSearchResult) => void;
}) {
  return (
    <div className="mb-4 border border-border-faint bg-surface-container-lowest p-2.5">
      <form
        className="flex items-center gap-2"
        onSubmit={(event) => {
          event.preventDefault();
          onSearch();
        }}
      >
        <Search className="h-4 w-4 shrink-0 text-outline" aria-hidden="true" />
        <input
          value={query}
          onChange={(event) => onQueryChange(event.target.value)}
          placeholder="search conversation..."
          aria-label="Search conversation"
          className="min-w-0 flex-1 bg-transparent font-mono text-xs text-on-surface outline-none placeholder:text-outline"
        />
        <button
          type="submit"
          disabled={!query.trim() || loading}
          className="flex h-7 shrink-0 items-center rounded border border-border-faint px-2 font-mono text-[10px] uppercase tracking-[0.14em] text-outline hover:bg-surface-container-high hover:text-on-surface disabled:opacity-40"
        >
          {loading ? "searching" : "search"}
        </button>
      </form>
      {(error || degradation) && (
        <div className={cn("mt-2 flex items-start gap-2 font-mono text-[11px]", error ? "text-error" : "text-outline")}>
          <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
          <span>{error || degradation}</span>
        </div>
      )}
      {results.length > 0 && (
        <div className="mt-2 max-h-44 overflow-y-auto border-t border-border-faint pt-2">
          {results.map((result) => (
            <button
              key={`${result.cursor}-${result.item.source}-${result.item.id}`}
              type="button"
              onClick={() => onJump(result)}
              className="mb-1 block w-full rounded border border-transparent px-2 py-1.5 text-left font-mono text-xs hover:border-border-faint hover:bg-surface-container-high"
            >
              <span className="block text-[10px] uppercase tracking-[0.14em] text-outline">
                {result.item.session_id} / {result.item.role} / {formatTime(result.item.timestamp)}
              </span>
              <span className="mt-0.5 line-clamp-2 block text-on-surface-variant">
                {result.match.snippet}
              </span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function ThreadItem({
  event,
  peer,
  apiBase,
}: {
  event: Event;
  peer: Peer;
  apiBase: string;
}) {
  if (event.type === "chat_turn") {
    const isUser = event.role === "user";
    return (
      <div className={cn("mb-4 flex flex-col", isUser ? "items-end" : "items-start")}>
        <div className="mb-1 font-mono text-[10px] font-medium uppercase tracking-[0.14em] text-outline">
          {isUser ? "@dashboard" : peerLabel(peer)} · {formatTime(event.timestamp)}
        </div>
        <div
          className={cn(
            "max-w-[82%] min-w-0 rounded p-3 font-mono text-[13px] leading-6 text-on-surface [overflow-wrap:anywhere]",
            isUser
              ? "border-r-2 border-primary bg-primary/10"
              : "border-l-2 border-primary/70 bg-surface-container-high"
          )}
      >
        {isUser ? (
          <p className="whitespace-pre-wrap break-words">{event.text}</p>
          ) : (
            <div className="prose prose-sm max-w-none break-words [&_pre]:overflow-x-auto">
              <ReactMarkdown remarkPlugins={[remarkGfm]}>{event.text}</ReactMarkdown>
            </div>
          )}
          <AttachmentChips attachments={event.attachments} apiBase={apiBase} />
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
      : event.type === "ask"
      ? `ask ${event.from} -> ${event.to}`
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
      <AttachmentChips attachments={event.attachments} apiBase={apiBase} compact />
    </div>
  );
}

function AttachmentChips({
  attachments,
  apiBase,
  compact = false,
}: {
  attachments?: AttachmentRef[];
  apiBase: string;
  compact?: boolean;
}) {
  if (!attachments || attachments.length === 0) return null;
  return (
    <div className={cn("mt-2 flex flex-wrap gap-1.5", compact && "mt-0")}>
      {attachments.map((attachment, index) => {
        const label = attachment.filename || attachment.path?.split("/").pop() || attachment.id || "attachment";
        const href = attachment.id ? `${apiBase}/attachments/${encodeURIComponent(attachment.id)}` : undefined;
        const chip = (
          <span className="inline-flex max-w-64 items-center gap-1.5 truncate rounded border border-border-faint bg-surface-container-low px-2 py-1 font-mono text-[11px] text-on-surface-variant">
            <Paperclip className="h-3 w-3 shrink-0 text-outline" aria-hidden="true" />
            <span className="truncate">{label}</span>
            {attachment.size ? <span className="shrink-0 text-outline">{Math.ceil(attachment.size / 1024)}KB</span> : null}
          </span>
        );
        return href ? (
          <a key={`${attachment.id}-${index}`} href={href} className="hover:brightness-110">
            {chip}
          </a>
        ) : (
          <span key={`${label}-${index}`}>{chip}</span>
        );
      })}
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

function HistoryPane({ peer, apiBase }: { peer: Peer; apiBase: string }) {
  const [turns, setTurns] = useState<TranscriptTurn[]>([]);
  const [nextBefore, setNextBefore] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [initialized, setInitialized] = useState(false);
  const topSentinelRef = useRef<HTMLDivElement>(null);

  const fetchPage = async (before: string | null) => {
    setLoading(true);
    setError(null);
    try {
      const url = new URL(`${apiBase}/peers/${encodeURIComponent(peer.name)}/transcript`, window.location.origin);
      url.searchParams.set("limit", "50");
      if (before) url.searchParams.set("before", before);
      const res = await fetch(url.toString().replace(window.location.origin, ""), {
        credentials: "include",
      });
      if (!res.ok) {
        setError(`Error ${res.status}`);
        return;
      }
      const data = (await res.json()) as { turns: TranscriptTurn[]; next_before: string | null };
      setTurns((prev) => [...prev, ...data.turns]);
      setNextBefore(data.next_before);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed");
    } finally {
      setLoading(false);
      setInitialized(true);
    }
  };

  useEffect(() => {
    setTurns([]);
    setNextBefore(null);
    setInitialized(false);
    fetchPage(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [peer.peer_id]);

  useEffect(() => {
    const el = topSentinelRef.current;
    if (!el || !nextBefore || loading) return;
    const obs = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting) fetchPage(nextBefore);
      },
      { rootMargin: "100px" }
    );
    obs.observe(el);
    return () => obs.disconnect();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nextBefore, loading]);

  return (
    <div className="min-h-0 flex-1 px-4 py-4 md:overflow-y-auto md:px-6">
      {nextBefore && (
        <div ref={topSentinelRef} className="py-2 text-center font-mono text-[10px] text-outline">
          {loading ? "loading older…" : "scroll up for older"}
        </div>
      )}
      {turns.length === 0 && initialized && !loading && (
        <div className="py-10 font-mono text-xs leading-6 text-outline">
          &gt; no transcript history for {peerLabel(peer)}.<br />
          <span>{peer.backend === "claude-code" ? "this peer has no on-disk sessions yet." : "history is claude-code only in v1."}</span>
        </div>
      )}
      {turns.length === 0 && !initialized && loading && (
        <div className="py-10 font-mono text-xs leading-6 text-outline">&gt; loading…</div>
      )}
      {error && (
        <div className="mb-2 flex items-center gap-2 font-mono text-xs text-error">
          <AlertCircle className="h-3.5 w-3.5" />
          <span>{error}</span>
        </div>
      )}
      {[...turns].reverse().map((turn, idx) => (
        <HistoryTurn key={`${turn.session_id}-${turn.timestamp}-${idx}`} turn={turn} peer={peer} />
      ))}
    </div>
  );
}

function HistoryTurn({ turn, peer }: { turn: TranscriptTurn; peer: Peer }) {
  const isUser = turn.role === "user";
  return (
    <div className={cn("mb-4 flex flex-col", isUser ? "items-end" : "items-start")}>
      <div className="mb-1 font-mono text-[10px] font-medium uppercase tracking-[0.14em] text-outline">
        {isUser ? "@user" : peerLabel(peer)} · {turn.timestamp ? formatTime(turn.timestamp) : "—"}
      </div>
      <div
        className={cn(
          "max-w-[82%] min-w-0 rounded p-3 font-mono text-[13px] leading-6 text-on-surface [overflow-wrap:anywhere]",
          isUser
            ? "border-r-2 border-primary/40 bg-primary/5"
            : "border-l-2 border-primary/40 bg-surface-container-high"
        )}
      >
        {isUser ? (
          <p className="whitespace-pre-wrap break-words">{turn.text}</p>
        ) : (
          <div className="prose prose-sm max-w-none break-words [&_pre]:overflow-x-auto">
            <ReactMarkdown remarkPlugins={[remarkGfm]}>{turn.text}</ReactMarkdown>
          </div>
        )}
      </div>
      {!isUser && turn.tool_calls.length > 0 && <ToolCallBlock toolCalls={turn.tool_calls} />}
    </div>
  );
}

function SessionCommandPanel({
  peer,
  apiBase,
  repowireSessionId,
  onSent,
}: {
  peer: Peer;
  apiBase: string;
  repowireSessionId: string | null;
  onSent?: () => void;
}) {
  const [resumeState, setResumeState] = useState<SessionResumeControlResponse | null>(null);
  const [loadingCapability, setLoadingCapability] = useState(false);
  const [capabilityError, setCapabilityError] = useState<string | null>(null);
  const [notifyText, setNotifyText] = useState("");
  const [notifyPending, setNotifyPending] = useState(false);
  const [notifyStatus, setNotifyStatus] = useState<string | null>(null);
  const [notifyError, setNotifyError] = useState<string | null>(null);
  const [resumePending, setResumePending] = useState(false);
  const [resumeStatus, setResumeStatus] = useState<string | null>(null);
  const [resumeError, setResumeError] = useState<string | null>(null);

  const loadCapability = useCallback(async () => {
    if (!repowireSessionId) {
      setResumeState(null);
      setCapabilityError(null);
      setLoadingCapability(false);
      return;
    }
    setLoadingCapability(true);
    setCapabilityError(null);
    try {
      const res = await fetch(
        `${apiBase}/sessions/${encodeURIComponent(repowireSessionId)}/controls/resume`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          credentials: "include",
          body: JSON.stringify({ from_peer: "dashboard", dry_run: true }),
        },
      );
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        const detail = data?.detail;
        setResumeState(null);
        setCapabilityError(
          typeof detail === "string"
            ? detail
            : detail?.message || detail?.error || `Error ${res.status}`,
        );
        return;
      }
      setResumeState(data as SessionResumeControlResponse);
    } catch (e) {
      setResumeState(null);
      setCapabilityError(e instanceof Error ? e.message : "Request failed");
    } finally {
      setLoadingCapability(false);
    }
  }, [repowireSessionId, apiBase]);

  useEffect(() => {
    setNotifyText("");
    setNotifyStatus(null);
    setNotifyError(null);
    setResumeStatus(null);
    setResumeError(null);
    void loadCapability();
  }, [loadCapability]);

  if (!repowireSessionId) return null;

  const controlSessionId = resumeState?.repowire_session_id || repowireSessionId;
  const canNotifySession = Boolean(controlSessionId && resumeState?.capability === "active_executor");
  const canResumeSession = Boolean(controlSessionId && resumeState?.capability === "supported");
  const resumeLabel = loadingCapability
    ? "checking session..."
    : resumeState?.capability === "active_executor"
    ? "running agent"
    : resumeState?.capability === "supported"
    ? "resume available"
    : resumeState?.capability === "unavailable"
    ? "legacy session"
    : resumeState
    ? "not resumable"
    : "session unavailable";
  const controlMessage = resumeState?.capability === "active_executor"
    ? "This captured session has a running agent attached, so nudges can be sent now."
    : resumeState?.capability === "supported"
    ? "This captured session has resume metadata and can start a new backend-native resume."
    : resumeState?.capability === "unavailable"
    ? resumeState.message
    : resumeState?.capability === "unsupported"
    ? resumeState.message || "This session is not a resumable local agent session."
    : capabilityError === "Not Found" || capabilityError === "Error 404"
    ? "No session binding was found for the selected message."
    : capabilityError || "Checking whether this session has a running agent.";
  const templates = [
    { label: "Status update", text: "Please send a quick status update." },
    { label: "Checkpoint", text: "Checkpoint your current diff and next step." },
    { label: "Continue", text: "Continue with the active task and report blockers." },
  ];

  async function submitNotify() {
    const text = notifyText.trim();
    if (!controlSessionId || !text || notifyPending || !canNotifySession) return;
    setNotifyPending(true);
    setNotifyStatus(null);
    setNotifyError(null);
    try {
      const res = await fetch(
        `${apiBase}/sessions/${encodeURIComponent(controlSessionId)}/controls/notify`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          credentials: "include",
          body: JSON.stringify({
            from_peer: "dashboard",
            text,
            attachments: [],
            bypass_circle: true,
          }),
        },
      );
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        const detail = data?.detail;
        setNotifyError(
          typeof detail === "string"
            ? detail
            : detail?.message || detail?.error || `Error ${res.status}`,
        );
        return;
      }
      const body = data as SessionNotifyControlResponse;
      setNotifyText("");
      setNotifyStatus(body.delivery_state === "queued" ? "queued for active session" : "delivered to active session");
      onSent?.();
    } catch (e) {
      setNotifyError(e instanceof Error ? e.message : "Request failed");
    } finally {
      setNotifyPending(false);
    }
  }

  async function submitResume() {
    if (!controlSessionId || !canResumeSession || resumePending) return;
    setResumePending(true);
    setResumeStatus(null);
    setResumeError(null);
    try {
      const res = await fetch(
        `${apiBase}/sessions/${encodeURIComponent(controlSessionId)}/controls/resume`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          credentials: "include",
          body: JSON.stringify({ from_peer: "dashboard", dry_run: false }),
        },
      );
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        const detail = data?.detail;
        setResumeError(
          typeof detail === "string"
            ? detail
            : detail?.message || detail?.error || `Error ${res.status}`,
        );
        return;
      }
      const body = data as SessionResumeControlResponse;
      setResumeState(body);
      setResumeStatus(body.spawned_display_name ? `resume spawned: ${body.spawned_display_name}` : "resume spawned");
      onSent?.();
    } catch (e) {
      setResumeError(e instanceof Error ? e.message : "Request failed");
    } finally {
      setResumePending(false);
    }
  }

  return (
    <div className="border-t border-border-faint bg-surface px-3 py-2 md:px-4">
      <div className="rounded border border-border-faint bg-surface-container-low p-2.5">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-outline">
            session actions
          </span>
          <span
            className={cn(
              "inline-flex items-center rounded border px-2 py-0.5 font-mono text-[10px] uppercase tracking-[0.12em]",
              canNotifySession
                ? "border-primary/40 bg-primary/10 text-primary"
                : "border-border-faint text-outline",
            )}
          >
            {resumeLabel}
          </span>
          <button
            type="button"
            onClick={loadCapability}
            disabled={loadingCapability}
            title="Refresh session capability"
            aria-label="Refresh session capability"
            className="ml-auto flex h-7 w-7 items-center justify-center rounded text-outline hover:bg-surface-container-high hover:text-on-surface disabled:opacity-40"
          >
            <RotateCcw className={cn("h-3.5 w-3.5", loadingCapability && "animate-spin")} />
          </button>
        </div>
        <div className="mt-1.5 line-clamp-2 font-mono text-[11px] leading-4 text-outline">
          {controlMessage}
        </div>

        <div className="mt-2 flex flex-wrap gap-1.5">
          <button
            type="button"
            onClick={submitResume}
            disabled={!canResumeSession || resumePending}
            className={cn(
              "inline-flex items-center gap-1.5 rounded border px-2 py-1 font-mono text-[10px] uppercase tracking-[0.12em] transition-colors disabled:opacity-40",
              canResumeSession
                ? "border-primary/50 bg-primary/10 text-primary hover:bg-primary/15"
                : "border-border-faint text-outline",
            )}
          >
            {resumePending ? <RefreshCw className="h-3.5 w-3.5 animate-spin" /> : <Play className="h-3.5 w-3.5" />}
            Resume session
          </button>
        </div>

        <div className="mt-2 flex flex-wrap gap-1.5">
          {templates.map((template) => (
            <button
              key={template.label}
              type="button"
              onClick={() => setNotifyText(template.text)}
              disabled={!canNotifySession}
              className="rounded border border-border-faint px-2 py-1 font-mono text-[10px] uppercase tracking-[0.12em] text-outline hover:bg-surface-container-high hover:text-on-surface disabled:opacity-40"
            >
              {template.label}
            </button>
          ))}
        </div>

        <div className="mt-2 flex items-end gap-2">
          <textarea
            value={notifyText}
            onChange={(event) => setNotifyText(event.target.value)}
            disabled={!canNotifySession || notifyPending}
            rows={1}
            placeholder={canNotifySession ? `nudge ${peerLabel(peer)}'s running agent...` : "nudges need a running agent"}
            className="max-h-20 min-h-8 flex-1 resize-none rounded border border-border-faint bg-surface-container-lowest px-2.5 py-1.5 font-mono text-xs text-on-surface outline-none placeholder:text-outline focus:border-primary focus:ring-1 focus:ring-primary disabled:opacity-50"
          />
          <button
            type="button"
            onClick={submitNotify}
            disabled={!canNotifySession || !notifyText.trim() || notifyPending}
            aria-label="Send session nudge"
            className={cn(
              "flex h-8 w-8 shrink-0 items-center justify-center rounded transition-[filter,transform] active:scale-[0.98]",
              canNotifySession && notifyText.trim()
                ? "bg-primary text-on-primary hover:brightness-110"
                : "bg-surface-container-high text-outline",
            )}
          >
            {notifyPending ? <RefreshCw className="h-4 w-4 animate-spin" /> : <Bell className="h-4 w-4" />}
          </button>
        </div>
        {(notifyStatus || notifyError || resumeStatus || resumeError) && (
          <div
            className={cn(
              "mt-2 flex items-center gap-2 font-mono text-xs",
              notifyError || resumeError ? "text-error" : "text-primary",
            )}
          >
            {notifyError || resumeError ? <AlertCircle className="h-3.5 w-3.5" /> : <Check className="h-3.5 w-3.5" />}
            <span>{notifyError || resumeError || notifyStatus || resumeStatus}</span>
          </div>
        )}
      </div>
    </div>
  );
}

function ComposeBar({
  peer,
  apiBase,
  events,
  onSent,
}: {
  peer: Peer;
  apiBase: string;
  events: Event[];
  onSent?: () => void;
}) {
  // Draft text / file live in a per-peer external store, not local state, so
  // they survive peer switches without leaking across peers via shared
  // ComposeBar state. The store also flips protection SYNCHRONOUSLY when the
  // dirty bit changes — closing the "passive effect race" where an SSE event
  // could arrive after onChange but before a dirty-effect ran.
  const text = useDraftText(peer.peer_id);
  const file = useDraftFile(peer.peer_id);
  const setText = (next: string) => setDraftText(peer.peer_id, next);
  const setFile = (next: File | null) => setDraftFile(peer.peer_id, next);
  const isDirty = text.trim().length > 0 || file !== null;

  const [isPending, setIsPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pendingAsks, setPendingAsks] = useState<PendingAsk[]>([]);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  // History navigation cursor: null = not navigating; otherwise an index into
  // getHistory(peer_id) (0 = oldest). Reset whenever the composer is edited or
  // sent so a fresh ArrowUp starts from the newest entry.
  const historyIdx = useRef<number | null>(null);
  // Bumped on send/clear so the "clear history" affordance re-evaluates (the
  // history store is a plain module, not a reactive source).
  const [historyTick, setHistoryTick] = useState(0);
  const [showTemplates, setShowTemplates] = useState(false);
  // Composer mode: "ask" (tracked, ack lifecycle) or "notify" (fire-and-forget).
  const [mode, setMode] = useState<"ask" | "notify">("ask");
  const [notifyStatus, setNotifyStatus] = useState<{ ok: boolean; text: string } | null>(null);
  // Reset to the safe default on peer switch — the component may not remount on
  // a peer prop change, and a sticky "notify" would be a surprise-send footgun.
  useEffect(() => {
    setMode("ask");
    setNotifyStatus(null);
  }, [peer.peer_id]);

  // Insert a template into the draft: into an empty composer as-is, else
  // appended (never silently clobber unsent text).
  const insertTemplate = (tplText: string) => {
    setText(text.trim() ? `${text}\n\n${tplText}` : tplText);
    historyIdx.current = null;
    setShowTemplates(false);
    textareaRef.current?.focus();
  };

  useEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 120)}px`;
  }, [text]);

  // Match incoming notification events to pending asks via [ack #cid from @peer] framing.
  const openCids = useMemo(
    () => pendingAsks.filter((a) => a.state === "pending").map((a) => a.correlation_id),
    [pendingAsks]
  );
  useEffect(() => {
    if (openCids.length === 0) return;
    for (const ev of events) {
      if (ev.type !== "notification" || !ev.text) continue;
      const m = ev.text.match(ACK_FRAME_RE);
      if (!m) continue;
      const [, cid, from, body] = m;
      if (!openCids.includes(cid)) continue;
      setPendingAsks((prev) =>
        prev.map((a) =>
          a.correlation_id === cid && a.state === "pending"
            ? { ...a, state: "delivered", reply: body, reply_from: from }
            : a
        )
      );
    }
  }, [events, openCids]);

  // Bare-ack soft timeout: flip pending → timed_out after 120s.
  useEffect(() => {
    if (openCids.length === 0) return;
    const timers = openCids.map((cid) => {
      const ask = pendingAsks.find((a) => a.correlation_id === cid);
      if (!ask) return null;
      const elapsed = Date.now() - ask.sent_at;
      const remaining = Math.max(0, BARE_ACK_TIMEOUT_MS - elapsed);
      return window.setTimeout(() => {
        setPendingAsks((prev) =>
          prev.map((a) =>
            a.correlation_id === cid && a.state === "pending"
              ? { ...a, state: "timed_out" }
              : a
          )
        );
      }, remaining);
    });
    return () => {
      for (const t of timers) if (t !== null) window.clearTimeout(t);
    };
  }, [openCids, pendingAsks]);

  const dismissAsk = (cid: string) =>
    setPendingAsks((prev) => prev.filter((a) => a.correlation_id !== cid));

  const uploadFile = async (upload: File): Promise<AttachmentRef | null> => {
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
      return data as AttachmentRef;
    } catch {
      return null;
    }
  };

  const submit = async () => {
    if ((!text.trim() && !file) || isPending) return;
    setError(null);
    setIsPending(true);

    try {
      let msg = text.trim();
      let attachments: AttachmentRef[] = [];
      const hint = "\n(from @dashboard - reply naturally, dashboard sees your response automatically)";
      if (file) {
        const attachment = await uploadFile(file);
        if (!attachment) {
          setError("Failed to upload file");
          return;
        }
        attachments = [attachment];
        if (attachment.path) {
          msg = msg
            ? `${msg}\n[Attachment: ${attachment.path}]`
            : `[Attachment: ${attachment.path}]`;
        }
      }

      if (mode === "notify") {
        // Fire-and-forget: no ack lifecycle, no pending-ask card.
        const res = await fetch(`${apiBase}/notify`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            from_peer: "dashboard",
            to_peer: peer.peer_id,
            text: msg,
            attachments,
            bypass_circle: true,
          }),
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok || data.error) {
          // Preserve the draft so the send can be retried.
          setError(data.error || data.detail || `Error ${res.status}`);
        } else {
          setNotifyStatus({ ok: true, text: "notified" });
          pushHistory(peer.peer_id, text);
          historyIdx.current = null;
          setHistoryTick((t) => t + 1);
          clearDraft(peer.peer_id);
          onSent?.();
        }
        return;
      }

      const res = await fetch(`${apiBase}/ask`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          from_peer: "dashboard",
          to_peer: peer.peer_id,
          text: msg + hint,
          attachments,
          bypass_circle: true,
        }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || data.error) {
        setError(data.error || data.detail || `Error ${res.status}`);
      } else if (data.correlation_id) {
        const preview = msg.length > 60 ? msg.slice(0, 60) + "…" : msg;
        setPendingAsks((prev) => [
          ...prev,
          {
            correlation_id: data.correlation_id,
            to_peer_id: peer.peer_id,
            preview,
            sent_at: Date.now(),
            state: "pending",
            attachments,
          },
        ]);
        pushHistory(peer.peer_id, text);
        historyIdx.current = null;
        setHistoryTick((t) => t + 1);
        clearDraft(peer.peer_id);
        onSent?.();
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed");
    } finally {
      setIsPending(false);
    }
  };

  // Whether ArrowUp/Down should drive command-history recall right now. True
  // when the composer is empty (start a fresh recall) or we're already
  // navigating and the value still exactly matches the recalled entry (the user
  // hasn't started editing it). A textarea with unrelated multi-line text keeps
  // native arrow behavior.
  const navigatingHistory = (): boolean => {
    if (historyIdx.current === null) return text.length === 0;
    const entries = getHistory(peer.peer_id);
    return text === entries[historyIdx.current];
  };

  const recallHistory = (direction: "older" | "newer") => {
    const entries = getHistory(peer.peer_id);
    if (entries.length === 0) return;
    let idx = historyIdx.current;
    if (idx === null) {
      // Begin at the newest entry on the first ArrowUp.
      idx = direction === "older" ? entries.length - 1 : entries.length - 1;
    } else if (direction === "older") {
      idx = Math.max(0, idx - 1);
    } else {
      idx = idx + 1;
    }
    if (idx >= entries.length) {
      // Walked past the newest entry → return to an empty composer.
      historyIdx.current = null;
      setText("");
      return;
    }
    historyIdx.current = idx;
    setText(entries[idx]);
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    // Chat convention: Enter sends, Shift+Enter inserts a newline. Do not
    // submit while an IME is still composing text. Cmd/Ctrl+Enter continues
    // to send because it is also an unshifted Enter.
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault();
      submit();
      return;
    }
    if (event.key === "Escape" && historyIdx.current !== null) {
      event.preventDefault();
      historyIdx.current = null;
      setText("");
      return;
    }
    if ((event.key === "ArrowUp" || event.key === "ArrowDown") && navigatingHistory()) {
      event.preventDefault();
      recallHistory(event.key === "ArrowUp" ? "older" : "newer");
    }
  };

  const visibleAsks = pendingAsks.filter((a) => a.to_peer_id === peer.peer_id);
  // historyTick is read so a send/clear re-evaluates this (history is a plain module).
  void historyTick;
  const hasHistory = getHistory(peer.peer_id).length > 0;

  return (
    <div className="sticky bottom-0 z-10 border-t border-border-faint bg-surface-dim p-3 pb-[max(env(safe-area-inset-bottom),0.75rem)] md:static md:p-4">
      <div className="mb-2 flex items-center gap-2">
        <div className="inline-flex overflow-hidden rounded border border-border-faint" role="group" aria-label="composer mode">
          {(["ask", "notify"] as const).map((m) => (
            <button
              key={m}
              data-testid={`mode-${m}`}
              onClick={() => {
                setMode(m);
                setNotifyStatus(null);
              }}
              aria-pressed={mode === m}
              className={cn(
                "px-2 py-0.5 font-mono text-[9px] uppercase tracking-[0.14em] transition-colors",
                mode === m ? "bg-surface-container-high text-on-surface" : "text-outline hover:text-on-surface",
              )}
            >
              {m}
            </button>
          ))}
        </div>
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-outline">
          &rarr; {peerLabel(peer)}
        </span>
        {notifyStatus && (
          <span
            data-testid="notify-status"
            className={cn(
              "font-mono text-[9px] uppercase tracking-[0.14em]",
              notifyStatus.ok ? "text-secondary" : "text-error",
            )}
          >
            {notifyStatus.text}
          </span>
        )}
        {hasHistory && (
          <button
            data-testid="clear-history"
            onClick={() => {
              clearHistory(peer.peer_id);
              historyIdx.current = null;
              setHistoryTick((t) => t + 1);
            }}
            title="Clear this peer's command history"
            className="font-mono text-[9px] uppercase tracking-[0.14em] text-outline hover:text-on-surface"
          >
            clear history
          </button>
        )}
        <div className="relative">
          <button
            data-testid="templates-toggle"
            onClick={() => setShowTemplates((v) => !v)}
            aria-expanded={showTemplates}
            title="Ask templates"
            className="font-mono text-[9px] uppercase tracking-[0.14em] text-outline hover:text-on-surface"
          >
            templates
          </button>
          {showTemplates && (
            <TemplatesMenu
              draft={text}
              onInsert={insertTemplate}
              onClose={() => setShowTemplates(false)}
            />
          )}
        </div>
        {isDirty && (
          <span
            data-testid="compose-draft-pill"
            title="Inbound updates paused while draft is unsaved"
            className="inline-flex items-center gap-1 border border-primary/40 bg-primary/10 px-1.5 py-0.5 font-mono text-[9px] uppercase tracking-[0.14em] text-primary"
          >
            <span className="h-1.5 w-1.5 rounded-full bg-primary" aria-hidden="true" />
            draft
          </span>
        )}
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
          data-testid="compose-textarea"
          data-dirty={isDirty ? "true" : "false"}
          value={text}
          onChange={(event) => {
            // A user edit exits history navigation: once the value diverges
            // from the recalled entry it's an owned draft, so Esc must not
            // clear it and arrows return to native behavior.
            if (historyIdx.current !== null) {
              const entries = getHistory(peer.peer_id);
              if (event.target.value !== entries[historyIdx.current]) {
                historyIdx.current = null;
              }
            }
            if (notifyStatus) setNotifyStatus(null); // transient: clear on next edit
            setText(event.target.value);
          }}
          onKeyDown={onKeyDown}
          placeholder={`${mode} ${peerLabel(peer)} something...`}
          rows={1}
          className={cn(
            "max-h-32 min-h-10 flex-1 resize-none rounded border bg-surface-container-lowest px-3 py-2.5 font-mono text-base text-on-surface outline-none placeholder:text-outline focus:border-primary focus:ring-1 focus:ring-primary md:text-sm",
            isDirty ? "border-primary/40" : "border-border-faint"
          )}
        />
        <button
          onClick={submit}
          disabled={(!text.trim() && !file) || isPending}
          aria-label={mode === "notify" ? "Notify peer" : "Ask peer"}
          aria-busy={isPending}
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

      {visibleAsks.length > 0 && (
        <div className="mt-2 flex flex-col gap-1.5">
          {visibleAsks.map((a) => (
            <div
              key={a.correlation_id}
              className={cn(
                "border bg-surface-container-lowest px-3 py-2 font-mono text-xs",
                a.state === "delivered"
                  ? "border-primary/40"
                  : a.state === "timed_out"
                  ? "border-border-faint text-outline"
                  : "border-border-faint"
              )}
            >
              <div className="flex items-center gap-2">
                {a.state === "delivered" ? (
                  <Check className="h-3 w-3 shrink-0 text-primary" aria-hidden="true" />
                ) : a.state === "timed_out" ? (
                  <Check className="h-3 w-3 shrink-0 text-outline" aria-hidden="true" />
                ) : (
                  <Clock className="h-3 w-3 shrink-0 animate-pulse text-outline" aria-hidden="true" />
                )}
                <span className="shrink-0 text-[10px] uppercase tracking-[0.14em] text-outline">
                  #{a.correlation_id.slice(0, 8)}
                </span>
                <span className="hidden flex-1 truncate text-on-surface-variant md:inline">{a.preview}</span>
                <span className="ml-auto shrink-0 text-[10px] text-outline md:ml-0">
                  {a.state === "pending"
                    ? "pending"
                    : a.state === "delivered"
                    ? `reply from @${a.reply_from}`
                    : "acked (no reply)"}
                </span>
                <button
                  onClick={() => dismissAsk(a.correlation_id)}
                  aria-label="Dismiss"
                  className="shrink-0 p-0.5 text-outline hover:text-on-surface"
                >
                  <X className="h-3 w-3" aria-hidden="true" />
                </button>
              </div>
              <div className="mt-1 pl-5 text-on-surface-variant [overflow-wrap:anywhere] md:hidden">
                {a.preview}
              </div>
              {a.state === "delivered" && a.reply && (
                <div className="mt-1.5 max-h-24 overflow-y-auto whitespace-pre-wrap pl-5 text-on-surface-variant">
                  {a.reply}
                </div>
              )}
              <div className="pl-5">
                <AttachmentChips attachments={a.attachments} apiBase={apiBase} compact />
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function GitStatusBadge({ status }: { status?: { ahead: number; behind: number; dirty: number; staged: number } }) {
  if (!status) return null;
  const { ahead, behind, dirty, staged } = status;
  const hasLocal = dirty > 0 || staged > 0;
  const hasRemote = ahead > 0 || behind > 0;
  const color = hasLocal && hasRemote
    ? "bg-error"
    : hasRemote
    ? "bg-tertiary"
    : hasLocal
    ? "bg-accent"
    : "bg-secondary";
  const tooltip = `git: ${ahead} ahead, ${behind} behind, ${staged} staged, ${dirty} dirty`;
  return (
    <span
      title={tooltip}
      aria-label={tooltip}
      className={cn("inline-block h-2 w-2 shrink-0 rounded-full", color)}
    />
  );
}

function TabButton({ label, active, onClick }: { label: string; active: boolean; onClick: () => void }) {
  return (
    <button
      role="tab"
      aria-selected={active}
      onClick={onClick}
      className={cn(
        "border-b-2 px-3 py-2 font-mono text-[11px] uppercase tracking-[0.14em]",
        active
          ? "border-primary text-on-surface"
          : "border-transparent text-outline hover:text-on-surface"
      )}
    >
      {label}
    </button>
  );
}

interface McpServerEntry {
  name: string;
  scope: string;
  type: string;
  command: string | null;
  args: string[];
  url: string | null;
  env_keys: string[];
}

interface McpConfigScope {
  backend: string;
  owner: string;
  effective_scope: string;
  label: string;
  description: string;
  supported_scopes: string[];
  default_scope: string;
  is_global: boolean;
  peer_id: string;
  peer_name: string;
  project_path: string | null;
  peer_machine: string | null;
  self_machine: string;
  same_host: boolean;
}

function fallbackMcpScope(peer: Peer): McpConfigScope {
  const backend = peer.backend || "unknown";
  const isGlobal = backend === "codex";
  return {
    backend,
    owner: isGlobal ? "backend" : "peer/project",
    effective_scope: isGlobal ? "backend_global" : "peer_project",
    label: isGlobal ? `${backend} global backend config` : `${backend} peer/project config`,
    description: isGlobal
      ? `${backend} MCP edits target the user-level backend config shared by sessions on this host.`
      : `${backend} MCP edits target this peer and may support project/worktree scope.`,
    supported_scopes: backend === "claude-code" ? ["user", "project"] : ["user"],
    default_scope: "user",
    is_global: isGlobal,
    peer_id: peer.peer_id,
    peer_name: peer.display_name || peer.name,
    project_path: peer.path || null,
    peer_machine: peer.machine || null,
    self_machine: "",
    same_host: true,
  };
}

function McpScopeBanner({ scope }: { scope: McpConfigScope }) {
  return (
    <div className="mb-3 rounded border border-border-faint bg-surface-container-low px-3 py-2 font-mono text-xs text-outline">
      <div className="font-semibold text-on-surface-variant">{scope.label}</div>
      <div className="mt-1">{scope.description}</div>
      <div className="mt-1 text-[10px] uppercase tracking-wider">
        owner: {scope.owner} · scope: {scope.effective_scope}
        {scope.project_path ? ` · worktree: ${scope.project_path}` : ""}
      </div>
    </div>
  );
}

function McpPanel({ peer, apiBase }: { peer: Peer; apiBase: string }) {
  const [servers, setServers] = useState<McpServerEntry[] | null>(null);
  const [configScope, setConfigScope] = useState<McpConfigScope | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [crossHost, setCrossHost] = useState(false);
  const [unsupported, setUnsupported] = useState(false);
  const [showAdd, setShowAdd] = useState(false);
  const [refreshTick, setRefreshTick] = useState(0);
  const peerScopeFallback = useMemo(() => fallbackMcpScope(peer), [peer]);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      await Promise.resolve();
      if (cancelled) return;
      setServers(null);
      setConfigScope(null);
      setError(null);
      setCrossHost(false);
      setUnsupported(false);
      try {
        const r = await fetch(`${apiBase}/peers/${encodeURIComponent(peer.name)}/mcp`);
        if (r.status === 409) {
          const body = await r.json().catch(() => ({}));
          if (body?.detail?.error === "cross_host") {
            if (!cancelled) {
              setConfigScope(body.detail.config_scope || peerScopeFallback);
              setCrossHost(true);
            }
            return;
          }
        }
        if (r.status === 501) {
          if (!cancelled) {
            setConfigScope(peerScopeFallback);
            setUnsupported(true);
          }
          return;
        }
        if (!r.ok) {
          const text = await r.text();
          if (!cancelled) setError(text || `HTTP ${r.status}`);
          return;
        }
        const data = await r.json();
        if (!cancelled) {
          setConfigScope(data.config_scope || peerScopeFallback);
          setServers(data.servers || []);
        }
      } catch (e) {
        if (!cancelled) setError(String(e));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [peer.name, apiBase, refreshTick, peerScopeFallback]);

  async function handleRemove(name: string) {
    if (!confirm(`Remove MCP server "${name}"?`)) return;
    const r = await fetch(
      `${apiBase}/peers/${encodeURIComponent(peer.name)}/mcp/${encodeURIComponent(name)}`,
      { method: "DELETE" }
    );
    if (!r.ok) {
      const text = await r.text();
      setError(text || `HTTP ${r.status}`);
      return;
    }
    setRefreshTick((n) => n + 1);
  }

  if (crossHost) {
    return (
      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-6 md:px-6">
        {configScope && <McpScopeBanner scope={configScope} />}
        <div className="rounded border border-border-faint bg-surface-container-low p-4 font-mono text-xs text-outline">
          <div className="mb-1 font-semibold text-on-surface-variant">remote host</div>
          Per-peer MCP config is same-host only in v1. Peer runs on{" "}
          <span className="text-on-surface">{peer.machine}</span>; ACP transport is required to reach it.
        </div>
      </div>
    );
  }

  if (unsupported) {
    return (
      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-6 md:px-6">
        {configScope && <McpScopeBanner scope={configScope} />}
        <div className="rounded border border-border-faint bg-surface-container-low p-4 font-mono text-xs text-outline">
          Backend <span className="text-on-surface">{peer.backend}</span> does not support MCP config in v1.
        </div>
      </div>
    );
  }

  return (
    <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4 md:px-6">
      {configScope && <McpScopeBanner scope={configScope} />}

      {error && (
        <div className="mb-3 rounded border border-error/30 bg-error/10 px-3 py-2 font-mono text-xs text-on-surface">
          {error}
          <button
            onClick={() => { setError(null); setRefreshTick((n) => n + 1); }}
            className="ml-2 underline"
          >
            retry
          </button>
        </div>
      )}

      {servers === null ? (
        <div className="font-mono text-xs text-outline">loading...</div>
      ) : servers.length === 0 ? (
        <div className="py-6 font-mono text-xs text-outline">
          no MCP servers configured.
        </div>
      ) : (
        <ul className="space-y-2">
          {servers.map((s) => (
            <li
              key={s.name}
              className="flex items-start justify-between gap-3 rounded border border-border-faint bg-surface-container-low px-3 py-2 font-mono text-xs"
            >
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                  <span className="font-semibold text-on-surface">{s.name}</span>
                  <span className="text-[10px] uppercase tracking-wider text-outline">
                    {s.type}
                  </span>
                  {s.scope && s.scope !== "user" && (
                    <span className="text-[10px] uppercase tracking-wider text-outline">
                      {s.scope}
                    </span>
                  )}
                </div>
                <div className="mt-0.5 truncate text-outline">
                  {s.url || [s.command, ...s.args].filter(Boolean).join(" ")}
                </div>
                {s.env_keys.length > 0 && (
                  <div className="mt-0.5 text-[10px] text-outline">
                    env: {s.env_keys.join(", ")}
                  </div>
                )}
              </div>
              <button
                onClick={() => handleRemove(s.name)}
                className="shrink-0 border border-border-faint px-2 py-1 text-[10px] uppercase text-outline hover:bg-surface-container-high hover:text-on-surface"
              >
                remove
              </button>
            </li>
          ))}
        </ul>
      )}

      <div className="mt-4">
        {showAdd ? (
          <AddMcpForm
            peer={peer}
            apiBase={apiBase}
            configScope={configScope || peerScopeFallback}
            onCancel={() => setShowAdd(false)}
            onAdded={() => {
              setShowAdd(false);
              setRefreshTick((n) => n + 1);
            }}
            onError={(msg) => setError(msg)}
          />
        ) : (
          <button
            onClick={() => setShowAdd(true)}
            className="border border-border-faint px-3 py-1.5 font-mono text-[11px] uppercase tracking-[0.14em] text-on-surface hover:bg-surface-container-high"
          >
            + add server
          </button>
        )}
      </div>
    </div>
  );
}

function AddMcpForm({
  peer,
  apiBase,
  configScope,
  onCancel,
  onAdded,
  onError,
}: {
  peer: Peer;
  apiBase: string;
  configScope: McpConfigScope;
  onCancel: () => void;
  onAdded: () => void;
  onError: (msg: string) => void;
}) {
  const [name, setName] = useState("");
  const [type, setType] = useState<"stdio" | "http" | "sse">("stdio");
  const [command, setCommand] = useState("");
  const [argsText, setArgsText] = useState("");
  const [url, setUrl] = useState("");
  const [envText, setEnvText] = useState("");
  const [scope, setScope] = useState<"user" | "project">("user");
  const [submitting, setSubmitting] = useState(false);

  async function submit() {
    if (!name.trim()) {
      onError("server name is required");
      return;
    }
    if (configScope.is_global && !confirm(`Add "${name.trim()}" to ${configScope.label}? This backend config is shared beyond this peer.`)) {
      return;
    }
    const env: Record<string, string> = {};
    for (const line of envText.split("\n")) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      const eq = trimmed.indexOf("=");
      if (eq <= 0) continue;
      env[trimmed.slice(0, eq).trim()] = trimmed.slice(eq + 1);
    }
    setSubmitting(true);
    try {
      const r = await fetch(
        `${apiBase}/peers/${encodeURIComponent(peer.name)}/mcp?scope=${scope}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            name: name.trim(),
            type,
            command: type === "stdio" ? command.trim() : null,
            args: type === "stdio"
              ? argsText.split(/\s+/).filter(Boolean)
              : [],
            url: type !== "stdio" ? url.trim() : null,
            env,
          }),
        }
      );
      if (!r.ok) {
        const text = await r.text();
        onError(text || `HTTP ${r.status}`);
        return;
      }
      onAdded();
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="space-y-2 rounded border border-border-faint bg-surface-container-low p-3 font-mono text-xs">
      <div className="rounded border border-border-faint bg-surface px-2 py-1 text-[10px] uppercase tracking-wider text-outline">
        editing {configScope.label}
      </div>
      <input
        placeholder="name"
        value={name}
        onChange={(e) => setName(e.target.value)}
        className="w-full border border-border-faint bg-surface px-2 py-1 text-on-surface"
      />
      <div className="flex gap-2">
        <select
          value={type}
          onChange={(e) => setType(e.target.value as typeof type)}
          className="border border-border-faint bg-surface px-2 py-1 text-on-surface"
        >
          <option value="stdio">stdio</option>
          <option value="http">http</option>
          <option value="sse">sse</option>
        </select>
        {configScope.supported_scopes.length > 1 ? (
          <select
            value={scope}
            onChange={(e) => setScope(e.target.value as typeof scope)}
            className="border border-border-faint bg-surface px-2 py-1 text-on-surface"
          >
            <option value="user">user scope</option>
            <option value="project">project scope</option>
          </select>
        ) : (
          <div className="border border-border-faint bg-surface px-2 py-1 text-outline">
            scope: {configScope.default_scope}
          </div>
        )}
      </div>
      {type === "stdio" ? (
        <>
          <input
            placeholder="command"
            value={command}
            onChange={(e) => setCommand(e.target.value)}
            className="w-full border border-border-faint bg-surface px-2 py-1 text-on-surface"
          />
          <input
            placeholder="args (space separated)"
            value={argsText}
            onChange={(e) => setArgsText(e.target.value)}
            className="w-full border border-border-faint bg-surface px-2 py-1 text-on-surface"
          />
        </>
      ) : (
        <input
          placeholder="url"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          className="w-full border border-border-faint bg-surface px-2 py-1 text-on-surface"
        />
      )}
      <textarea
        placeholder="env (one KEY=value per line)"
        value={envText}
        onChange={(e) => setEnvText(e.target.value)}
        rows={3}
        className="w-full border border-border-faint bg-surface px-2 py-1 text-on-surface"
      />
      <div className="flex gap-2">
        <button
          onClick={submit}
          disabled={submitting}
          className="border border-primary/40 bg-primary/10 px-3 py-1 uppercase tracking-[0.14em] text-on-surface hover:bg-primary/20 disabled:opacity-50"
        >
          {submitting ? "adding..." : "add"}
        </button>
        <button
          onClick={onCancel}
          className="border border-border-faint px-3 py-1 uppercase tracking-[0.14em] text-outline hover:text-on-surface"
        >
          cancel
        </button>
      </div>
    </div>
  );
}

const EDITOR_SCHEMES: { id: string; label: string; href: (path: string) => string }[] = [
  { id: "vscode", label: "VS Code", href: (p) => `vscode://file/${encodeURI(p)}` },
  { id: "cursor", label: "Cursor", href: (p) => `cursor://file/${encodeURI(p)}` },
  { id: "zed", label: "Zed", href: (p) => `zed://${encodeURI(p)}` },
];

function OpenInEditorButton({ path }: { path: string }) {
  const [open, setOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (event: MouseEvent) => {
      if (!containerRef.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  return (
    <div ref={containerRef} className="relative hidden sm:inline-flex">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        title={`Open ${path} in editor`}
        aria-label="Open peer cwd in editor"
        aria-haspopup="menu"
        aria-expanded={open}
        data-testid="open-in-editor"
        className="flex h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface"
      >
        <span aria-hidden="true">↗</span>
      </button>
      {open && (
        <div
          role="menu"
          className="absolute right-0 top-9 z-20 w-32 rounded border border-border bg-surface-container-low py-1 shadow-md"
        >
          {EDITOR_SCHEMES.map((scheme) => (
            <a
              key={scheme.id}
              href={scheme.href(path)}
              role="menuitem"
              onClick={() => setOpen(false)}
              className="block px-3 py-1.5 font-mono text-[11px] text-on-surface transition-colors hover:bg-surface-container-high"
            >
              {scheme.label}
            </a>
          ))}
        </div>
      )}
    </div>
  );
}

function SwitchBackendControl({ peer, apiBase }: { peer: Peer; apiBase: string }) {
  const [configuredBackends, setConfiguredBackends] = useState<string[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    fetch(`${apiBase}/spawn/config`, { credentials: "include" })
      .then((r) => (r.ok ? r.json() : null))
      .then((data) => {
        if (cancelled || !data) return;
        setConfiguredBackends(
          data.commands && typeof data.commands === "object" ? Object.keys(data.commands) : []
        );
      })
      .catch(() => {
        if (!cancelled) setConfiguredBackends([]);
      });
    return () => {
      cancelled = true;
    };
  }, [apiBase]);

  // Map configured runtimes → unique backends, filter out current backend.
  const options = useMemo(() => {
    if (!configuredBackends) return [];
    const seen = new Set<string>();
    const out: string[] = [];
    for (const backend of configuredBackends) {
      if (!backend || backend === peer.backend || seen.has(backend)) continue;
      seen.add(backend);
      out.push(backend);
    }
    return out;
  }, [configuredBackends, peer.backend]);

  if (configuredBackends === null || options.length === 0) return null;

  async function onChange(event: React.ChangeEvent<HTMLSelectElement>) {
    const newBackend = event.target.value;
    event.target.value = "";  // reset placeholder
    if (!newBackend) return;
    if (!confirm(
      `Switch ${peer.name} from ${peer.backend} to ${newBackend}?\n\n` +
      `The current session will be killed and a fresh ${newBackend} session ` +
      `will start in the same directory. Conversation state is not preserved.`
    )) return;
    setError(null);
    setBusy(true);
    try {
      const r = await fetch(
        `${apiBase}/peers/${encodeURIComponent(peer.name)}/switch-backend`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          credentials: "include",
          body: JSON.stringify({ new_backend: newBackend }),
        }
      );
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        const detail = body?.detail;
        const hint = typeof detail === "string"
          ? detail
          : detail?.hint || detail?.error || `HTTP ${r.status}`;
        setError(hint);
        return;
      }
      // Success: peer list will refresh via events; nothing else to do here.
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="hidden items-center gap-1.5 sm:flex" title={error || undefined}>
      <select
        aria-label="Switch backend"
        disabled={busy}
        defaultValue=""
        onChange={onChange}
        className={cn(
          "h-8 max-w-[140px] truncate rounded border bg-surface-container-low px-2 font-mono text-[11px] uppercase tracking-[0.14em] text-on-surface-variant",
          error ? "border-error/60" : "border-border",
          busy && "opacity-50"
        )}
      >
        <option value="" disabled>
          {busy ? "switching…" : `switch → ${peer.backend}`}
        </option>
        {options.map((backend) => (
          <option key={backend} value={backend}>
            {backend}
          </option>
        ))}
      </select>
    </div>
  );
}

function timelineKey(sessionId?: string, turnId?: string): string | null {
  if (!turnId) return null;
  return `${sessionId || "legacy"}:${turnId}`;
}

function entryTargetKey(entry: ThreadItemEntry): string | null {
  if (entry.type === "history_turn" || entry.type === "chat_turn_delta_group") {
    return timelineKey(entry.session_id, entry.turn_id);
  }
  if (entry.type === "chat_turn") {
    return timelineKey(entry.session_id, entry.turn_id);
  }
  return null;
}

function mergeTimeline(
  historyTurns: TranscriptTurn[],
  sortedEvents: Event[],
  activeSessionId: string | null,
): ThreadItemEntry[] {
  const historyItems: HistoryTimelineTurn[] = historyTurns
    .filter((turn) => !activeSessionId || turn.session_id === activeSessionId)
    .map((turn) => ({
      ...turn,
      type: "history_turn",
      id: `history-${turn.session_id}-${turn.turn_id}`,
    }));
  const historyByKey = new Map<string, HistoryTimelineTurn>();
  const dedupedHistoryItems: HistoryTimelineTurn[] = [];
  for (const turn of historyItems) {
    const key = timelineKey(turn.session_id, turn.turn_id);
    if (!key) {
      dedupedHistoryItems.push(turn);
      continue;
    }
    if (!historyByKey.has(key)) dedupedHistoryItems.push(turn);
    historyByKey.set(key, turn);
  }

  const liveItems = coalesceDeltas(sortedEvents);
  const out: ThreadItemEntry[] = [];
  const replacedHistoryKeys = new Set<string>();

  for (const item of liveItems) {
    if (item.type === "chat_turn") {
      const key = timelineKey(item.session_id, item.turn_id);
      if (key) replacedHistoryKeys.add(key);
      out.push(item);
      continue;
    }
    if (item.type === "chat_turn_delta_group") {
      const key = timelineKey(item.session_id, item.turn_id);
      if (key && historyByKey.has(key)) continue;
      out.push(item);
      continue;
    }
    out.push(item);
  }

  for (const item of dedupedHistoryItems) {
    const key = timelineKey(item.session_id, item.turn_id);
    if (key && replacedHistoryKeys.has(key)) continue;
    out.push(key ? historyByKey.get(key) || item : item);
  }

  out.sort((a, b) => a.timestamp.localeCompare(b.timestamp));
  return out;
}

/** Coalesce chat_turn_delta events into one pending bubble per turn_id, then
 * drop any group whose turn_id has a matching final `chat_turn`.
 *
 * Reconciliation rule: the final `chat_turn` carries the same `turn_id` as
 * its deltas (assistant message uuid). Matching by id is order-independent —
 * a late delta arriving after the final still gets dropped, fixing the
 * "permanent streaming bubble" race that timestamp-based reconcile had.
 * Falls back to the timestamp heuristic only for deltas whose turn_id never
 * gets a matching final (network drop, agent crash mid-turn): a chat_turn
 * for the same peer that came *after* the delta absorbs it. */
function coalesceDeltas(sorted: Event[]): ThreadItemEntry[] {
  const groups = new Map<
    string,
    {
      group: ChatTurnDeltaGroup;
      deltas: Event[];
    }
  >();

  // Final chat_turns carrying turn_id — exact match drops their deltas
  // regardless of arrival order.
  const finalizedTurnIds = new Set<string>();
  // Fallback: per-peer last final-chat_turn timestamp, for turns where the
  // final didn't carry turn_id (legacy or codex transcripts without uuid).
  const peerLastChatTurnTs = new Map<string, string>();
  for (const ev of sorted) {
    if (ev.type !== "chat_turn" || ev.role !== "assistant") continue;
    const key = timelineKey(ev.session_id, ev.turn_id);
    if (key) finalizedTurnIds.add(key);
    if (ev.peer_id) {
      const prev = peerLastChatTurnTs.get(ev.peer_id);
      if (!prev || prev < ev.timestamp) peerLastChatTurnTs.set(ev.peer_id, ev.timestamp);
    }
  }

  for (const ev of sorted) {
    if (ev.type !== "chat_turn_delta" || !ev.turn_id) continue;
    const key = timelineKey(ev.session_id, ev.turn_id);
    if (key && finalizedTurnIds.has(key)) continue;
    const lastFinalTs = ev.peer_id ? peerLastChatTurnTs.get(ev.peer_id) : undefined;
    if (lastFinalTs && ev.timestamp <= lastFinalTs) continue;

    const groupKey = key || `event:${ev.id}`;
    let entry = groups.get(groupKey);
    if (!entry) {
      entry = {
        deltas: [],
        group: {
          type: "chat_turn_delta_group",
          id: `delta-group-${ev.turn_id}`,
          session_id: ev.session_id,
          turn_id: ev.turn_id,
          peer_id: ev.peer_id,
          timestamp: ev.timestamp,
          text: "",
          tool_calls: [],
        },
      };
      groups.set(groupKey, entry);
    }
    entry.deltas.push(ev);
    if (ev.timestamp > entry.group.timestamp) entry.group.timestamp = ev.timestamp;
  }

  const out: ThreadItemEntry[] = [];
  for (const ev of sorted) {
    if (ev.type === "chat_turn_delta") continue;
    out.push(ev);
  }
  for (const { group, deltas } of groups.values()) {
    const ordered = [...deltas].sort((a, b) => {
      const aIndex = typeof a.chunk_index === "number" ? a.chunk_index : Number.MAX_SAFE_INTEGER;
      const bIndex = typeof b.chunk_index === "number" ? b.chunk_index : Number.MAX_SAFE_INTEGER;
      if (aIndex !== bIndex) return aIndex - bIndex;
      return a.timestamp.localeCompare(b.timestamp);
    });
    for (const ev of ordered) {
      if (ev.kind === "tool_use" && ev.tool_call) {
        group.tool_calls.push(ev.tool_call);
      } else if ((ev.kind === "text" || ev.kind === undefined) && ev.text) {
        // Adjacent text blocks within one turn are conceptually paragraphs.
        group.text = group.text ? `${group.text}\n\n${ev.text}` : ev.text;
      }
    }
    if (!group.text && group.tool_calls.length === 0) continue;
    out.push(group);
  }
  out.sort((a, b) => a.timestamp.localeCompare(b.timestamp));
  return out;
}

function StreamingTurnItem({ group, peer }: { group: ChatTurnDeltaGroup; peer: Peer }) {
  return (
    <div className="mb-4 flex flex-col items-start">
      <div className="mb-1 flex items-center gap-2 font-mono text-[10px] font-medium uppercase tracking-[0.14em] text-outline">
        <span>{peerLabel(peer)} · {formatTime(group.timestamp)}</span>
        <span className="inline-flex items-center gap-1 rounded border border-primary/40 bg-primary/10 px-1.5 py-px text-[9px] text-primary">
          <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-primary" />
          streaming
        </span>
      </div>
      {group.text && (
        <div className="max-w-[82%] min-w-0 rounded border-l-2 border-primary/70 bg-surface-container-high p-3 font-mono text-[13px] leading-6 text-on-surface [overflow-wrap:anywhere]">
          <div className="prose prose-sm max-w-none break-words [&_pre]:overflow-x-auto">
            <ReactMarkdown remarkPlugins={[remarkGfm]}>{group.text}</ReactMarkdown>
          </div>
        </div>
      )}
      {group.tool_calls.length > 0 && <ToolCallBlock toolCalls={group.tool_calls} />}
    </div>
  );
}

function PathCopyButton({ path, parent, folder }: { path: string; parent: string; folder: string }) {
  const [copied, setCopied] = useState(false);
  const handleCopy = () => {
    navigator.clipboard.writeText(path);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };
  return (
    <button
      type="button"
      onClick={handleCopy}
      title={copied ? "Copied" : `Copy ${path}`}
      aria-label={`Copy peer cwd ${path}`}
      data-testid="copy-peer-cwd"
      className="cursor-pointer rounded px-0.5 transition-colors hover:bg-surface-container-high hover:text-on-surface focus:outline focus:outline-1 focus:outline-primary"
    >
      {parent}
      <span className="text-on-surface-variant">{folder}</span>
      {copied ? (
        <span className="ml-1 inline-flex items-center text-secondary" aria-hidden="true">✓</span>
      ) : null}
    </button>
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
      aria-label="Copy peer name"
      className="hidden h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface sm:inline-flex"
    >
      {copied ? <Check className="h-3.5 w-3.5 text-secondary" /> : <Copy className="h-3.5 w-3.5" />}
    </button>
  );
}

function TemplatesMenu({
  draft,
  onInsert,
  onClose,
}: {
  draft: string;
  onInsert: (text: string) => void;
  onClose: () => void;
}) {
  const [tick, setTick] = useState(0);
  const [name, setName] = useState("");
  void tick; // re-render trigger; templates store is a plain module
  const templates = listTemplates();

  const save = () => {
    const result = saveTemplate(name, draft);
    if (result !== "rejected") {
      setName("");
      setTick((t) => t + 1);
    }
  };

  return (
    <div
      data-testid="templates-menu"
      className="absolute bottom-full left-0 z-20 mb-1 w-72 rounded border border-border bg-surface-container-low p-2 shadow-lg"
    >
      <div className="mb-1.5 flex items-center justify-between">
        <span className="font-mono text-[9px] uppercase tracking-[0.14em] text-outline">Ask templates</span>
        <button onClick={onClose} aria-label="Close templates" className="text-outline hover:text-on-surface">
          <X className="h-3 w-3" />
        </button>
      </div>
      <div className="max-h-48 overflow-y-auto">
        {templates.length === 0 ? (
          <p className="px-1 py-2 font-mono text-[11px] text-outline">No templates yet.</p>
        ) : (
          templates.map((t: Template) => (
            <div key={t.id} className="flex items-center gap-1.5 px-1 py-1 hover:bg-surface-container-high">
              <button
                onClick={() => onInsert(t.text)}
                title={t.text}
                className="min-w-0 flex-1 truncate text-left font-mono text-[11px] text-on-surface-variant hover:text-on-surface"
              >
                {t.name}
              </button>
              <button
                onClick={() => {
                  deleteTemplate(t.id);
                  setTick((x) => x + 1);
                }}
                aria-label={`Delete template ${t.name}`}
                className="shrink-0 text-outline hover:text-error"
              >
                <X className="h-3 w-3" />
              </button>
            </div>
          ))
        )}
      </div>
      <div className="mt-2 flex items-center gap-1.5 border-t border-border-faint pt-2">
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="save draft as…"
          aria-label="Template name"
          className="min-w-0 flex-1 rounded border border-border-faint bg-surface-container-lowest px-1.5 py-1 font-mono text-[11px] text-on-surface outline-none placeholder:text-outline focus:border-primary"
        />
        <button
          onClick={save}
          disabled={!name.trim() || !draft.trim()}
          className="shrink-0 rounded bg-primary px-2 py-1 font-mono text-[10px] font-bold uppercase tracking-[0.1em] text-on-primary disabled:opacity-40"
        >
          Save
        </button>
      </div>
    </div>
  );
}
