---
title: "When Routing Benchmarks Reward Repetition: Codex's paper angle"
created: 2026-10-13
status: independent-framing
tags:
  - project/repowire
  - research
  - routing
  - writing
aliases:
  - Codex router paper angle
---

## A. The paper I would submit

Prasanna, I would write an **evaluation paper with a concrete empirical counterexample**. Its contribution is a protocol for distinguishing repetition, recipient changes, and missing history when evaluating routing to persistent agents. The event-journal reconstruction supplies the experiment. The arrival rule supplies a design implication. Neither should compete with the evaluation claim for the title.

**Lead claim:** In this mesh, aggregate recipient agreement conceals opposing router effects on continuations and switches. Changing the mixture of those requests reverses the ranking of fixed policies. Report history-conditioned agreement before claiming that a router improves routing.

That is a scientific argument: a measurement problem, an operational definition, an observed counterexample, controls that test competing explanations, and a prospective consequence. The mathematics of mixture averages is elementary. The contribution is showing that this problem changes the conclusion of a real agent-routing evaluation, and giving others a reproducible protocol that exposes it.

**Title: When Routing Benchmarks Reward Repetition: Evaluating Recipient Selection in a Persistent Agent Mesh**

### Abstract

Routing requests to persistent agents combines repeated contact with recipient changes, yet aggregate agreement scores merge these decisions. We introduce a history-conditioned evaluation protocol and apply it to an event-journal reconstruction of an agent mesh. Across 555 worker-directed requests, a state-informed router matches the recorded recipient on 57.8%, compared with 60.0% for recent contact and 56.0% for past majority. These totals conceal opposing effects: against past majority, the router gains 22 matches on switches, loses 31 on continuations, and gains 19 without exchange history. Reweighting the observed continuation and switch scores reverses the router ranking without changing either policy. Activity masking and a novelty control separate residual routing signal from simple historical cues. A DecisionBench replay illustrates why history definitions and label semantics must accompany cross-dataset comparisons. We contribute a reproducible protocol for measuring where routing policies help, and derive a prospective test of arrival-triggered routing from its error decomposition.

### The experiment and the unit of measurement

Call the outcome **recipient agreement@1**, not routing quality or skill fidelity. The label is the recipient selected in the recorded ask. That is a useful, reproducible behavioral target, and we should own it explicitly.

Define a continuation using the reconstructed recent exchange with the sender; a switch is an eligible request whose recorded recipient differs from that contact; retain missing exchange history as its own stratum. A switch is not necessarily a new task, a new session, a wrong habitual choice, or a consequential decision. These strata are retrospective diagnostics using the label. They are not features a deployed router gets to observe.

The protocol is: reconstruct only information available before the request; report candidate coverage and exclusions; score uniform chance analytically; compare temporal baselines without future data; report agreement by history condition and sender type; decompose the aggregate difference; test explanations using masks and simple controls; validate any derived policy on later traffic. Each step answers a specific alternative explanation for a routing improvement.

### The single figure

**Figure 1: The winner changes when only the request mix changes.** Horizontal axis: fraction of continuations among requests with exchange history, from zero to one. Vertical axis: recipient agreement@1. Plot three straight lines, using the exact counts in Table 2:

$$
A_r(p)=p A_{r,C}+(1-p)A_{r,S}.
$$

Here $C$ is continuation and $S$ is switch. Past majority runs from $43/169$ to $268/333$; Jev attested from $65/169$ to $237/333$; recent contact from zero to one. Mark the observed history-bearing mixture at $333/502=66.3\%$. Jev crosses past majority at **58.3%** continuations and recent contact at **57.2%**. Put the names at the line ends, with direct labels at the crossings.

**What the reader sees in two seconds:** the same router wins in a switch-heavy mixture and loses in a continuation-heavy mixture. A leaderboard number needs its traffic composition.

Caption: “Descriptive reweighting of observed conditional scores, not a forecast under distribution shift. Missing-history requests are reported separately. No routing decisions are rerun.” Do not place DecisionBench on these lines: its conditional scores and history definition differ. The observed point is 66.3% among history-bearing requests, not the 60.0% obtained with all worker requests in the denominator. [R1]

### The four tables

**Table 1. What the evaluation actually observes.** This is a compact dataset and estimand table, not the project's chronology.

