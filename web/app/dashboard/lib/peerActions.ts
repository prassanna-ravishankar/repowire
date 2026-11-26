// Operator actions on one peer from the dashboard, through the daemon's own
// routes. Each resolves to null on success or to the error to show.

/** Sets a peer's task description (POST /peers/{id}/description). An empty
 *  description clears it. */
export async function setPeerDescription(apiBase: string, peerId: string, description: string): Promise<string | null> {
  try {
    const res = await fetch(`${apiBase}/peers/${encodeURIComponent(peerId)}/description`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ description: description.trim() }),
    });
    if (res.ok) return null;
    const data = (await res.json().catch(() => ({}))) as { detail?: string; error?: string };
    return data.detail || data.error || `Error ${res.status}`;
  } catch (e) {
    return e instanceof Error ? e.message : "Request failed";
  }
}
