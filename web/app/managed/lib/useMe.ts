import { useEffect, useState } from "react";
import { ApiError, managedApi, type Me } from "./api";

export type Gate =
  | { kind: "loading" }
  | { kind: "ready"; me: Me }
  | { kind: "signed-out" }
  | { kind: "unconfigured"; message: string }
  | { kind: "error"; message: string };

/** Who is signed in, and whether Repowire Managed is configured on this relay. */
export function gateFor(result: { me: Me } | { error: unknown }): Gate {
  if ("me" in result) {
    return result.me.managed ? { kind: "ready", me: result.me } : { kind: "unconfigured", message: "Repowire Managed is not enabled on this relay." };
  }
  const error = result.error;
  if (error instanceof ApiError && error.status === 401) return { kind: "signed-out" };
  if (error instanceof ApiError && error.status === 503) return { kind: "unconfigured", message: error.message };
  return { kind: "error", message: error instanceof Error ? error.message : String(error) };
}

export function useMe(): [Gate, () => void] {
  const [gate, setGate] = useState<Gate>({ kind: "loading" });
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    let cancelled = false;
    managedApi.me().then(
      (me) => !cancelled && setGate(gateFor({ me })),
      (error) => !cancelled && setGate(gateFor({ error })),
    );
    return () => {
      cancelled = true;
    };
  }, [attempt]);
  return [gate, () => setAttempt((n) => n + 1)];
}