| Property | Historical mesh | DecisionBench replay used as contrast |
|---|---|---|
| Evaluated population | 555 worker-bound asks, drawn from 908 scorable asks | 691 aware-c2 delegations |
| History groups | 333 continuation; 169 switch; 53 no exchange | 93 same; 270 switch; 328 first, within task |
| Repeat fraction with history | 333/502 = 66.3% | 93/363 = 25.6% |
| Repeat fraction over all evaluated rows | 333/555 = 60.0% | 93/691 = 13.5% |
| Label | Recorded ask recipient | Recorded orchestrator delegate, not the benchmark's skill annotation |
| History semantics | Reconstructed exchanges, including qualifying replies, with expiry | Previous delegation within task |
| Candidate information | Activity-inferred registry and reconstructed metadata | Static model cards |

Sources: historical note, “Method” and “Strata sizes (corrected)”; DecisionBench note, “aware-c2, all delegations”; fractions recomputed from those counts. The much-quoted 22% in DecisionBench is 392/1,749 non-first delegations across all conditions, not the denominator corresponding to the mesh's 60%. These are different observational regimes; they are not an experiment isolating persistent state. [H, D, R1]

**Table 2. Where aggregate differences come from.** Put this beside Figure 1.

| History condition | Requests | Recent contact, correct | Past majority, correct | Jev attested, correct | Jev minus majority |
|---|---:|---:|---:|---:|---:|
| Continuation | 333 | 333 | 268 | 237 | -31 |
| Switch | 169 | 0 | 43 | 65 | +22 |
| No exchange history | 53 | 0 | 0 | 19 | +19 |
| All worker requests | 555 | 333 | 311 | 321 | +10 |

On requests with exchange history, Jev has 302/502 matches versus majority's 311/502. Its overall advantage over majority comes from adding the missing-history stratum. This is the cleanest new reading of the existing rows: **the aggregate advantage and the switch advantage are different effects**. Zero on missing history for majority is an abstention scored as a miss, so coverage belongs beside conditional accuracy. [R1]

Add a small sender breakdown in the final paper rather than treating pooled performance as an orchestrator result: Jev versus majority matches are 39 versus 44 on 97 orchestrator requests, 156 versus 121 on 243 peer requests, and 126 versus 146 on 215 human-relay requests. These are descriptive differences, not separately established population effects. [R1]

**Table 3. Which explanations survive controls?**

| Diagnostic set | Jev attested | Past majority | Strict activity mask | Uniform novelty | Analytic uniform chance |
|---|---:|---:|---:|---:|---:|
| All switches, n=169 | .385 | .254 | .302 | .082 | .072 |
| Switches with any recipient/project reference, n=58 | .379 | .466 | .224 | .068 | .084 |
| Switches without any recipient/project reference, n=111 | .387 | .144 | .342 | .089 | .066 |
| Novel-recipient switches, n=29 | .414 | .000 | .414 | .478 | .083 |
| Orchestrator switches, n=62 | .371 | .339 | .242 | .084 | .061 |

Sources: H, “Results,” and the new reference audit R3 in section C for the two reference-filtered rows. Report the pooled Jev/majority discordant counts, 46 versus 24, with nominal row-level p=.012; weekly-block p=.031 and sender-block sign p=.134 show that the inference depends on the assumed dependence unit. The magnitude-weighted sender sign-flip test gives p=.294. Report orchestrator switches, 14 versus 12, nominal p=.845. The strict residual does not collapse after excluding all detected recipient/project references: it scores 38/111=.342. That establishes robustness to this lexical filter, not semantic understanding; candidate membership remains activity-selected. Uniform novelty is an analytic expected score; its higher point estimate on novel recipients does not establish statistical equivalence or superiority. The result motivates a cheap control, not a claim that any novelty rule always finds the target. [H, R3]

**Table 4. Does a diagnostic become an executable policy?** Separate exploratory replay from prospective results.

| Policy or control | Worker agreement | Switch agreement | Orchestrator-switch agreement | Evidence status |
|---|---:|---:|---:|---|
| Past majority | .560 | .254 | .339 | Recorded baseline |
| Jev attested | .578 | .385 | .371 | Recorded router |
| Journal-defined hybrid-arrival | .568 | .326 | .392 | Exploratory historical replay |
| Ledger-defined hybrid-arrival | .511 | .258 | .284 | History-availability sensitivity |
| Frozen arrival policy on later traffic | Pending | Pending | Pending | Prospective validation |

Source: H, “hybrid-arrival.” Explain the policy in one sentence: use past majority unless an eligible candidate has no previous ask participation and activity within 600 seconds, then choose uniformly among such candidates. The threshold was chosen on the historical data. It is frozen for the prospective test, not pre-registered for the retrospective result. Last activity is not birth time; call this an activity-and-history trigger, not an observed spawn detector. The journal/ledger gap makes history availability part of the algorithm's specification. [H]

