---
title: Jev router — paper skeleton
created: 2026-10-11
updated: 2026-10-12
status: draft
tags:
  - project/repowire
  - research
  - routing
  - writing
---

# Routing to Stateful Agents

Working title. Companion to [[jev-router-related-work-2026-10]]. Nothing below is written until the data says something; this is the shape the argument has to fit.

## Proposed abstract (converged 2026-10-13, for Prasanna's pick)

*Working title: Mostly Habit: What Routing Among Stateful Agents Actually Needs*

Routers and delegation benchmarks score every decision alike. Among long-lived agents that is the wrong unit: in three months of a production agent mesh, two messages in three go to the peer the sender spoke to last, and a recency rule or a per-sender habit table is right most of the time with no model at all. We reconstruct 908 routing decisions from the mesh's event journal, add a live shadow track on current traffic, and score a System One classifier fed attested agent state (liveness, turn activity, inbound load, project) against chance, recency, habit and a one-line "most recently active peer" rule, conditioning every number on whether the decision was a context switch and reporting chance analytically beside candidate-set size. The classifier is far above chance and above recency at switches; pooled across senders it also beats a per-sender habit table there (p = .012), but on the orchestrator's own switches, the population a live deployment would see, it does not (p = .85), and over all decisions a one-line recency rule is as good as the best hybrid we can build. Masking every activity-derived field, sender included, shows that most of its score on continuation decisions is activity recency; what remains at switches, about four times chance, is fit between message and peer beyond those fields, though the candidate set itself is still selected by activity. Where history is empty, on switches to peers that just appeared, it routes at four to six times chance while habit scores zero by construction, but so does a rule that picks any novel peer: the cold-start problem is detecting that a novel peer is the target, not choosing it. A detector exists in state: a peer that appeared in the last ten minutes predicts a switch to it with 54% precision in the historical data (a threshold chosen there, now pre-registered for live validation), and the design "habit, unless someone just arrived, then state" beats habit at switches and ties the best single arm, with the model behind the trigger contributing a few points. Habit is wrong on six to seven switches in ten, and most switches are among known peers, where recency is wrong by definition, novelty is silent, and only fit between message and peer does anything. The open problem in routing among stateful agents is therefore not choosing a peer but detecting when habit is wrong; the classifier's own confidence cannot, and arrival time can for about a fifth of switches. A stateless benchmark shows none of this structure (22% of delegations repeat the previous delegate against about 60% in the mesh), so repeat rate is the diagnostic for which regime a routing problem is in. We release the reconstruction method, the harness and the switch-conditioned metric.

> Pending before submission: codex round 3 on #461 (arithmetic of §6e.3); the paired Jev-vs-habit test on the corrected switches, which decides "ties" vs "beats"; Clef as the second classifier; the frontier arm (needs a key) for the claimed-state comparison the §1 framing still promises. If hybrid-v2 beats habit at switches, the last sentence of the findings softens to "detecting when habit is wrong, which state can do when a fresh peer is in the room".

## Working abstract (superseded 2026-10-12, kept for the record)

Existing benchmarks for LLM routing and delegation evaluate routers that choose among stateless workers with fixed capability profiles. We study a different setting: routing messages among pre-existing, long-lived coding agents whose descriptions decay, who are mid-task, who hold context a successor would have to rebuild, and who die. Using a production agent mesh with months of explicitly-addressed traffic as ground truth, we show (1) that routing fidelity affects end-task cost far more in the stateful setting than prior work found in the stateless one, (2) that a small classifier fed attested agent state outperforms a frontier model prompted with self-described state, and (3) that the failure modes differ in kind: staleness, not misrepresentation, dominates. We release the logging schema and a static replay harness.

> Every claim above is a hypothesis until measured. Rewrite from the results, not toward them.

## 1. Introduction

- The thing people build: a mesh of coding agents across projects and machines. Senders must name the recipient exactly. Memory records the cost of that: display-name collisions, stale descriptions, peers addressed after they died.
- The obvious fix: let a model route. The obvious benchmark: DecisionBench, MasRouter, Uno-Orchestra, TwinRouterBench.
- The problem with the obvious benchmark: every one of them routes to stateless workers. The peer pool is a set of models with static cards. None has context, history, a current task, or a death.
- Contribution, stated narrowly. Not "LLM routing works."

## 2. Related work

Drawn from [[jev-router-related-work-2026-10]]. Three lineages:

- **Model routing** — RouterBench, LLMRouterBench, TwinRouterBench. Which LLM answers a query. Cost/quality tradeoff. Workers interchangeable by construction.
- **MAS construction** — MasRouter, Uno-Orchestra. Build the agent system per task: decomposition, roles, model per role. Agents do not pre-exist.
- **Delegation among peers** — DecisionBench. Closest. Orchestrator delegates subtasks to a pool of models with profile cards. Fidelity@k. Finding to build on: fidelity doubled, quality flat.
- **Trust in self-report** — Provenance Paradox. Claimed vs attested identity. Their failure is lying; ours is staleness.

Gap sentence, verbatim from the related-work note: every benchmark routes to stateless workers chosen from a pool with static profiles.

## 3. Setting

- Repowire: peers, circles, asks/notifies, durable jobs. Backends: Claude Code (hooks), Codex (native App Server), Pi.
- What a peer carries. **Claimed:** description (free text, self-set, decays). **Attested:** turn_state, last_seen, path, project, backend, addressable, parent. Table.
- Why "right peer" is not "best peer at the skill": the peer already holding the worktree and the thread is right even if another is more capable. Context rebuild cost. This is the mechanism the stateless benchmarks cannot see.
- Ground truth: senders name a target today. Months of explicitly-addressed asks. Caveat: assumes the human was right (§7).

## 4. Method

- Logging schema (deliverable 1). Candidate set with attested state snapshotted at decision time.
- Arms: explicit · random · frontier+descriptions · jev-claimed · jev-attested · jev-both.
- Fail-closed contract. Routing decision visible in delivery.
- Jev as classifier: fixed question template, descriptions as data not instructions, probabilities out.

## 5. Experiments

