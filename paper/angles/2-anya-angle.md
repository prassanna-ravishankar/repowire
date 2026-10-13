---
title: Router paper, Anya's angle
created: 2026-10-13
updated: 2026-10-13
status: draft
tags:
  - project/repowire
  - research
  - routing
  - writing
---

# Router paper, Anya's angle

Written before reading [[jev-router-codex-angle-2026-10]], for Prasanna to compare. Numbers are from [[jev-router-historical-track-2026-10]] and [[decisionbench-sanity-2026-10]]; the notebook is [[jev-router-paper-skeleton]].


> [!warning] Corrections after reading [[jev-router-codex-angle-2026-10]] (same day)
> - The trigger's 54% precision is conditioned on known switches (15/28). Unconditionally, over all worker requests, it is 27.0% (20/74); orchestrator-sender 37.5% (6/16). It identifies a candidate *set* containing the recipient, not a switch to one newborn peer. Codex's replacement sentence is right and supersedes mine.
> - Repeat-rate comparison with matched denominators: mesh 333/502 = 66.3% among history-bearing requests vs DecisionBench c2 93/363 = 25.6%; not 60% vs 22%.
> - "Habit" conflates recent contact and past majority; they are different baselines and the paper must name which one each sentence means.
> - My recommendation to Prasanna: take codex's angle as the spine (protocol, Figure 1 mixture crossover, Table 2 decomposition) and fold my two-panel figure in as Figure 2 if space allows. It is the stronger paper.

## The claim

**Routing benchmarks score the wrong decisions.** In any routing problem where the same sender decides repeatedly, most decisions are continuations, a one-line recency rule gets them right, and a fidelity number averaged over all of them is a measurement of habit. The decisions that test a router are the context switches. Condition on them, report chance analytically beside candidate-set size, and the ranking of routers changes. On a production agent mesh this reordering is total: averaged, recency beats every model; at switches, a state classifier is the best single arm, habit is the deployable competitor at about a third, and a classifier with no history matches a habit table that needed three months of it.

## Title

*Only the Switches Count: Measuring Routers Where Habit Fails*

(Alternative, if the design is the lead: *Habit Unless Someone Just Arrived: Routing Among Stateful Agents*.)

## Abstract (150 words)

Benchmarks for LLM routing and delegation score every decision alike. We show this measures habit. In three months of a production mesh of long-lived coding agents (908 reconstructed decisions), two decisions in three repeat the sender's previous target; a recency rule scores .68 and no router beats it on the average. Conditioning on the 169 context switches, where recency is zero by construction, reorders every arm: a classifier on attested agent state leads (.39), a per-sender habit table follows (.25), and chance, reported analytically because it varies tenfold with candidate-set size, is .07. Masking activity fields shows most of the classifier's score elsewhere is recency; at switches a residual four times chance remains. Where history is empty, on switches to newly arrived peers, state matches a habit table with none, and "habit unless someone just arrived" beats habit at switches (p < .01). A stateless benchmark shows no switch structure (22% repeats); repeat rate diagnoses the regime. We release the metric, the reconstruction method and a live pre-registered test.

## The figure

One figure, two panels, same arms, same y-axis (f@1), chance drawn as a dashed line in each panel.

- Left panel, "All decisions (n = 908)": bars for recent, habit (majority-past), newest, jev-attested, strict, novelty, random. Recent is tallest; the models are below it. The reader sees "the rule wins".
- Right panel, "Context switches (n = 169)": same bars. Recent collapses to zero, jev-attested is tallest, habit is a third, chance is near the floor. The reader sees the bars reorder.

Two seconds: the ranking flips when you condition. That is the paper.

Optional second figure: f@1 against candidate-set size (four buckets) for random, habit and jev-attested, showing random collapsing tenfold while habit barely moves. It justifies "chance must be reported beside n".

## Tables

1. Arms by stratum, f@1 / f@3 with analytic chance and Wilson CIs: all 908, worker 555, continuation 333, switches 169, novel-target 29, orchestrator-sender 97 and its 62 switches. Arms: random, recent, newest, novelty, majority-past, majority (retrospective, labelled), jev-attested, jev-both, strict, jev-claimed, clef-flash (when scored).
2. Paired sign tests at switches: Jev vs habit (46-24, p = .012 pooled; 14-12, p = .85 orchestrator-sender), strict vs habit (45-37, p = .44), hybrid-v2 vs habit (15-1, p < .01), hybrid-v2 vs Jev alone (ns).
3. Cold start: novel-target 29 and existing 140, arms as above plus the novelty rule (.478 conditional) and the trigger's precision at 10 and 60 minutes.
4. Regime diagnostic: repeat rate and the switch split on DecisionBench (22%, flat) versus the mesh (about 60%, reordering).

## Related work, positioned

DecisionBench, RouterBench, TwinRouterBench, MasRouter, Uno-Orchestra, OrchBench evaluate routers on stateless pools; none conditions on the sender's history because their senders have none. The Provenance Paradox's claimed/attested split is the vocabulary for our views. The contribution is not "stateful routing is different" as a position; it is a metric that shows the difference and the first dataset where it can be measured.

## What to cut

- The frontier-vs-small-classifier framing, unless the frontier arm runs (~$15). Without it the paper does not mention frontier models.
- "Staleness dominates": untestable in a tended mesh; one sentence in limitations.
- The misroute-cost claim: never measured; gone.
- The hub-skip analysis: a footnote at most.

## What a reviewer asks first, and the answer

- *One mesh, one user, one epoch.* Yes; stated in paragraph one of limitations; the live pre-registered test is the answer in progress, and the metric itself is the portable contribution.
- *Ground truth is an LLM's addressing.* Yes; the measured quantity is orchestrator-consistent routing, named as such.
- *Is Jev the point?* No; the strict mask and the novelty control show the signal is state and the cold start is detection; a second classifier (Clef) is scheduled; the arms are swappable.
- *Was the trigger tuned on the test data?* Yes; it is pre-registered and validating live from 2026-10-13, reported separately.

## Venue

A workshop on agents or LLM systems at ICLR or NeurIPS, and arXiv the same week. Four pages plus appendix; the appendix carries the reconstruction method and the declared divergences in full, which is where the method contribution lives for anyone who wants to repeat it on their own mesh.
