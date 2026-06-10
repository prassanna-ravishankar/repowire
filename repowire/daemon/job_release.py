"""Shared executor-release seam for tracked work.

Terminal job states must release the executor the runner acquired for the
fire (kill the per-fire pane, unregister the peer). Both the HTTP routes
(PATCH /jobs, cancel) and the fire-completion service drive terminals, so the
release/merge helpers live here rather than in either caller.
"""

from __future__ import annotations

from typing import Any

from repowire.daemon.work_store import TrackedWork, is_terminal_state


def current_attempt_id(work: TrackedWork) -> str | None:
    runner = (work.provenance or {}).get("runner") or {}
    current = runner.get("current_attempt_id")
    return current if isinstance(current, str) and current else None


def current_attempt(work: TrackedWork) -> dict[str, Any] | None:
    current = current_attempt_id(work)
    runner = (work.provenance or {}).get("runner") or {}
    for attempt in list(runner.get("attempts") or []):
        if isinstance(attempt, dict) and attempt.get("attempt_id") == current:
            return attempt
    return None


def has_release_handle(work: TrackedWork) -> bool:
    attempt = current_attempt(work)
    acquisition = (attempt or {}).get("acquisition") or {}
    return isinstance(acquisition.get("release_handle"), dict)


def merge_release_result(
    work: TrackedWork,
    release_result: dict[str, Any],
) -> dict[str, Any]:
    provenance = dict(work.provenance)
    runner = dict(provenance.get("runner") or {})
    attempts = list(runner.get("attempts") or [])
    current = runner.get("current_attempt_id")
    for attempt in attempts:
        if not isinstance(attempt, dict) or attempt.get("attempt_id") != current:
            continue
        releases = list(attempt.get("release_results") or [])
        releases.append(release_result)
        attempt["release_result"] = release_result
        attempt["release_results"] = releases[-5:]
        acquisition = dict(attempt.get("acquisition") or {})
        release_handle = acquisition.get("release_handle")
        if isinstance(release_handle, dict) and release_result.get("released_at"):
            acquisition["release_handle"] = {
                **release_handle,
                "released_at": release_result["released_at"],
            }
            attempt["acquisition"] = acquisition
        if release_result.get("reap_error"):
            attempt["reap_error"] = release_result["reap_error"]
        break
    runner["attempts"] = attempts
    runner["release_result"] = release_result
    if release_result.get("reap_error"):
        runner["reap_error"] = release_result["reap_error"]
    provenance["runner"] = runner
    return provenance


async def release_executor_if_needed(
    work: TrackedWork,
    *,
    work_store: Any,
    session_control: Any | None,
    terminal_reason: str,
    attempt_id: str | None,
) -> TrackedWork:
    """Release the acquired executor for terminal work; idempotent."""
    if not is_terminal_state(work.state) or not has_release_handle(work):
        return work
    if session_control is None or not hasattr(session_control, "release_executor_for_work"):
        return work
    release_result = await session_control.release_executor_for_work(
        work,
        terminal_reason=terminal_reason,
    )
    provenance = merge_release_result(work, release_result)
    refreshed = work_store.update_state(
        work.work_id,
        state=work.state,
        state_reason=work.state_reason,
        phase=work.phase,
        progress=work.progress,
        result_summary=work.result_summary,
        result_data=work.result_data,
        error=work.error,
        artifacts=work.artifacts,
        provenance=provenance,
        attempt_id=attempt_id,
    )
    return refreshed or work