- **Static track:** replay logged asks, every arm, fidelity@1 / @3 against declared target. Thousands of asks, no API cost beyond router calls.
- **Dynamic track — shadow (primary):** every explicit ask runs all configured arms, delivers to the declared target, logs what each arm would have picked. Ground truth is the human's real-time choice with real peer state. Zero misroute risk, so it runs on all traffic. Frontier arm sampled (every Nth, N logged) for cost. Metrics: live fidelity@k per arm, latency.
- **Dynamic track — live (opt-in):** only asks addressed to the reserved `@route` target are actually routed. This is where misroute cost is measured: turns to redirect, declines, same-text re-sends to a different peer within 30 min. Small n by design.
- Why this beats per-ask arm randomisation: randomisation would misroute real work to measure fidelity. Shadow measures fidelity for free and reserves real misrouting for an explicit opt-in. The schema is in [[jev-router-logging-schema]].
- **Stateless sanity check:** Jev on DecisionBench profile cards. If it cannot route stateless cards, nothing above is credible.
- **Ablation that carries the paper:** jev-claimed vs jev-attested. If attested wins, the Provenance Paradox framing holds and staleness is the mechanism. If claimed wins, the paper is wrong and that is the finding.

## 6. Results

Empty until data.

## 6a. What the live ledger says about scale (2026-10-11)

Read from `~/.repowire/state.db`, read-only, last 30 days: **107 explicit asks, ~3.6/day, ~100 labelled asks per month.** Zero `ask_many` children. The dataset is volume-limited, not cost-limited — all three frontier arms on every ask is ~$10/month.

Consequence: a credible static track needs **three months of recording minimum** before the per-arm comparison has power. The paper's timeline is set by this, not by implementation.

**t = 0: 2026-10-11.** PR #417 merged as `d15ded3`, daemon restarted on `v0.21.0-dev.a4037e9`, `experiments.router.record: true` live. Every explicit ask from this date is a labelled example. Earliest credible static-track read: **2027-01-11**. Comfortable: April 2027.

Also from the ledger: `delivery_traces` is **not pruned** despite `docs/reference/cli.md` saying so (fixed). Rows go back to 2026-08-10. But those rows have no candidate snapshots, so they cannot be replayed — there is no retroactive dataset. **Revised 2026-10-12: see §6d. The `events` table does allow a reconstructed attested-only dataset back to 2026-05-22.**

**Baselines for the results table:** random, and **majority-class**. On DecisionBench's aware-c2/gemini-3-flash GAIA run the orchestrator sent 37% of delegations to one model, so majority-class is 0.37 and random is a weak floor. Report both. On the mesh, expect majority-class to be high too — the orchestrator addresses a few peers most of the time.

**DecisionBench caveat for §5:** their dominant-skill fidelity label cannot be reconstructed from released code. The adapter measures **agreement with the orchestrator's pick**, not fidelity to skill. Pipeline validated: random scored f@1 0.089 / f@3 0.316 against chance 0.10 / 0.30 on 158 delegations.

## 6b. Early-data caveats (from the 2026-10-11 smoke test)

Attested features are thin at the start and after every daemon restart. Exclude or flag the affected rows in the static track:

