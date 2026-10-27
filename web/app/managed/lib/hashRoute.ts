import { useCallback, useSyncExternalStore } from "react";

export type AgentTab = "chat" | "runs" | "asks" | "workspace";

export type Route =
  | { view: "agents"; agent: null }
  | { view: "agents"; agent: string; tab: AgentTab }
  | { view: "clients" };

/** `#/agents`, `#/agents/:id`, `#/agents/:id/runs|asks|workspace`, `#/clients`. Anything else is the agent list. */
export function parseRoute(hash: string): Route {
  const parts = hash.replace(/^#\/?/, "").split("/").filter(Boolean).map(decodeURIComponent);
  if (parts[0] === "clients") return { view: "clients" };
  if (parts[0] === "agents" && parts[1]) {
    const tab = parts[2] === "runs" || parts[2] === "asks" || parts[2] === "workspace" ? parts[2] : "chat";
    return { view: "agents", agent: parts[1], tab };
  }
  return { view: "agents", agent: null };
}

export function routeHash(route: Route): string {
  if (route.view === "clients") return "#/clients";
  if (!route.agent) return "#/agents";
  const base = `#/agents/${encodeURIComponent(route.agent)}`;
  return route.tab === "chat" ? base : `${base}/${route.tab}`;
}

function subscribe(onChange: () => void) {
  window.addEventListener("hashchange", onChange);
  return () => window.removeEventListener("hashchange", onChange);
}

export function useHashRoute(): [Route, (route: Route) => void] {
  const hash = useSyncExternalStore(subscribe, () => window.location.hash, () => "");
  const navigate = useCallback((route: Route) => {
    window.location.hash = routeHash(route);
  }, []);
  return [parseRoute(hash), navigate];
}