I would not headline hybrid-v2's best historical score. Its threshold variants and fallback behavior belong in the supplement. In particular, the analysis script's v2 falls back to Jev when majority has no vote, even if the arrival trigger is off. That differs from a pure “majority unless arrival” policy and from the free hybrid-arrival arm. A policy comparison has to name that fallback explicitly. [R1: inspection of `analyze4.py`, functions `v1`, `v2`, `v2n`]

### Where related work should do real argumentative work

Against **DecisionBench**, distinguish its skill-based routing fidelity from our replay agreement with the recorded delegate. It supports evaluating orchestration separately from end-task quality; it does not make agreement with an orchestrator a correctness label. Our adapted replay is a contrast in history structure, not replication of its published fidelity or external validation of our mesh result. Do not explain its quality result by asserting worker interchangeability without testing that mechanism. [DecisionBench](https://arxiv.org/abs/2605.19099)

Against **TwinRouterBench**, position chronological reconstruction beside its router-visible prefixes and execution-verified labels. Both make offline routing evaluation concrete; their downstream execution grounding is exactly what our recipient labels do not yet supply. Use the current version and avoid inheriting old numerical claims from an earlier version. [TwinRouterBench](https://arxiv.org/abs/2605.18859v4)

Against **MasRouter** and **Uno-Orchestra**, define the routing object. They allocate collaboration, models, or workers; ours selects an already-existing recipient in a changing mesh. Say “we study a different routing object,” not “all earlier benchmarks are stateless.” That universal absence claim needs a much wider search. [MasRouter](https://aclanthology.org/2025.acl-long.757/), [Uno-Orchestra](https://arxiv.org/abs/2605.05007)

Use **The Provenance Paradox** only to distinguish asserted descriptions from observed metadata. Historical descriptions are absent here, so these rows cannot show that observed state beats genuine descriptions. Use **OrchBench** as adjacent work on evaluation without full live execution; its validation cannot validate our reconstruction by analogy. [The Provenance Paradox](https://arxiv.org/abs/2603.18043), [OrchBench](https://arxiv.org/abs/2607.25656)

The remaining related-work search should cover temporal link prediction and next-contact prediction, session-based recommendation, and selective prediction with abstention. Those are the strongest conceptual challenges to novelty: repetition baselines, time-respecting evaluation, and deciding when to override a default are established ideas. Our claim should be the measured failure mode and operational protocol for persistent agent routing, not invention of those ideas.

### What I would cut

Remove the provider bake-off from the central narrative. Keep Jev as the instrument used for this study; a cheap independent classifier is more valuable scientifically than another branded leaderboard column. Remove failed provider calls, quota timing, release history, implementation review rounds, and cumulative experiment spend from the body. Keep reconstruction divergences and provider failures in an artifact appendix because they affect reproducibility and coverage.

Cut the empty claimed arm as evidence about description quality; retain it only as a no-information control. Cut the retrospective majority as a “ceiling”: future-informed majority is a leakage reference, not a bound on achievable accuracy. Cut “switches matter more” unless we measure their downstream cost. Cut the full threshold sweep from the main tables, and do not equate missing exchange history, no majority vote, novel target, and fresh spawn. They select different requests.

### What the reviewer will ask first, and the next experiment

The first question will be: **“You partitioned on the answer. What does this tell a router before it chooses?”** The response is a clear division of labor: the partition diagnoses where a policy helps; a frozen, observable trigger tests whether that diagnosis can improve selection. Report trigger coverage, errors on both sides, agreement, and abstention on the complete future stream, with sender strata fixed beforehand. Test the trigger on all eligible traffic; its precision conditional on already knowing a request is a switch is not deployment precision.

For the evaluation claim, the next free analysis should use time blocks and leave-one-sender-out sensitivity, with paired uncertainty clustered by a defensible interaction unit. The existing row-level tests are exploratory because requests share senders and conversations, and many variants were inspected. Publish the number of independent blocks before interpreting an interval. A later collection period and another independently operated mesh would test whether the empirical pattern travels.

For stronger routing claims, first add an inexpensive lexical or linear candidate ranker, with chronological training and test boundaries, and audit explicit recipient names or identifiers in request text. A strict activity mask alone does not distinguish task matching from recovering an address written in the message. Then measure recipient acceptance, redirect behavior, or verified task completion on a prospective sample. These answer different questions and should not be collapsed into one “accuracy.”

### Limitations paragraph for the paper

This study measures agreement with recorded recipients in one user's mesh, not optimal recipients or downstream task utility. Its registry is reconstructed from activity, descriptions are missing, and selection into the scorable set may favor observable peers. The address audit finds only one literal-recipient reference among switches and retains a .342 strict score after excluding all detected project or address references, but it does not remove indirect identity cues or identify the model's mechanism. Almost all switches belong to an early epoch (165 of 169 are in May or June): the eight occupied weeks are not proven independent, and one sender contributes 16 of Jev's 22 net extra matches. Weekly and sender clustering give different inferential conclusions; after jointly grouping by shared week or sender there are only five blocks, with one holding 145 requests. Thresholds and subgroup analyses are exploratory, model runs are not replicated, masks retain activity-selected candidates, and the DecisionBench adapter differs in history semantics and labels. These boundaries scope the empirical case study while leaving the reporting protocol independently reusable. [H, D, R3]

### Venue and manuscript shape

Write an arXiv preprint as a compact empirical evaluation paper, then target an agents-evaluation workshop. My specific topical model is **Evaluation of Interactive Agents at NeurIPS**: its call explicitly welcomes collaborative-agent evaluation, trajectory metrics, and early-stage work. Its listed 2026 submission deadline has already passed, so that edition is a fit reference, not an available submission slot. Target a subsequent verified call with that scope; do not invent an open deadline. [Official workshop call](https://eval-interactive-agents-workshop.github.io/)

The paper should proceed from the misleading aggregate comparison to the protocol, reconstruction, decomposition figure, explanatory controls, and frozen prospective policy. The artifact contribution strengthens this evaluation paper. A pure reconstruction paper would need more external adoption or validation; a pure policy paper would lean too heavily on a retrospectively selected rule. The evaluation angle is already supported by the recorded evidence.

### Evidence ledger for A

- **H:** [[jev-router-historical-track-2026-10]], corrected “Results,” “Paired tests,” “Strata sizes,” and “hybrid-arrival” tables. This supersedes stale mesh counts mentioned in the DecisionBench note.
- **D:** [[decisionbench-sanity-2026-10]], aware-c2 table, stratum definitions, adapter caveats, and limits. The random scores there are recorded sampled results; do not silently relabel them analytic expectations.
- **Related-work starting point:** [[jev-router-related-work-2026-10]]. Primary paper pages above were checked separately for positioning.
- **R1:** Free recomputation on 2026-10-13 from `historical-track-2026-10/historical.jsonl`, `runs/jev-attested.rows`, `runs/jev-attested-strict.rows`, and `runs2/majority-past.rows`, using the loading and stratum definitions before `mode=sys.argv` in `analyze4.py`. The source directory is the existing router-paper research directory. Sum binary `hit1` over each worker stratum, retaining fallbacks as misses. This yields Table 2 and the sender counts. The crossing with majority is $(65/169-43/169)/[(65/169-43/169)+(268/333-237/333)]$; with recent it is $(65/169)/[1-237/333+65/169]$. No provider calls or source edits were made.

Part A was written to this file before opening the paper skeleton. The framing sentence and candidate angles in the incoming brief were already visible; independence here means independent reading and synthesis before seeing the skeleton, not a blind review.

## B. Where I disagree with “Mostly Habit”

I have now read the proposed abstract and sections 6d through 6f of [[jev-router-paper-skeleton]]. My choice stays the same: make the evaluation protocol the paper, and make habit and arrival the observations it explains. Anya's strongest insight is that history changes the problem. The abstract weakens that insight by turning it into a general claim about what routing needs.

### What it gets right, and what it buries

It correctly insists on temporal baselines, analytic chance, sender strata, an activity mask, and a novelty control. Those controls turned a product comparison into an investigation. The useful result is not that a particular small model wins or loses. It is that several plausible readings of the same aggregate result disagree until the measurement is decomposed.

The lead is buried in the final promise to release a switch-conditioned metric. Move that contribution to the first paragraph, but broaden it to a history-conditioned protocol: dropping continuations would simply create another selective leaderboard. The intended reader should leave knowing how to evaluate their own persistent-agent router, even if they never use Repowire or Jev.

The clean decomposition in Table 2 is stronger than “mostly habit.” A model can have a real switch advantage and still lose on history-bearing traffic; adding missing-history requests changes the overall conclusion. That observation gives the paper an argument with a beginning and an end.

### The sentence I expect a reviewer to reject first

> “A detector exists in state: a peer that appeared in the last ten minutes predicts a switch to it with 54% precision in the historical data...”

The reported statistic conditions on the event the detector is supposed to discover. It is precision among known switches for whether the target belongs to the eligible novel-candidate set. It does not measure prediction of a switch in the incoming stream, nor does it identify a unique recipient. Also, “appeared” is measured by recent activity and no prior ask participation, not recorded birth time.

I recomputed the exact same trigger across the broader populations, without new routing calls:

| Population on which the trigger is evaluated | Trigger-positive requests | Recorded target in eligible novel set | Precision |
|---|---:|---:|---:|
| Worker switches only | 28 | 15 | 53.6% |
| All worker requests | 74 | 20 | 27.0% |
| All orchestrator-worker requests | 16 | 6 | 37.5% |

Source R2: the same loading block as R1; for each population, select rows where `novel(trace_id, 600)` is nonempty, then count whether `meta[trace_id]["target"]` is in that set. `novel` uses journal-defined prior ask participation and `last_seen_age_s < 600`. These numbers are detection precision for the set, not the hybrid's agreement score.

Replace the sentence with: **“A recent-activity and no-prior-ask trigger identifies a candidate set containing the recorded recipient on 27.0% of its activations across worker requests, rising to 53.6% when evaluated only on known switches; we therefore test its net routing effect prospectively on the whole stream.”** A low precision trigger could still improve a weak default on its activation set. The policy test, not the conditional precision alone, decides that.

### Other claims to rewrite

**“The open problem ... is therefore not choosing a peer but detecting when habit is wrong.”** Detection is a useful design hypothesis, not an identified reduction of the whole problem. On known-target switches, someone still has to select among familiar candidates. Past majority is also nonzero there. Moreover, a trigger for majority error is not the same as a trigger for router advantage: overriding helps only when the replacement has greater expected utility. Write: “The results motivate selective override policies that learn when an alternative improves on a temporal default.”

**“Only fit between message and peer does anything.”** Historical majority succeeds on some switches, and the mask does not identify the mechanism of the remaining signal. Names, project metadata, candidate selection, and explicit recipient references can all contribute. Say what survived the mask, then test which inputs explain it.

**“The classifier's own confidence cannot.”** The section compares mean confidence on a small live continuation/switch split. Similar means do not rule out discrimination, a nonlinear relationship, or confidence useful for conditional correctness. The relevant target is the alternative's advantage over the default. Replace the impossibility claim with a prospective calibration question, or omit it from the abstract.

**“Ties the best single arm,” “as good as,” and “on the orchestrator's own switches beats everything.”** A nonsignificant difference is not an equivalence result. The hybrid also changes when its no-vote fallback or history source changes. Use exact policies and point estimates; reserve stronger claims for a predefined paired comparison with uncertainty. The free journal-defined hybrid's .568 worker agreement and the Jev-backed hybrid-v2's .611 belong to different policies, not two implementations of the same score. [H, skeleton 6e.3]

**“Repeat rate is the one-number diagnostic.”** It is an informative descriptor after specifying the history unit, denominator, expiry, and first-request handling. It is not sufficient: conditional router accuracy and sender composition also determine rankings. The mix curve in Figure 1 holds conditional scores fixed specifically to isolate the mixture effect; it does not transfer a routing threshold between datasets. DecisionBench's pooled repeat fraction and the mesh's all-request fraction should not appear side by side without their denominators.

**“State matters at cold start.”** Distinguish missing exchange history, no majority vote, and novel recipient. The controls support usable metadata signal and a simple novelty heuristic on selected strata. They do not establish that the model provides a distinctive cold-start capability or that a fresh session was observed. “History-based and metadata-based rules help on different request populations” is both stronger as a reusable finding and more faithful to what was measured.

There is also an editorial task: section 6e contains successive superseded claims and tables whose conclusions reverse. A reader should encounter one final table per question. Preserve the chronology in the research notebook and artifact audit trail, not the manuscript. That alone will remove much of the work-log feel.

### Is “Mostly Habit” the right title?

It is memorable, but I would not use it as the paper title. “Habit” conflates the most recent contact with the most frequent contact; those are distinct baselines. It also suggests an explanation of agent behavior when we have measured predictability of recipient labels. “What Routing Among Stateful Agents Actually Needs” promises a general design conclusion from a case study.

Use **When Routing Benchmarks Reward Repetition**. It names the phenomenon the paper can demonstrate and invites the reviewer to inspect the measurement. “Mostly habit?” could be a subsection heading, followed immediately by the decomposition showing why the answer depends on the population.

### One paper or two?

**One paper now.** The metric protocol is the contribution; the reconstructed journal is its empirical substrate; the arrival policy is a testable consequence. They form a coherent chain when each has that role. Splitting them now would leave a method paper without enough independent validation and a design paper whose best historical policy was selected on its evaluation data.

A later paper could study selective routing with prospective utility labels, a held-out trigger, and multiple meshes. That would answer a new question: whether choosing overrides improves completed work. The present paper answers a worthwhile question already: how the composition and construction of a routing evaluation change what its scores mean.

Prasanna, I would start writing this paper now, around Table 2 and Figure 1. Treat the live validation as an additional result when it matures, rather than letting another provider column determine whether there is a paper.

### Read-order record

Part A's file SHA-256 before the skeleton was opened: `d900468203b0b1d79aa928c63dfdccb93a728397b6e2cd37dd6fe55a744c6fc7`. Part B was appended afterward. This note proposes follow-up analyses and experiments; it does not claim they were run. Work completed here was reading, free recomputation of recorded rows, and this framing note.

## C. Recipient references and dependence-aware uncertainty

Follow-up completed on 2026-10-13, using recorded rows only. Table 3 and the limitations paragraph above have been updated with these results; the earlier proposals to do these two audits are now fulfilled. No provider was called. The remaining proposed experiments are still prospective.

**The strict residual survives the recipient-reference filter. The pooled Jev improvement is sensitive to how dependence is grouped.** Those are complementary findings for the evaluation paper: the apparent routing signal is not explained by literal recipient names, while the strength of an improvement claim requires an explicit sampling unit.

### C1. What counts as a recipient reference

All 555 worker rows have recorded, nonempty, untruncated request text. For each row, match against the declared recipient's candidate metadata as reconstructed at that request, not against its current name. Matching is case-insensitive.

- **Literal address:** a bounded full display name, exact peer ID, or `@` handle using the full display name, the backend-stripped name, or the parsed project name. The full-name rule does not match a longer hyphenated name just because it shares a prefix. A sentence-ending period is allowed.
- **Project mention:** the complete parsed `project` field with non-alphanumeric boundaries. This deliberately includes the project appearing inside paths and punctuation-separated names. A project mention can be ordinary task content and can identify several candidates; do not label every project match an explicit address.
- **Any reference:** the union of those two tests. This is the conservative exclusion used below. A stricter sensitivity test requires a standalone project token, excluding its appearance inside a longer dotted or hyphenated token.

These tests detect references, not whether the sender intentionally supplied a routing answer. Inspection of all literal matches found a direct same-project peer reference, roster/status descriptions, and a CLI instruction containing a peer ID. The one literal match on a switch is the CLI instruction; neither Jev arm gets that row right. Shortened aliases, fuzzy spellings, descriptive nicknames, and indirect context are outside this detector.

| Stratum | n | Full display name | Peer ID | `@` handle | Project mention | Any reference, n (%) |
|---|---:|---:|---:|---:|---:|---:|
| All worker requests | 555 | 3 | 1 | 0 | 170 | 171 (30.8%) |
| Continuation | 333 | 3 | 0 | 0 | 93 | 93 (27.9%) |
| Switch | 169 | 0 | 1 | 0 | 57 | 58 (34.3%) |
| No exchange history | 53 | 0 | 0 | 0 | 20 | 20 (37.7%) |
| Novel-target switch | 29 | 0 | 0 | 0 | 7 | 7 (24.1%) |

The detector columns overlap; their sum is not the union. Novel-target switches are a subset of switches. The literal-address union is 4/555 (0.7%) overall, including 3/333 continuations (0.9%) and 1/169 switches (0.6%). Source: R3, `prevalence`.

### C2. Switch scores with and without references

All entries below use the recorded arm outputs; no text was modified and no model rerun. The fractions therefore measure subgroup performance, not the causal effect of deleting a name.

| Switch subset | n | Jev attested, hits / n | Strict, hits / n | Majority-past, hits / n | Analytic chance |
|---|---:|---:|---:|---:|---:|
| All switches | 169 | 65/169 (.385) | 51/169 (.302) | 43/169 (.254) | .072 |
| Literal address present | 1 | 0/1 (.000) | 0/1 (.000) | 0/1 (.000) | .063 |
| No literal address | 168 | 65/168 (.387) | 51/168 (.304) | 43/168 (.256) | .072 |
| Any reference present | 58 | 22/58 (.379) | 13/58 (.224) | 27/58 (.466) | .084 |
| No address or project reference | 111 | 43/111 (.387) | 38/111 (.342) | 16/111 (.144) | .066 |

Under the stricter standalone-project sensitivity definition, 43 switches are flagged and 126 remain: Jev scores 50/126=.397, strict 39/126=.310, majority 26/126=.206, and chance .068. Simple case-insensitive substring matching gives exactly the same flagged rows as the broader any-reference rule. The conclusion is therefore not an artifact of the chosen token boundaries. Source: R3, `switch_audit`.

**Paper wording:** “The strict-mask residual persists after excluding requests that contain the recorded recipient's display name, peer ID, recognized handle, or project token: agreement is .342 against analytic chance .066 on the remaining requests.” This supports residual routing signal beyond detected lexical references. It does not establish that the model performs semantic fit: the retained group has different senders and candidates, and indirect identity cues remain available. Do not claim that removing references improves the router based on these observational subsets.

### C3. Definitions for the dependence analysis

Compare binary hits on the same 169 switches, with fallbacks as misses. For each block, count discordances as **arm-only : majority-only** and use a two-sided exact paired sign test. These within-block tests still treat discordant rows as independent and are descriptive diagnostics. For the test **over blocks**, first sum hit differences within each block, then apply an exact sign test to positive versus negative block sums, discarding zero sums. That gives each informative block equal weight and tests directional consistency, not the traffic-weighted average effect.

As a complementary magnitude-sensitive calculation, swap the arm labels jointly within each block and enumerate the exact distribution of the summed difference. This cluster sign-flip test retains the request-weighted effect statistic but assumes independent, sign-exchangeable cluster contrasts under the null. Neither test proves its independence assumption. No multiplicity correction or confirmation claim is attached to this exploratory collection of tests.

Weekly blocks start Monday at 00:00 UTC. Epochs are May, June, and July onward. Sender clusters use `peer_id`, because display names are reused. The additional display-name analysis deliberately merges those sessions as a coarser sensitivity test. For hybrid-v2, use Jev if the journal-defined 600-second novelty trigger fires **or majority has no vote**, and majority otherwise. This is the Jev-backed exploratory v2, not the free `hybrid-arrival` policy.

### C4. Weekly and epoch results

| UTC week beginning | Switches | Jev-only : majority-only | Within-week p | Hybrid-v2-only : majority-only | Within-week p |
|---|---:|---:|---:|---:|---:|
| 2026-05-18 | 33 | 11 : 2 | .0225 | 2 : 1 | 1.0000 |
| 2026-05-25 | 77 | 21 : 17 | .6271 | 4 : 0 | .1250 |
| 2026-06-01 | 34 | 9 : 4 | .2668 | 7 : 0 | .0156 |
| 2026-06-08 | 1 | 1 : 0 | 1.0000 | 0 : 0 | 1.0000 |
| 2026-06-15 | 16 | 3 : 1 | .6250 | 2 : 0 | .5000 |
| 2026-06-22 | 4 | 0 : 0 | 1.0000 | 0 : 0 | 1.0000 |
| 2026-07-06 | 3 | 0 : 0 | 1.0000 | 0 : 0 | 1.0000 |
| 2026-08-03 | 1 | 1 : 0 | 1.0000 | 0 : 0 | 1.0000 |
| Pooled rows | 169 | 46 : 24 | .01153 | 15 : 1 | .000519 |

For tests over weeks, Jev has 6 positive, 0 negative, and 2 tied blocks: **p=.03125**. Hybrid-v2 has 4 positive, 0 negative, and 4 tied blocks: **p=.125**. The magnitude-sensitive sign-flip values happen to be identical because all nonzero weekly contrasts have the same sign. Weeks with no switches are absent, not independent negative evidence. Source: R3, `dependence.*.weeks`.

| Epoch | Switches | Jev-only : majority-only | Within-epoch p | Hybrid-v2-only : majority-only | Within-epoch p |
|---|---:|---:|---:|---:|---:|
| May | 110 | 32 : 19 | .0919 | 6 : 1 | .1250 |
| June | 55 | 13 : 5 | .0963 | 9 : 0 | .0039 |
| July onward | 4 | 1 : 0 | 1.0000 | 0 : 0 | 1.0000 |

Across the three epochs, Jev has 3 positive blocks: **p=.25**. Hybrid-v2 has 2 positive and 1 tied block: **p=.50**. These coarse tests have very little resolution; failure to reject is not evidence of equality. Source: R3, `dependence.*.epochs`.

### C5. Sender clusters and leave-one-sender-out sensitivity

| Contrast | Jev vs majority | Hybrid-v2 vs majority |
|---|---:|---:|
| Full-sample difference | +22/169, +13.02 percentage points | +14/169, +8.28 percentage points |
| Sender peer-ID blocks: positive / negative / tied | 15 / 7 / 30 | 8 / 0 / 44 |
| Sign test over sender blocks | .13380 | .0078125 |
| Magnitude-sensitive sender sign-flip p | .29407 | .0078125 |
| Display-name blocks: positive / negative / tied | 9 / 3 / 9 | 6 / 0 / 15 |
| Sign test over display-name blocks | .14600 | .03125 |
| Magnitude-sensitive display-name sign-flip p | .30371 | .03125 |
| Leave-one-peer-ID-out difference range | +4.44 to +16.25 percentage points | +5.625 to +8.974 percentage points |
| Leave-one-peer-ID-out nominal row-p range | .001858 to .496617 | .000122 to .011719 |
| Leave-one-out runs with nominal row p below .05 | 51/52 | 52/52 |

Source: R3, `dependence` and `loso`. Leave-one-out p-values are still row-level diagnostics; they are not an additional clustered test or 52 independent replications.

For Jev, removing sender `repow-default-d0345799` (`repowire-codex`, 34 switches) leaves 49/135 hits versus majority's 43/135, with discordances 30:24 and nominal p=.4966. That sender contributes **16 of the 22 net extra matches**. Every leave-one-sender-out effect remains positive, but the pooled significance is heavily dependent on one sender.

For hybrid-v2, the largest reduction comes from removing `repow-default-02ea620a` (`orchestrator-claude-code`, 9 switches): 50/160 versus 41/160, discordances 10:1, nominal p=.01172. Its direction and row-level significance survive every sender deletion. Its lack of weekly significance and its retrospectively selected trigger still govern what can be claimed.

### C6. How many independent blocks are there?

There are **8 occupied weekly blocks, 3 epochs, 52 sender peer IDs, and 21 sender display names**. These are candidate dependence partitions, not counts of demonstrated independent replications. The study contains one independently operated mesh; independence between any of its blocks cannot be established from these rows alone.

To allow both same-week dependence and dependence across the same sender's requests, take connected components after linking rows that share either week or sender peer ID. This yields **5 disjoint blocks of sizes 145, 16, 4, 3, and 1**. It is a conservative additional partition for those two dependence sources, not a guarantee against shared recipients or shared workstreams.

Jev's net differences in those blocks are +19, +2, 0, 0, +1: 3 informative blocks and **p=.25** for both block-sign and joint sign-flip tests. Hybrid-v2 has +12, +2, 0, 0, 0: 2 informative blocks and **p=.50**. Most rows therefore cannot be treated as independent evidence once both dependence relations are preserved. Source: R3, `dependence.*.sender_week_components`.

**Does p=.012 survive defensible clustering?** It remains nominally below .05 under the explicit weekly independence assumption, at p=.03125. It does not survive sender clustering, coarse epochs, or the joint sender/week partition. Thus there is a defensible weekly sensitivity result, but no dependence-robust significance claim. The original p=.012 should be labeled row-level and exploratory. Hybrid-v2 shows the complementary pattern: sender-cluster evidence remains below .05, but weekly and epoch evidence do not. Neither deserves an unconditional “beats habit” claim on this dataset.

**What this changes in the paper:** keep the observed ranking reversal and decomposition as the main result. Add the reference-filtered residual as a successful control. Report the paired differences with their dependence sensitivities, and reserve claims about general routing improvement for the frozen prospective study. The protocol now demonstrates why both lexical-reference controls and dependence units belong beside routing scores.

### R3. Reproduction and artifacts

The existing local `router-paper/historical-track-2026-10/` research directory now contains:

- `codex-angle-audit-2026-10.py`: deterministic free analysis, including exact tests and declared regex rules.
- `codex-angle-audit-2026-10.json`: full counts, scores, every sender deletion, all cluster results, input file SHA-256 values, and a digest of journal-derived first-ask times.
- `codex-angle-reference-flags-2026-10.jsonl`: per-trace match flags for all worker rows, with no message text copied.
- `codex-angle-audit-2026-10.txt`: readable complete output.

Reproduce with `python3 codex-angle-audit-2026-10.py` from that directory. Inputs are the corrected `historical.jsonl`, recorded `runs/jev-attested.rows`, `runs/jev-attested-strict.rows`, `runs2/majority-past.rows`, and the existing `analyze4.py` loading block, which reads `state-copy.db` in read-only mode for journal novelty. The script checks the original 46:24 discordances and hybrid-v2's 57 correct switch predictions before writing its outputs. No repository source, recorded arm output, or original dataset was edited.
