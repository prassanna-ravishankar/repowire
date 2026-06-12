"""Tests for the shared hardened tmux paste injector (repowire.tmux_inject).

These cover the bracketed-paste + Enter-swallow-retry sequence (migrated from
the old websocket_hook TestTmuxSendKeys) and the readiness-polling helper used
by the daemon's post-spawn seed.
"""

from __future__ import annotations

from subprocess import CompletedProcess
from unittest.mock import patch

from repowire.tmux_inject import inject_text, wait_for_composer_ready


def _mode_result(in_mode: bool) -> CompletedProcess:
    return CompletedProcess(
        args=[], returncode=0, stdout=("1" if in_mode else "0") + "\n", stderr=""
    )


def _ok() -> CompletedProcess:
    return CompletedProcess(args=[], returncode=0, stdout="", stderr="")


def _capture(text: str) -> CompletedProcess:
    return CompletedProcess(args=[], returncode=0, stdout=text, stderr="")


def _fake_clock():
    """A monotonic/sleep pair backed by a shared mutable clock so elapsed time
    advances deterministically by the slept amount on each sleep."""
    now = {"t": 0.0}

    def monotonic() -> float:
        return now["t"]

    def sleep(seconds: float) -> None:
        now["t"] += seconds

    return monotonic, sleep


class TestInjectText:
    """inject_text drives the hardened paste/submit sequence."""

    def test_closes_bracketed_paste_without_bare_escape(self):
        """Normal mode: no -X cancel, just literal/paste-close/Enter sequence."""
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep"),
        ):
            mock_run.side_effect = [
                _mode_result(False),  # display-message (copy-mode probe)
                _ok(),  # -l text
                _ok(),  # -H close
                _ok(),  # Enter
                _capture("❯ \n"),  # capture-pane: empty composer (submitted)
            ]
            assert inject_text("%5", "hello") is True

        calls = [call.args[0] for call in mock_run.call_args_list]
        assert calls == [
            ["tmux", "display-message", "-t", "%5", "-p", "#{pane_in_mode}"],
            ["tmux", "send-keys", "-t", "%5", "-l", "hello"],
            ["tmux", "send-keys", "-t", "%5", "-H", "1b", "5b", "32", "30", "31", "7e"],
            ["tmux", "send-keys", "-t", "%5", "Enter"],
            ["tmux", "capture-pane", "-t", "%5", "-p"],
        ]
        assert ["tmux", "send-keys", "-t", "%5", "Escape"] not in calls
        assert ["tmux", "send-keys", "-t", "%5", "-X", "cancel"] not in calls

    def test_cancels_copy_mode_before_paste(self):
        """Copy-mode: -X cancel runs before the literal paste, in that order."""
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep"),
        ):
            mock_run.side_effect = [
                _mode_result(True),  # display-message reports copy-mode
                _ok(),  # -X cancel
                _ok(),  # -l text
                _ok(),  # -H close
                _ok(),  # Enter
                _capture("❯ \n"),  # empty composer (submitted)
            ]
            assert inject_text("%5", "hello") is True

        calls = [call.args[0] for call in mock_run.call_args_list]
        assert calls == [
            ["tmux", "display-message", "-t", "%5", "-p", "#{pane_in_mode}"],
            ["tmux", "send-keys", "-t", "%5", "-X", "cancel"],
            ["tmux", "send-keys", "-t", "%5", "-l", "hello"],
            ["tmux", "send-keys", "-t", "%5", "-H", "1b", "5b", "32", "30", "31", "7e"],
            ["tmux", "send-keys", "-t", "%5", "Enter"],
            ["tmux", "capture-pane", "-t", "%5", "-p"],
        ]
        cancel_idx = calls.index(["tmux", "send-keys", "-t", "%5", "-X", "cancel"])
        paste_idx = calls.index(["tmux", "send-keys", "-t", "%5", "-l", "hello"])
        assert cancel_idx < paste_idx, "cancel must precede the literal paste"

    def test_mode_probe_failure_treated_as_not_in_mode(self):
        """If display-message fails, skip cancel rather than blocking the send."""
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep"),
        ):
            mock_run.side_effect = [
                CompletedProcess(args=[], returncode=1, stdout="", stderr="no pane"),
                _ok(),
                _ok(),
                _ok(),
                _capture("❯ \n"),
            ]
            assert inject_text("%5", "hello") is True

        calls = [call.args[0] for call in mock_run.call_args_list]
        assert ["tmux", "send-keys", "-t", "%5", "-X", "cancel"] not in calls

    def test_swallowed_enter_is_resent_once(self):
        """If the composer still holds the injected text after Enter (paste
        heuristic swallowed it as a newline), nudge with exactly one more
        Enter — but only on positive evidence."""
        composer_stuck = "❯ old prompt history\n✶ thinking\n❯ hello there friend\n"
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep"),
        ):
            mock_run.side_effect = [
                _mode_result(False),
                _ok(),  # -l text
                _ok(),  # -H close
                _ok(),  # Enter
                _capture(composer_stuck),
                _ok(),  # retry Enter
            ]
            assert inject_text("%5", "hello there friend") is True

        calls = [call.args[0] for call in mock_run.call_args_list]
        assert calls.count(["tmux", "send-keys", "-t", "%5", "Enter"]) == 2

    def test_submitted_text_in_transcript_does_not_trigger_resend(self):
        """A submitted prompt stays visible in the transcript; only text in
        the bottom-most composer prompt counts as unsubmitted."""
        submitted = "❯ hello there friend\n⏺ on it\n❯ \n"
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep"),
        ):
            mock_run.side_effect = [
                _mode_result(False),
                _ok(),
                _ok(),
                _ok(),
                _capture(submitted),
            ]
            assert inject_text("%5", "hello there friend") is True

        calls = [call.args[0] for call in mock_run.call_args_list]
        assert calls.count(["tmux", "send-keys", "-t", "%5", "Enter"]) == 1

    def test_capture_failure_does_not_resend(self):
        """No retry on uncertainty: a failed capture must not nudge."""
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep"),
        ):
            mock_run.side_effect = [
                _mode_result(False),
                _ok(),
                _ok(),
                _ok(),
                CompletedProcess(args=[], returncode=1, stdout="", stderr="boom"),
            ]
            assert inject_text("%5", "hello") is True

        calls = [call.args[0] for call in mock_run.call_args_list]
        assert calls.count(["tmux", "send-keys", "-t", "%5", "Enter"]) == 1

    def test_send_failure_returns_false(self):
        """A failing tmux send-keys surfaces as False, not a swallowed success."""
        from subprocess import CalledProcessError

        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep"),
        ):
            mock_run.side_effect = [
                _mode_result(False),
                CalledProcessError(1, ["tmux"]),  # -l text fails
            ]
            assert inject_text("%5", "hello") is False


