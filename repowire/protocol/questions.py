"""Structured mesh questions — the shared semantic primitive.

A ``Question`` rides on an ask; an ``Answer`` rides on the ack. This turns three
things into one primitive at points on a spectrum:

  * plain ask        — no question (legacy) or ``Question(kind="acknowledge")``
  * tool approval     — ``Question(kind="choice", options=[allow, deny],
                        blocking=True, default_answer=deny)``
  * AskUserQuestion   — ``Question(kind="choice", options=[...N...])``

The load-bearing boundary (see ``docs``/design notes): ``Question``/``Answer`` is
the shared *semantic* primitive, but **blocking is a transport capability, not a
property of every ask**. ``Question.blocking`` is what the origin *requests*;
whether a wait actually happens depends on the transport binding (ACP and a
PreToolUse hook own a suspendable call; an MCP ask from an already-running agent
does not — its answer arrives on a later turn). Bindings must never silently
pretend a non-blocking origin blocked.

The option shape (``id``/``title``/``description``) matches both ACP's
``RequestPermission`` options and the harness ``AskUserQuestion`` tool's
``{label, description}`` — that alignment is why the unification is natural.
"""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, Field

QuestionKind = Literal["acknowledge", "choice", "text"]
"""How the question is answered: a bare ack, one of N options, or free text."""

QuestionScope = Literal["mesh_ask", "tool_permission", "user_question"]
"""Where the question originated — diagnostic/routing hint, not behavior."""

AnswerOutcome = Literal["answered", "acknowledged", "timed_out", "cancelled"]


class QuestionOption(BaseModel):
    """One choice for a ``kind="choice"`` question."""

    id: str = Field(..., description="Stable option id echoed back in the Answer")
    title: str = Field(..., description="Human-facing label")
    description: str | None = Field(None, description="Optional longer explanation")


class Answer(BaseModel):
    """The typed resolution of a question (rides on the ack)."""

    option_id: str | None = Field(None, description="Selected option id for a choice question")
    text: str | None = Field(None, description="Free-text answer / reply body")
    outcome: AnswerOutcome = Field(
        "answered",
        description="answered (option/text given), acknowledged (bare), timed_out, cancelled",
    )
    message: str | None = Field(None, description="Optional human note alongside the answer")


class Question(BaseModel):
    """A structured question carried on an ask.

    ``Ask.text`` stays the canonical human body; ``prompt`` only overrides when
    the rendered question differs from the ask text.
    """

    kind: QuestionKind = Field(..., description="acknowledge | choice | text")
    prompt: str | None = Field(None, description="Optional rendered prompt; defaults to ask.text")
    options: list[QuestionOption] = Field(
        default_factory=list, description="Choices for kind='choice'"
    )
    blocking: bool = Field(
        False,
        description=(
            "REQUESTED semantics: does the origin want to suspend until answered? "
            "Honored only by transports that own a suspendable call (ACP, "
            "PreToolUse hook). Non-blocking origins must reject or downgrade with "
            "explicit metadata, never silently fake a block."
        ),
    )
    timeout_seconds: float | None = Field(
        None, description="How long a blocking transport waits before applying default_answer"
    )
    default_answer: Answer | None = Field(
        None, description="Applied on timeout (e.g. deny for an approval)"
    )
    scope: QuestionScope | None = Field(None, description="Origin hint (diagnostic/routing)")
    metadata: dict[str, Any] = Field(default_factory=dict)

    def option_ids(self) -> set[str]:
        return {opt.id for opt in self.options}

    def validate_answer(self, answer: Answer) -> str | None:
        """Return an error string if ``answer`` is invalid for this question, else None.

        Enforced in the daemon (not just the UI): a choice must pick a known
        option (unless it's a timeout/cancel applying the default); text/ack are
        permissive. First-answer-wins + lifecycle is the tracker's job.
        """
        if answer.outcome in ("timed_out", "cancelled"):
            return None
        if self.kind == "choice":
            if answer.option_id is None:
                return "choice question requires an option_id"
            if answer.option_id not in self.option_ids():
                return f"unknown option_id: {answer.option_id}"
        return None
