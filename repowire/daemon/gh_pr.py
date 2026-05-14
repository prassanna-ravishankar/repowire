"""Best-effort `gh api` lookup for PR head SHA + state, with a small TTL cache.

We shell out to `gh` rather than re-implementing GitHub auth — `gh auth token`
is already the convention in the rest of the codebase. The result is cached
for 60 seconds keyed on the PR URL so /reviews?reviewer=... over many tracked
PRs doesn't fan out to N API calls per request.
"""

from __future__ import annotations

import asyncio
import logging
import re
import time
from dataclasses import dataclass

logger = logging.getLogger(__name__)

_CACHE_TTL_SECONDS = 60.0
_PR_URL_RE = re.compile(
    r"^https?://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/pull/(?P<number>\d+)"
)


@dataclass(frozen=True)
class PRInfo:
    """Snapshot of a PR's current state. `state` is one of: open, merged,
    closed, unknown. Unknown means we couldn't reach the API (network down,
    gh unauthenticated, PR not found, etc.) — the caller should fall back to
    last-known values."""

    head_sha: str | None
    state: str  # open | merged | closed | unknown


_cache: dict[str, tuple[float, PRInfo]] = {}
_cache_lock = asyncio.Lock()


def _parse_pr_url(pr_url: str) -> tuple[str, str, str] | None:
    m = _PR_URL_RE.match(pr_url.strip())
    if not m:
        return None
    return m.group("owner"), m.group("repo"), m.group("number")


async def _run_gh_api(path: str) -> dict | None:
    """Invoke `gh api <path>`. Returns parsed JSON or None on any failure."""
    try:
        proc = await asyncio.create_subprocess_exec(
            "gh",
            "api",
            path,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        stdout, stderr = await asyncio.wait_for(proc.communicate(), timeout=10.0)
    except (FileNotFoundError, asyncio.TimeoutError) as e:
        logger.debug("gh api %s failed: %s", path, e)
        return None
    if proc.returncode != 0:
        logger.debug("gh api %s exited %s: %s", path, proc.returncode, stderr[:200])
        return None
    try:
        import json
        return json.loads(stdout)
    except (ValueError, TypeError):
        return None


async def fetch_pr_info(pr_url: str) -> PRInfo:
    """Return the PR's current head SHA and merge/close state.

    Cached for `_CACHE_TTL_SECONDS`. On any error returns
    `PRInfo(head_sha=None, state="unknown")` so callers can fall back to
    cached/stored values.
    """
    parts = _parse_pr_url(pr_url)
    if parts is None:
        return PRInfo(head_sha=None, state="unknown")
    owner, repo, number = parts

    now = time.monotonic()
    async with _cache_lock:
        cached = _cache.get(pr_url)
        if cached and now - cached[0] < _CACHE_TTL_SECONDS:
            return cached[1]

    data = await _run_gh_api(f"repos/{owner}/{repo}/pulls/{number}")
    if data is None:
        info = PRInfo(head_sha=None, state="unknown")
    else:
        head_sha = (data.get("head") or {}).get("sha")
        if data.get("merged"):
            state = "merged"
        else:
            raw_state = data.get("state")
            state = raw_state if raw_state in ("open", "closed") else "unknown"
        info = PRInfo(head_sha=head_sha, state=state)

    async with _cache_lock:
        _cache[pr_url] = (now, info)
    return info


def _clear_cache_for_tests() -> None:
    """Test helper: drop the in-process cache."""
    _cache.clear()
