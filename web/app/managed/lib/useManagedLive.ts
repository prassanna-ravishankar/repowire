import { useCallback, useEffect, useReducer, useRef, useState } from "react";
import { managedApi, type Agent, type Clients, type ManagedFrame, type RelayDaemon } from "./api";
import { EMPTY_LIVE, applyFrame } from "./state";
import { useManagedEvents, type LiveConnection } from "./useManagedEvents";

/** The relay's own link to the Worker. Down means the stream is open but carries nothing. */
export type Upstream = { connected: true } | { connected: false; error: string | null };

type RelayFrame =
  | { type: "gateway"; connected: boolean; error?: string }
  | { type: "daemons"; daemons: RelayDaemon[] }
  | { type: "daemon"; daemon: unknown };

/**
 * Page state for /managed: HTTP for the full picture, SSE for changes.
 *
 * A resync (mount, every SSE reconnect, the relay's Worker link recovering)
 * fetches agents, workspaces and clients. Frames that arrive while it is in
 * flight are buffered and replayed on top of its result, so a slow response
 * can never overwrite something newer the stream already delivered. A newer
 * resync supersedes an older one.
 */
export function useManagedLive() {
  const [live, dispatch] = useReducer(applyFrame, EMPTY_LIVE);
  const [loaded, setLoaded] = useState(false);
  const [listError, setListError] = useState<string | null>(null);
  const [clients, setClients] = useState<Clients | null>(null);
  const [clientsError, setClientsError] = useState<string | null>(null);
  const [upstream, setUpstream] = useState<Upstream>({ connected: true });
  const buffer = useRef<ManagedFrame[] | null>(null);
  const generation = useRef(0);
  // Frame sequence, and the last sequence that touched each agent (a snapshot touches all).
  const seq = useRef(0);
  const touched = useRef({ all: 0, byAgent: new Map<string, number>() });

  const loadClients = useCallback(() => {
    managedApi.clients().then(
      (data) => {
        setClients(data);
        setClientsError(null);
      },
      (err: Error) => setClientsError(err.message),
    );
  }, []);

  const resync = useCallback(() => {
    const gen = ++generation.current;
    buffer.current = [];
    return Promise.all([managedApi.agents(), managedApi.workspaces(), managedApi.clients()]).then(
      ([agents, workspaces, clientData]) => {
        if (gen !== generation.current) return;
        const replay = buffer.current ?? [];
        buffer.current = null;
        setListError(null);
        setClients(clientData);
        dispatch({ type: "snapshot", agents, workspaces, clients: clientData.gateway, outbox: clientData.outbox, at: new Date().toISOString() });
        for (const frame of replay) dispatch(frame);
        setLoaded(true);
      },
      (err: Error) => {
        if (gen !== generation.current) return;
        buffer.current = null;
        setListError(err.message);
      },
    );
  }, []);

  useEffect(() => {
    void resync();
  }, [resync]);

  const onFrame = useCallback((frame: ManagedFrame) => {
    const n = ++seq.current;
    if (frame.type === "snapshot") touched.current.all = n;
    if (frame.type === "agent") touched.current.byAgent.set(frame.agent.id, n);
    buffer.current?.push(frame);
    dispatch(frame);
    // Any Worker frame proves the relay's link to it is up.
    setUpstream((current) => (current.connected ? current : { connected: true }));
    if (frame.type === "snapshot") setLoaded(true);
  }, []);

  const onRelay = useCallback(
    (data: unknown) => {
      const frame = data as RelayFrame | null;
      if (!frame || typeof frame !== "object") return;
      if (frame.type === "gateway") {
        if (frame.connected) {
          setUpstream({ connected: true });
          void resync();
        } else {
          setUpstream({ connected: false, error: frame.error ?? null });
        }
      } else if (frame.type === "daemons") {
        setClients((current) => (current ? { ...current, daemons: frame.daemons } : current));
      } else if (frame.type === "daemon") {
        loadClients();
      }
    },
    [resync, loadClients],
  );

  /**
   * For single-agent HTTP results (a refresh, or the reply to create or
   * retire): call `since()` before the request, then `applyAgent(agent, token)`.
   * It is dropped if the stream already said something newer about that agent.
   */
  const since = useCallback(() => seq.current, []);
  const applyAgent = useCallback((agent: Agent, token: number) => {
    const last = Math.max(touched.current.all, touched.current.byAgent.get(agent.id) ?? 0);
    if (last > token) return false;
    dispatch({ type: "agent", agent });
    return true;
  }, []);

  const onOpen = useCallback((reconnected: boolean) => reconnected && void resync(), [resync]);

  const connection: LiveConnection = useManagedEvents({ enabled: true, onFrame, onRelay, onOpen });

  return { live, dispatch, since, applyAgent, loaded, listError, clients, clientsError, loadClients, resync, connection, upstream };
}