- `description_set_at` is null for any description set before schema v15; it stamps on the peer's next `set_description`. So `description_age_s` is empty until each peer touches its description once.
- `last_exchange_with_sender_at` lives in the in-memory `AskTracker`, which a restart wipes. Refills as asks flow.
- `turn_state` is empty for every claude-code candidate until a hook fires after re-registration; codex peers report `idle` immediately. First-turn-after-bounce rows see a degraded attested view.
- `telegram` (the human's relay) is in the candidate set under the access rules. Whether any arm ranks the human is a finding. It is also a gate on live mode: routing to `@telegram` delivers to a phone. Decide before live is enabled.
- First real Jev call returned 402 (no TypeSafe credits). The fallback row carried model and template identity; the breaker then bounded probes to one per 30s. Until credits exist, jev-* shadow rows are `planned_not_run:breaker_open` and the control arm is unaffected. Follow-up: classify 402 as `billing`, not generic `error`.

## 6c. First scored rows (2026-10-11 ~18:10) and a circularity threat

Credits added; breaker probe succeeded; Jev scoring live. 371–455ms per call over 11 candidates, network-bound (402 round trips took the same). First two scored asks both ranked the declared target first (0.37, 0.63). Jev ranked `telegram` 10th of 11 — not drawn to the human.

**Threat to the central claim, found on the first two rows:** both hits were "orchestrator → the peer it had just been talking to." The attested features `last_exchange_with_sender_at` and `open_asks_inbound` encode exactly that, so jev-attested agreeing with the orchestrator may be **recency, not routing**. The control labels are the orchestrator's own choices, which are themselves recency-driven. Agreement could be circular.

What the analysis must do about it:
- Report fidelity **stratified by whether the declared target was the sender's most recent contact.** The interesting rows are the ones where the human chose someone *other* than the recent peer — did jev-attested follow the human or the recency prior?
- Add an ablation arm, or a replay view, with `last_exchange_with_sender_at` and `open_asks_inbound` **masked**. If fidelity collapses, the attested advantage was recency. If it holds, it was state.
- A trivial "most-recent-contact" baseline belongs in the results table beside random and majority-class. It may be hard to beat.

This is cheap to add now (replay can mask fields) and expensive to discover at write-up.

**Built the same day (PR #418):** `--arm recent`, `--arm majority` (leave-one-out, documented as a ceiling for habit not a causal predictor), `--mask f1,f2` reaching the Jev and frontier request bodies, and a three-way recency stratum on every `--rows` line: `declared_recent_contact` / `declared_other` / `no_exchange_data`. The third exists because restart-wiped `AskTracker` rows carry no exchange data and say nothing about recency.

**First row of the kind that matters (ask-1761e245, 2026-10-11):** the orchestrator's most recent contact was `telegram`; the `recent` baseline therefore ranked the human first. The orchestrator addressed the jev-router peer; jev-attested ranked that peer first at 0.63 and `telegram` 10th of 11. The only Jev row that went against recency agreed with the human. n=1 — record it, do not lean on it.

Harness on n=8: random f@1 0.25, recent 0.375, majority 0.875, jev-attested 0.50 (two 402 fallbacks counted as misses; 1.00 on answered). Majority sits near oracle because nearly every ask is orchestrator↔one peer — that is why it belongs in the table.

**Methodology note from codex review of #418:** the `majority` baseline is whole-window leave-one-out — asks *after* the scored one vote too. That is valid as the disclosed retrospective habit control, and "ceiling" is descriptive, not a statistical bound. **If the paper claims improvement over a deployable history-based predictor, it needs a past-only, time-ordered `majority` variant** (and the same for `recent`, which already is past-only by construction). Add before the static-track analysis. Code comment in `routing/baselines.go:88-98` should mirror the docs caveat at `cli.md:192`; follow-up PR.

**Done in PR #419** (`--arm majority-past`: only asks decided strictly before the scored one vote). On the same n=8: `majority` 0.875, `majority-past` 0.625, `jev-attested` 0.500 recorded. **The 0.25 gap is the future leaking into the whole-window baseline** — a direct demonstration that the causal variant was required. Report both in the results table; `majority-past` is the deployable competitor, `majority` is the retrospective ceiling. #419 also classifies 402 → `billing` and 401/403 → `unauthorized` for both providers (daemon rebuild needed before new rows carry them).

**Failure attribution (resolved in #420, verified 2026-10-11, pending codex close):** `error` is split into `request_error` (4xx-other, our bug, does **not** trip the provider breaker), `server_error` (5xx), `billing` (402, plus Anthropic's documented spend-limit 400 and `enforced_spend_limit_reached` 429), `unauthorized` (401/403), `rate_limited`, `timeout`, `unparseable`. Export carries `fallback_status` and `fallback_provider` as structured fields; for rows recorded before #420 they are parsed from the stored text, so **legacy rows are attributable after all** — no exclusion needed, but state the parse in the methods. **The ledger stores no provider error body** — ever, in either `record_text` setting. Codex bypassed a regex scrubber twice with adversarial tests (URL-encoded and `\u`-escaped key; a padded body that truncated the key mid-literal before the clip), so the design moved to structured fields only: `http_status`, `provider`, `fallback_reason`, and `error_code` from an **allowlist** of provider-documented codes (anything else → `other`). Free-text provider messages go to the daemon log at debug level, which is not the research dataset. `record_text` governs only the ask text in `route_decision` rows. For the paper: attribution rests on status + reason + allowlisted code, which is sufficient and cannot carry secrets. Also found while hardening tests: the Anthropic SDK reports a context cancelled *after* an error response as bare "context canceled," which would have recorded a 402 as `timeout`; fixed by capturing error responses in SDK middleware. Also: `--since`/`--limit` truncate the **vote population** for `majority-past`, so a windowed replay has a cold start — disclose the training window or preserve pre-window history.

## 6d. What 1.2 days of shadow data already say (2026-10-12)

Read from the ledger at 23:30: 51 decisions, 49 shadow rows, one arm (jev-attested). Too small to publish, large enough to change the design.

**Stratify hub-bound traffic out.** 30 of 49 scored decisions have the orchestrator as the declared target (Telegram relay to orchestrator, peer replies to orchestrator). Those have one answer and are not a routing problem. The routing problem is the 19 orchestrator-to-worker asks.

| stratum | n | jev-attested f@1 | f@3 |
|---|---|---|---|
| all | 47 | 0.45 | 0.81 |
| hub-bound | 28 | 0.50 | 0.86 |
| orchestrator to worker | 19 | 0.37 | 0.74 |

Random on 11 to 16 candidates: about 0.08 / 0.23. Two rows were fallbacks and count as misses.

**The recency split on the 19 worker-bound asks.** This is the stratum §6c asked for.

| | n | jev f@1 | jev f@3 | `recent` f@1 |
|---|---|---|---|---|
| target = sender's previous target | 14 | 0.29 | 0.71 | 1.00 |
| target is a switch | 5 | 0.60 | 0.80 | 0.00 |

Reading: a router's value is concentrated at context switches, where every history baseline scores zero by construction. The paper's question is "does the router beat recency at switches", not "does the router route". Thirteen of the 19 went to one peer tonight, so `majority-past` is also strong on this window; traffic is workstream-concentrated, which is the normal shape of a single-user mesh.

**Negative finding.** Jev's mean top confidence is 0.65 on the recency-hit rows and 0.67 on the switch rows. Its confidence does not separate the two regimes, so a hybrid that trusts recency unless the router is confident has nothing to key on. A switch detector would have to come from state (sender's last target against the candidates), not from the router's score.

**Ground-truth provenance.** The human's asks are hub-bound in both epochs: the Telegram relay sent 24 of 51 here, and `telegram-claude-code` sent 543 of 2,545 historically, all to an orchestrator. The worker-bound labels are the orchestrator LLM's addressing choices. The measured quantity is agreement with an orchestrator's explicit addressing; say "orchestrator-consistent routing" rather than imply human intent. The human never routes to workers directly in this mesh.

**Staleness is not visible in a tended mesh.** Tonight the median decision had 0 candidates with a description older than an hour (max 3). Claim (3), that staleness dominates, cannot be tested on an evening when every peer is being actively driven. It needs the historical epoch, where many peers ran untended for days.

**A retroactive dataset exists after all.** §6a said no, because `delivery_traces` rows before 2026-10-11 carry no snapshots. But the `events` table holds 2,545 `ask` events from 2026-05-22 to 2026-08-10 with `from`, `to`, `to_peer_id`, text and timestamp, plus `peer_online` (3,333), `peer_offline` (789), `peer_status` (3,516), `chat_turn` (8,009) and `ack` (736). From those the attested view can be rebuilt at each ask's timestamp: who was online, last seen, turn activity, open inbound asks, last exchange with the sender, project and backend from the peer name. Descriptions were not logged, so the claimed view cannot be rebuilt; the historical track is attested-minus-description, which is the cleaner ablation anyway. Senders `jobs` (1,426) and `daemon-job-runner` (79) are machine dispatch and are excluded; Telegram (543) is hub-bound. Roughly 500 orchestrator-to-worker asks remain across 170 distinct targets, about 25x the current n, in a different peer population (moltbook drivers, daily brief, mesh health). Assigned 2026-10-12 as a reconstruction harness.

**Arms live from 2026-10-12 23:40:** jev-claimed, jev-attested, jev-both, clef-flash-attested, every ask, daemon v0.21.0-dev.2bad1d4d. The shadow pool is 4 global, so a fifth arm would record `saturated` rows; raising it is a one-line change to bundle with the frontier arms once a key exists. Frontier is the remaining blocker on claim (2)'s other half; at `sample_every: 2` it is about $5 a month.

First ask under the new daemon (22:39Z, hub-bound): jev-attested 0.47, jev-both 0.21, jev-claimed 0.11; clef-flash fell back with Cloudflare error 4006, daily free allocation exhausted. The cap probes earlier that evening cost about 1,400 neurons each at the 64,000-token cap, and seven of them used the 10,000-neuron day. Routine calls cost about 14. The allocation resets at 00:00 UTC; the row is misclassified as `rate_limited`, see the 4006 issue. Clef rows for 2026-10-12 after 22:39Z are therefore absent, not failed.

**Still unmeasured: misroute cost.** Claim (1) needs real routing, which needs the live track, which nobody has used. The `@route` reserved target exists. Proposal for the user's decision: the orchestrator addresses its low-stakes asks to `@route` for a defined period, so misroute cost (turns to redirect, declines, same-text re-sends within 30 minutes) accrues on real work with a bounded blast radius.

## 6e. The historical track: 908 reconstructed asks (2026-10-12, PR #461 draft)

Reconstructed from the `events` table on a backup copy of state.db, 2026-05-22 to 2026-08-10. Baselines only so far; paid re-runs authorised the same night.

**Caveat that governs everything below:** `peer_online` and `peer_status` events begin 2026-08-11, after the last historical ask. Liveness is therefore inferred from activity (24h window, minus offline/reaped). Candidate sets are a lower bound. The window is a flag; the habit baselines barely move with it, random does (see below).

**Counts.** 2,545 asks. Excluded: machine sender 1,505, self_target 33, service target 6. Target not online at t, kept apart and unscored: 93 (75 no prior evidence, mostly fresh spawns; 9 offline after last activity; 9 stale). Scorable: **908**, of which hub-bound 353, worker-bound 555. Worker strata: declared_recent_contact 316, declared_other (switches) **200**, no_exchange_data 39. Candidates per row: median 12, p75 19, p90 35, max 110, mean 16.5. Distinct targets 123 by peer_id, 75 by display name (per-run job peers reuse names).

**Baselines, f@1 / f@3.** Fallbacks count as misses. majority-past votes from all 1,001 decisions including the 93 unscored.

| set | n | random | recent | majority | majority-past |
|---|---|---|---|---|---|
| all | 908 | .117 / .289 | .640 / .807 | .739 / .873 | .627 / .763 |
| worker-bound | 555 | .141 / .326 | .569 / .786 | .658 / .843 | .560 / .715 |
| worker, recent contact | 316 | .203 / .411 | 1.00 / 1.00 | .797 / .956 | .766 / .921 |
| worker, **switch** | 200 | .050 / .200 | 0.00 / .600 | .475 / .735 | **.345 / .530** |
| worker, no exchange data | 39 | .103 / .282 | 0 | .462 / .487 | 0 |

**What this corrects in §6d.** At switches `recent` is zero by construction, as claimed. Habit is not: a switch usually returns to a peer the sender talks to often, so `majority-past`, the deployable competitor, still gets 34.5% @1 and 53% @3 at 200 switches. The bar for "only the switches matter" is therefore about 35% @1, not 0. The live n=5 result (jev 60%) would need to hold against that on this population.

**Candidate-set size is a confound for random, not for habit.** Sweeping the liveness window from 1h to 720h moves mean candidates from 3.8 to 139; random @1 goes from .43 to .02 while worker `recent` moves .635 to .564 and `majority-past` .639 to .560. Report every arm beside its candidate count, bucketed; a pooled number hides the comparison.

**Fresh spawns inflate the switch set.** Per-run job peers are ephemeral ids, which habit cannot predict. That is not noise: a switch to a peer that just appeared is exactly where attested state (just online, zero exchanges, matching project) should see most and habit sees nothing. Split requested: switch-to-fresh vs switch-to-existing, per arm.

**Re-runs authorised (about $0.60 of Jev, Clef after the daily reset):** jev-attested, jev-both, jev-claimed as an empty-description control, and jev-attested with `--mask last_exchange_with_sender_at,open_asks_inbound`. The masked run is the ablation that carries the paper: if masked attested still beats majority-past at the 200 switches, the signal is state; if it collapses, attested routing was recency in a classifier, and that is the finding. clef-flash-attested on the 555 worker-bound rows, with the expected state_too_large rate on large candidate sets reported as a bias.

Harness changes from the night: paid arms are opt-in (`--allow-paid`), `--arm all` means free arms, replay prints per-provider calls and input tokens on exit. Codex review of #461 opened the same night, targeting the liveness inference, exchange TTL and stratum definitions.

### 6e.1 Jev on the historical track (2026-10-12, 4 arms x 908 rows, 3,632 calls, 15.9M input tokens, about $0.67)

f@1 / f@3. Fallbacks count as misses; every Jev arm had 0 fallbacks. Mean candidates: 16.5 overall, 12.4 recent-contact, 21.2 switch, 26.4 no-exchange.

| arm | all 908 | worker 555 | recent contact 316 | **switch 200** | no exchange 39 |
|---|---|---|---|---|---|
| random | .117/.289 | .141/.326 | .203/.411 | .050/.200 | .103/.282 |
| recent | .640/.807 | .569/.786 | 1.00/1.00 | 0/.600 | fb |
| majority | .739/.873 | .658/.843 | .797/.956 | .475/.735 | .462/.487 |
| majority-past | .627/.763 | .560/.715 | .766/.921 | **.345/.530** | 0 (fb 38) |
| jev-attested | .444/.776 | .575/.829 | .715/.896 | **.390/.755** | .385/.667 |
| jev-both | .461/.794 | .586/.843 | .734/.918 | .390/.755 | .385/.692 |
| jev-claimed (empty descriptions) | .110/.289 | .139/.292 | .142/.316 | .125/.265 | .179/.231 |
| jev-attested, masked* | .412/.764 | .533/.818 | .642/.867 | .390/.770 | .385/.667 |

*masked = last_exchange_with_sender_at and open_asks_inbound removed from the request.

**Controls hold.** jev-claimed equals random (.139 vs .141 on worker rows): nothing leaks through empty descriptions. jev-both tracks jev-attested within 1pp (the prompt differs by two empty fields).

**The strong angle fails.** At the 200 switches, jev-attested .390 against majority-past .345 is statistically indistinguishable: paired, Jev-only 44 rows, habit-only 35, sign test p = .37 (Jev 95% CI about +-6.8pp). On the 176 switches where habit answers, habit .392 vs Jev .409. Against the retrospective ceiling (.475): Jev-only 35, majority-only 52, p = .086. "Beats recency" is true and trivial (0 vs .39); "beats habit" has no support on this data.

**The ablation as run is uninformative at switches, and says why.** Masking the two exchange-derived fields leaves switch f@1 at .390 exactly (11 rows flip each way, p = 1.0); it only hurts where sender-recency applies (recent-contact .715 to .642). So at switches Jev does not use those fields. But last_seen_age_s and turn_state are derived from the same journal activity and survive the mask, so this does not yet separate "state" from "activity recency". Stricter mask authorised (also last_seen, last_seen_age_s, turn_state). If switch f@1 holds, Jev has signal beyond activity (project and backend match). If it collapses toward .125, attested routing at switches is activity recency, which is itself attested state and still needs no history.

**Where Jev wins: history is empty.** Switch-to-fresh (target had no prior exchange with anyone, n = 22) vs switch-to-existing (178):

| arm | fresh 22 | existing 178 |
|---|---|---|
| random | .045/.227 | .051/.197 |
| majority-past | 0 (fb 3) | .388/.596 |
| majority | .091/.455 | .522/.770 |
| jev-attested | **.545/.818** | .371/.747 |
| jev-claimed | 0/.227 | .140/.270 |

12 of 22 for Jev where habit gets 0 by construction and the no-state control gets 0: this is attested state (just online, project match) doing what no history can. n = 22, CI about +-20pp: suggestive. House-bias check pending: a one-line "newest-online peer" rule might reproduce it. If it does, the paper reports a classifier matching a trivial rule, not beating one.

**By candidate count (worker rows, f@1):**

| bucket | n | random | recent | majority-past | majority | jev-attested |
|---|---|---|---|---|---|---|
| 1-5 | 82 | .427 | .805 | .817 | .902 | .805 |
| 6-12 | 211 | .128 | .611 | .649 | .777 | .687 |
| 13-25 | 153 | .092 | .588 | .510 | .569 | .405 |
| 26+ | 109 | .018 | .284 | .266 | .367 | .422 |

Jev is at or below habit up to 25 candidates and ahead only at 26+, where habit is degraded by fallbacks. Jev's f@1 falls from .80 to .42 with set size, tracking random's collapse, yet stays far above random everywhere (26+: .42 vs .02): real signal, scale-sensitive. The 24h liveness window makes the small buckets the likelier undercounted ones.

**The angle, revised by the data.** Not "a router beats habit at switches". It does not. The defensible claim is cold start: attested state matches a habit table with zero history, and beats it wherever history is empty (fresh spawns, no exchange data, habit fallbacks). The practical design is two-tier, habit when it has a vote and state when it does not; a `habit-then-state` hybrid arm is being scored from the recorded rows. The switch-conditioned metric stays as the methodological contribution: uniform fidelity is inflated by recency, and the deployable competitor at switches is habit at about a third, not zero.

**Codex review of #461 (same night), four corrections that apply to every table above:**
- *Exchange definition.* The harness counted `notification` events as exchanges; the live #418 tracker does not. Removing them reclassifies 37 of the 200 switches as recent-contact and 5 as no-data: 171 switches remain, and majority-past on them is .251/.468. Of the 37, the youngest-activity rule gets 20 @1 and 35 @3: they were easy rows, inflating both habit and Jev at switches. All strata are being recomputed to match the daemon's AskTracker event for event (also: live expires by ask creation, a bare ack need not update RepliedAt). The stratum classifier itself matches #418 exactly; only the exchange population differed. Numbers above are the pre-correction run.
- *Activity is a baseline, and it is strong at @3.* Youngest `last_seen_age_s` alone scores switches .220/.760; target-age AUROC for recent vs other .693 (median age 118s for recent targets, 551s for switches). Its f@3 equals jev-attested's (.755): at @3 Jev is "the three most recently active peers". Only @1 separates them (.390 vs .220). The two-field mask therefore isolates nothing; the strict mask (also last_seen, last_seen_age_s, turn_state) and a `newest` baseline are required before any "state, not recency" sentence.
- *Expected random.* The single seeded draw (.050/.200 at switches) is below the analytic expectation (.0867/.2550). Report the expectation; lift over chance was overstated.
- *Sender type.* Only 97/555 worker rows and 64/200 switches have an orchestrator-prefixed sender; "worker-bound" is a target-only split and most of the historical traffic is peer-to-peer. The live track is orchestrator-sender. Every table gets a sender_type stratum, and the historical-vs-live comparison is made on the orchestrator-sender subset. 195/200 switches are May/June: one epoch, one population.
- Also: the 75 "no prior evidence" targets cannot be rescued (spawn_seed_delivered begins 2026-10-07; 59 have no prior event at all). 59 of the 75 act within five minutes after the ask, so they were very likely fresh spawns asked at birth, but future activity must not be used as an as-of-t repair. Excluding them biases *against* the cold-start finding; the limitation should say so. open_asks_inbound is bounded by a 24h TTL but correlates .342 with majority-top membership; masked in the strict run.

### 6e.2 Corrected strata and the strict mask (2026-10-13, #461 after codex round 1)

Exchange semantics now match the daemon's AskTracker (asks only; counted at creation or first ack with a message; 24h expiry by creation; self-asks inbound; restart wipes unreconstructable). Chance is analytic per row. Worker-bound 555 by sender: orchestrator 97, peer 243, human relay 215. Worker strata: recent-contact 333, **switches 169**, no-exchange 53. 165 of 169 switches are May/June. f@1 unless marked; Wilson 95% CIs in brackets. "OLD" Jev rows were scored on the pre-correction exchange values; a rerun on corrected values is authorised (~$0.37). Masked rows are unaffected.

| set (chance @1) | recent | newest | majority | majority-past | jev-attested OLD | jev strict* | jev-claimed |
|---|---|---|---|---|---|---|---|
| worker 555 (.134) | .600 | .571 | .658 | .560 | .575 [.53-.62] | .288 [.25-.33] | .139 |
| recent-contact 333 (.170) | 1.0 | .793 | | .805 | .700 | .285 | |
| **switches 169 (.072)** | 0 | .195 (@3 .740) | .396 | **.254** (@3 .467) | **.385** [.31-.46] (@3 .746) | **.296** [.23-.37] (@3 .627) | .095 |
| switch, fresh target 29 | | .276 | .069 | 0 | .448 [.28-.62] | .414 [.26-.59] | 0 |
| switch, existing 140 | | .179 | .464 | .307 | .371 | .271 | |
| no-exchange 53 | | .377 | | 0 | .396 | .283 | |
| habit has no vote 76 (.112) | | .382 | | 0 | .382 [.28-.49] | .329 [.23-.44] | .158 |
| **orchestrator-sender 97 (.060)** | .289 | .258 | .598 | **.454** | .402 [.31-.50] | .196 | |
| orch. switches 62 | | .048 | .500 | .339 | .371 | .242 | |
| orch. fresh 10 | | .10 | | 0 | .50 | .50 | |
| peer-sender 243 | | .613 | | .498 | .617 | .477 | |
| human-relay sender 215 | | .665 | | .679 | .605 | .116 | |
| all 908 (.115) | | .578 (@3 .888) | .739 | .627 | .444 | .192 | .110 |

*strict = also hides last_seen, last_seen_age_s, turn_state (every activity-derived field).

> **Withdrawn pending rerun (codex round 2, 2026-10-13):** `Mask()` left the sender's `open_asks_inbound`, `last_seen_age_s` and `turn_state` in every request. The strict column above was scored with sender activity present and, through the sender's open-ask count, on pre-correction exchange values (74/1,001 bodies differ; 36/169 switches; 13 of the 50 strict hits). The mask is being extended to the sender block with a body-level test, and strict is being rerun (~$0.20). Until then the fit-vs-activity sentence below is a hypothesis, not a result.

**The strict mask is the instrument.** It drops Jev from .575 to .288 on worker rows and from .700 to .285 on continuation rows: most of what the classifier scores where recency applies *is* activity recency. At switches it keeps .296 against chance .072 and the no-state control .095, about 4x chance: the residual signal there is project, backend and status, not activity. On fresh spawns strict still gets .414 (12/29) where habit gets 0 and newest .276.

**Newest equals Jev at @3** on every stratum (switches .740 vs .746). At @1 on fresh spawns the fit adds about +17pp over "youngest" (.448 vs .276) with overlapping CIs at n = 29. Honest framing: the fresh-spawn win is mostly recency of activity plus some fit, not conclusively more.

**The hybrid wins overall and loses at switches.** habit-then-state (majority-past where it has a vote, else Jev): worker .613 with OLD, .605 strict; beats majority-past alone (.560) and Jev alone (.575). But at switches .284 vs Jev alone .385, and on fresh spawns 0: habit votes for a wrong existing peer instead of abstaining. So the two-tier design needs a "habit is wrong here" detector, not "habit has no vote". The router's own confidence is not one (§6d). The one detector state can offer is "a fresh candidate is present"; hybrid-v2 on that trigger is being scored from recorded rows.

**The orchestrator-sender population is the harder and truer one** (the live track is orchestrator-sender): majority .598 and majority-past .454 beat Jev .402 outright; on its 62 switches majority-past .339 vs Jev .371, within noise; on its 10 fresh spawns Jev .50 vs habit 0. The pooled positives are carried by peer and relay senders.

**Claims the data supports, as of this run:** (i) Jev is far above chance and above recency at switches; (ii) it does not beat a deployable habit table at switches; (iii) most of its score on continuation rows is activity recency, and the residual at switches is fit; (iv) where history is empty it routes at 4 to 6x chance, with a small, uncertain edge over "youngest peer"; (v) habit-then-state wins overall but needs a wrongness detector to win at switches. Angle, converged: **routing among stateful agents is mostly habit; state matters at cold start; the open problem is detecting when habit is wrong.**

**Codex round 2 (2026-10-13), what it does to the positive claim.** "Fresh target" means zero prior ask involvement, not a proven spawn; 9 of the 29 rows have exactly one such candidate. A rule that picks uniformly among no-prior-ask candidates has conditional expected f@1 **.478** on those 29, above strict's .414 and near unmasked .448; across all 169 switches it scores .082. So the cold-start win is reproduced by "pick any novel peer" once you know the target is novel, and is worthless when you do not. The claim shrinks to its true shape: given a detector for "habit is wrong here", trivial rules route the cold start and the classifier adds nothing demonstrable; the detector is the whole problem. hybrid-v2's trigger precision ("a no-prior-ask candidate is present") is now the decisive number, and it is being scored with `novelty` as well as Jev behind the trigger. Also from round 2: the hybrid gate has no future leakage (908/908 checked); the 171 to 169 difference is bare-ack semantics; four parity divergences from the live tracker are to be declared with per-row impact (ambiguous message acks touch 20 scored switch rows; a sensitivity line will exclude them); four mask names suffice once applied to the sender block.

### 6e.3 Clean inputs: sender-masked strict, novelty control, trigger, hybrid-v2 (2026-10-13, #461 after codex round 2; round 3 pending)

Reruns on corrected exchange values with the sender block masked (3 x 908 Jev calls, 12.9M tokens). f@1; Wilson CIs where given.

| arm | worker 555 | continuation 333 | **switches 169** | novel target 29 | orch 97 | orch switches 62 | no-vote 76 |
|---|---|---|---|---|---|---|---|
| chance | .134 | .170 | .072 | .083 | .060 | .061 | .112 |
| jev-claimed | .139 | .156 | .095 | 0 | .113 | .129 | .158 |
| jev-attested | .578 | .712 | **.385** [.31-.46] | .414 | .402 | .371 | .355 |
| jev-both | .595 | .727 | .396 | .414 | .423 | .403 | .382 |
| **strict, sender masked** | .312 | .321 | **.302** [.24-.37] | .414 | .196 | .242 | .329 |
| strict-v1 (sender leak) | .288 | .285 | .296 | .414 | .196 | .242 | .329 |
| novelty (analytic) | .037 | 0 | .082 | **.478** | .054 | .084 | .151 |
| newest | .571 | .793 | .195 | .276 | .258 | .048 | .382 |
| majority-past | .560 | .805 | .254 | 0 | .454 | .339 | 0 |

**The strict claim stands on clean inputs.** Masking every activity-derived field, sender included, collapses continuation (.712 to .321) and leaves switches at .302, about 4x chance and 3x the no-state control. Most of the classifier's score where recency applies is activity; the residual at switches is fit between message and peer *beyond the activity fields*. Codex round 3 caution, adopted: a field ablation cannot remove activity entirely, because candidate membership and status are themselves activity-selected, and strict vs habit pooled is 45-37, p = .44, so strict's edge over habit is unestablished. The sender leak had moved the number by .006.

**Cold start is a detection problem, and the detector exists.** Given a novel target, uniform choice among novel candidates (.478) beats Jev (.414) and newest (.276): the router adds nothing there. The question is knowing when. Trigger "a no-prior-ask candidate is present": precision at any age .236 (29/123), under 60 min .489 (23/47), **under 10 min .536** (15/28); orchestrator-sender .256 / .538 / .600. Misses (target novel, trigger off): 0/46, 6/122, 14/141.

**hybrid-v2 = habit unless the trigger fires, then state (fallback to Jev with no vote).** Trigger under 10 min, Jev as the state arm, paired sign tests:

| set | hybrid-v2 | hybrid-v1 | majority-past | jev alone |
|---|---|---|---|---|
| worker 555 | .611 | .609 (ns) | .560 (41-13, p<.01) | .578 |
| continuation 333 | .793 | .817 (4-12, p=.08) | .805 | .712 |
| **switches 169** | **.337** | .284 (10-1, p=.01) | .254 (15-1, p<.01) | .385 (23-31, ns) |
| novel 29 | .241 | 0 | 0 | .414 |
| orch 97 | .526 | .505 | .454 | .402 |
| **orch switches 62** | **.468** | .387 (5-0, p=.06) | .339 (8-0, p=.01) | .371 |

Under 60 min: switches .367, orch switches .452. With strict as the state arm: switches .320, orch switches .419. With `novelty` as the state arm: switches .320 to .326, orch switches .392, so the model barely matters once the trigger fires; Jev adds a few points. Reading: v2 beats both habit variants at switches and holds worker overall, gives up 2.4pp on continuation, ties Jev alone at switches, and on the orchestrator's own switches beats everything (n = 62). Deployable, cheap, and most of its value is the trigger, not the model. **Caveat (codex round 3):** the 10-minute threshold was chosen on the same data, so this is exploratory, not held-out. Pre-register it: from 2026-10-13 the live shadow track scores `habit-unless-arrival(10min)` as a fixed rule, and the paper reports its live precision and f@1 separately from the historical fit. The off-row figures (14/141, 6/122) are conditional prevalence; the false-negative rates are 14/29 and 6/29.

**Base rates at switches.** Habit voted and missed on 71% of worker switches (107/150) and 60% of the orchestrator's (31/52). The target was the newest candidate on 19.5% of worker switches and 4.8% of the orchestrator's: "someone just woke up" explains under a fifth of switches. Most switches are among known peers, where recency is wrong by definition, novelty is silent, and only fit does anything.

**Declared divergences from the live tracker,** each with impact: 15 ambiguous message acks touch 20 scored switch rows; excluding them (149 switches) moves nothing (mp .221, newest .221, jev .383, strict .309). TTL exact vs lazy: 0 scored rows at 24h10m or 48h. ForgetPeer on reaping: 0. Self-pair: self-asks counted in open_asks_inbound but not in the sender's own last_exchange (live would set it), sender block only, 74/1,001 snapshots, left as a limitation. The 171 to 169 difference is two bare-ack rows.

**Paired, Jev vs majority-past (corrected strata).** Switches 169, fallbacks as misses: Jev-only 46, habit-only 24, exact sign **p = .012** (.385 vs .254); restricted to the 150 rows where habit voted: 41-24, p = .046 (.400 vs .287). Orchestrator switches 62: 14-12, **p = .85** (.371 vs .339); where habit voted: 11-12, p = 1.0. So Jev beats habit at switches pooled across senders, and not at all on the orchestrator's own switches, which is the population the live track measures. The earlier tie (44-35, p = .37) was an artifact of the pre-fix strata: the exchange fix moved the easy-for-habit rows out of the switch set. jev-attested vs jev-both: 4-2, p = .69, a tie everywhere.

**Best deployable arm per stratum (@1).** all 908: `recent` .681 vs habit-then-Jev .663, p = .26, **a recency rule is as good as the best hybrid over everything**; worker 555: habit-then-Jev .609 = recent .600; continuation 333: habit-then-Jev .817 / habit .805 > Jev .712; switches 169: Jev .385 (both .396) > habit .254; novel-target 29: novelty .478 > Jev .414 > newest .276 > habit 0; existing-target 140: Jev-both .393 vs habit .307, p = .155; no-exchange 53: Jev-both .396 = newest .377; orchestrator worker 97: habit-then-Jev .505 vs habit .454, p = .06; orchestrator switches 62: Jev-both .403 / Jev .371 / habit .339, indistinguishable; habit no-vote 76: newest .382 = Jev-both .382; peer-sender 243: Jev .642 >> habit; relay-sender 215: recent .791 > habit-then-Jev .698, p = .001.

**Codex round 3 (2026-10-13): all four checks pass**, numbers reproduce exactly from an independent backup; mask test verified across 36,308 sender and candidate blocks; novelty analytic; trigger reads no target; sign tests agree (pooled 46-24 p = .0115; orchestrator 14-12 p = .845). Review released. **Open:** Clef-flash on the 555 worker rows after 00:05 UTC; live validation of the pre-registered trigger.

Cost of the historical track so far: 6,356 Jev calls, 28.8M input tokens, about $1.21.

## 6f. DecisionBench sanity check (2026-10-12)

Full note: [[decisionbench-sanity-2026-10]]. Adapter measures agreement with the orchestrator's pick (§6a caveat).

**Can the arms route stateless cards?** Modestly, yes. aware-c2 full (n = 691): jev-claimed .175 / .547 @1/@3, jev-both .185 / .575, random .096 / .305; inside DecisionBench's published 14 to 30% band. jev-attested .119 / .353: cards carry no attested fields, so it is a no-information control and behaves like one. An in-order majority baseline beats Jev here too (.315 / .641): orchestrators concentrate on a few delegates.

**The switch split does not exist there.** Only 22% of delegations repeat the previous delegate (26% within task), against roughly 57 to 64% in the mesh. Jev is flat across the split (c2: switch .178 vs same .129; pooled over four conditions, n = 2,645: .161 vs .186). So DecisionBench is a different regime: a stateless pool where the orchestrator spreads work, not a stateful mesh where most traffic is continuation. This is not external support for the switch angle; it is the quantitative form of the paper's positioning. **Repeat rate is the one-number diagnostic for which regime a routing problem is in**, and the switch-conditioned metric only matters where it is high.

Caveats: the adapter's `recent` and `majority-past` are degenerate on cards (no exchange fields), so prev and majority-prior were computed post hoc from trace order (issue filed). jev-claimed on c1 cards (about 125KB) returns HTTP 400 on every row. Clef did not run: the account's daily allocation was exhausted; follow-up command recorded in the note for a free UTC day. Jev cost about $0.65.

## 7. Limitations — write these first

- **~100 labelled asks per month.** The honest n. Three months is the floor for the static track; six is comfortable.

- One user's mesh. Partly answered by static track and DecisionBench check. Not fully.
- "Intended peer" as ground truth assumes the sender was right. Need an outcome measure: did the ask close usefully? Redirects are the signal.
- Jev is a product. A second classifier (logistic on the same features, as TwinRouterBench did) is required for the claim to be about *attested state*, not about Jev.
- Candidate sets are small (single-digit peers). Fidelity@3 may saturate.
- **Clef arms (added 2026-10-12, branch feat/clef-arms): re-run vs recorded is a methods choice.** `replay --recorded` scores only what a model answered at decision time, so Clef has no recorded rows until it runs as a shadow arm. Scoring Clef on the asks logged before that point means re-running it (one paid call per ask). A re-run asks today's model about a candidate snapshot that was taken earlier, and ages are measured from the recorded `decided_at`. What drifts is the model, not the state. A model that changed or was redeployed after the ask is then compared against Jev rows recorded at the time. State in the methods which rows were re-run and which were recorded, and don't pool them in a single fidelity number without saying so. Workers AI model ids carry no version, so the template hash pins the prompt but nothing pins the model: record the re-run date. The template is shared with Jev byte for byte (same `template_sha256` per view), so a jev/clef/clef-flash comparison varies only the model.
- **Clef truncation.** Cloudflare silently truncates an oversized state (clef-flash's window is 24,576 tokens, clef's 65,536). **The invariant is the provider's count:** an answer is scored only if `usage.input_tokens` is positive and more than 256 below the window. Otherwise it is `state_truncated`, or `unparseable` if the count is missing or <= 0. A local byte check (`state_too_large`) is a cost-saving heuristic, not a bound. Codex disproved "bytes >= tokens" (#423 review): the tokenizers NFC-normalize before byte-level BPE, so U+0344 is 2 bytes and 3 tokens. The true worst case is about 3 tokens per byte (UAX #15 caps NFC expansion at 3x), so a real bound would be window/3, about 7.8KB for flash, which would refuse more than half of the logged asks. The stronger reason is structural: the server parses our JSON and tokenizes its own rendering of the state, so no client-side byte budget can ever be a true bound, however it is set. Methods should say: no row is scored without a provider count below the window. The heuristic usually overcounts, so large asks fall back on flash somewhat sooner than they need to. That fallback rate is per model and visible; report it beside fidelity, because it biases flash's scored subset toward smaller states. `signals.request_bytes` / `input_tokens` on every row show how close each ran to the ceiling.
- **Clef hosted caps (live smoke 2026-10-12): the documented window is not the limit.** The hosted endpoint cuts a long state to exactly 64,000 tokens (clef) or 24,000 (clef-flash) and still answers HTTP 200, reporting usage.input_tokens at the cap. The evidence: identical noul to four decimals across ~200KB (clef) and ~60KB (flash) of extra body. Far past the cap it returns 413 / 5021. Both caps sit below the documented windows, so the first truncation check (window − 256) missed them; fixed in #423 to judge against the measured cap. The caps are undocumented and can change: re-run `TestLiveClefHostedCap` on any Workers AI change. Measured on real ledger states (`both` view, 11-16 candidates): ~296 tokens and ~1KB per candidate, 5,658 tokens at 16 candidates, ~3.6 bytes per token. U+0344 text: 1.41 tokens per byte. Single-call latency on this run: clef 0.46-1.3s, clef-flash 0.9-1.3s, so flash was not faster here, against the vendor's 39ms claim. n is tiny, but replay should report latency per arm.
- **Shared Clef breaker.** clef and clef-flash share one breaker (one account). A breaker_open row lists `gated_by` models, and replay suffixes `:other_model` when the gate came from the other model, so "never sampled" and "gated by its sibling" stay distinct.

## Venue

Undecided. Workshop first is the honest default — the n=1 weakness is real. arXiv regardless.

## If the router loses

Write it. "Explicit addressing beats learned routing among stateful agents, and here is why" is more useful than the positive result and nobody publishes it.