class TestWaitForComposerReady:
    """wait_for_composer_ready polls capture-pane for the composer prompt."""

    def test_returns_true_immediately_when_prompt_present(self):
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep") as mock_sleep,
        ):
            mock_run.return_value = _capture("❯ \n")
            assert wait_for_composer_ready("%5", timeout=5.0) is True
            # No need to sleep when ready on the first capture.
            mock_sleep.assert_not_called()

    def test_polls_until_prompt_appears(self):
        monotonic, sleep = _fake_clock()
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep", side_effect=sleep) as mock_sleep,
            patch("repowire.tmux_inject.time.monotonic", side_effect=monotonic),
        ):
            mock_run.side_effect = [
                _capture("booting...\n"),  # not ready yet
                _capture("still booting\n"),  # not ready yet
                _capture("❯ \n"),  # ready (glyph)
            ]
            assert wait_for_composer_ready("%5", timeout=10.0, poll=0.1) is True
            assert mock_sleep.call_count == 2  # two not-ready captures => two sleeps

    def test_returns_false_on_timeout(self):
        monotonic, sleep = _fake_clock()
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep", side_effect=sleep),
            patch("repowire.tmux_inject.time.monotonic", side_effect=monotonic),
        ):
            mock_run.return_value = _capture("never shows a prompt\n")
            # Changing/non-glyph content never matches; clock advances past the
            # deadline via the sleeps, so the wait times out. (poll=5 reaches
            # the 5s deadline in two iterations.)
            assert wait_for_composer_ready("%5", timeout=5.0, poll=5.0) is False

    def test_capture_failure_is_not_ready_keeps_polling(self):
        monotonic, sleep = _fake_clock()
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep", side_effect=sleep),
            patch("repowire.tmux_inject.time.monotonic", side_effect=monotonic),
        ):
            mock_run.side_effect = [
                CompletedProcess(args=[], returncode=1, stdout="", stderr="boom"),
                _capture("❯ \n"),
            ]
            assert wait_for_composer_ready("%5", timeout=10.0, poll=0.1) is True

    def test_stable_content_without_glyph_is_ready_after_floor(self):
        """Unknown-glyph backend: non-empty content unchanged across
        stable_polls consecutive captures counts as ready once the floor has
        elapsed (latency guard)."""
        monotonic, sleep = _fake_clock()
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep", side_effect=sleep),
            patch("repowire.tmux_inject.time.monotonic", side_effect=monotonic),
        ):
            # Same non-glyph content repeated. With poll=2s and a 5s floor, the
            # streak (>=3) is satisfied early but the floor blocks ready until
            # >=5s elapsed.
            mock_run.return_value = _capture("weird-prompt$ \n")
            assert (
                wait_for_composer_ready(
                    "%5", timeout=30.0, poll=2.0, stable_polls=3, stable_floor=5.0
                )
                is True
            )

    def test_stable_does_not_fire_before_floor(self):
        """A stable streak reached before stable_floor must NOT declare ready —
        firing at ~1s would be worse than main's blind 5s sleep. With a short
        timeout the call returns False (caller then injects as fallback)."""
        monotonic, sleep = _fake_clock()
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep", side_effect=sleep),
            patch("repowire.tmux_inject.time.monotonic", side_effect=monotonic),
        ):
            # Stable from poll 1, but timeout (3s) trips before the 5s floor, so
            # the stable signal is suppressed and the wait times out.
            mock_run.return_value = _capture("weird-prompt$ \n")
            assert (
                wait_for_composer_ready(
                    "%5", timeout=3.0, poll=1.0, stable_polls=2, stable_floor=5.0
                )
                is False
            )

    def test_changing_content_resets_stability_streak(self):
        """Content that keeps changing never trips the stable fallback; falls
        through to timeout (then the caller injects anyway)."""
        monotonic, sleep = _fake_clock()
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep", side_effect=sleep),
            patch("repowire.tmux_inject.time.monotonic", side_effect=monotonic),
        ):
            mock_run.side_effect = [
                _capture("line 1\n"),
                _capture("line 1\nline 2\n"),
                _capture("line 1\nline 2\nline 3\n"),
                _capture("line 1\nline 2\nline 3\nline 4\n"),
            ]
            assert (
                wait_for_composer_ready(
                    "%5", timeout=3.0, poll=1.0, stable_polls=3, stable_floor=5.0
                )
                is False
            )

    def test_glyph_beats_stable_fallback_and_floor(self):
        """A glyph match returns ready immediately — no floor applies, since
        glyph is positive evidence the composer exists."""
        monotonic, sleep = _fake_clock()
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep", side_effect=sleep) as mock_sleep,
            patch("repowire.tmux_inject.time.monotonic", side_effect=monotonic),
        ):
            mock_run.return_value = _capture("❯ \n")
            assert (
                wait_for_composer_ready(
                    "%5", timeout=30.0, poll=0.1, stable_polls=3, stable_floor=5.0
                )
                is True
            )
            mock_sleep.assert_not_called()  # ready on first capture, well under floor

    def test_empty_capture_does_not_count_as_stable(self):
        """Blank/whitespace-only captures reset the streak — an empty pane is
        not 'ready', it's just not booted yet."""
        monotonic, sleep = _fake_clock()
        with (
            patch("repowire.tmux_inject.subprocess.run") as mock_run,
            patch("repowire.tmux_inject.time.sleep", side_effect=sleep),
            patch("repowire.tmux_inject.time.monotonic", side_effect=monotonic),
        ):
            # Empty 3x would trip stability if blanks counted; they must not,
            # so this falls through to the glyph capture to become ready.
            mock_run.side_effect = [
                _capture("\n"),
                _capture("   \n"),
                _capture("\n"),
                _capture("❯ \n"),
            ]
            assert (
                wait_for_composer_ready(
                    "%5", timeout=30.0, poll=1.0, stable_polls=3, stable_floor=5.0
                )
                is True
            )
