# Draft 2 audit, 15 October 2026

Local revision for the author's read, not a submission. No research database or private message text is included. `main.tex`, `refs.bib` and the compiled `main.pdf` are the deliverables. Section order, tables and figures are retained. The flow table gains its missing one-request row; the lifecycle figure gains a caption so its reference resolves to a figure. Existing co-author, artifact-URL and acknowledgement TODOs remain visible. Author and affiliation now read Prassanna Ganesh Ravishankar, Independent Researcher, me@prassanna.io.

## Evidence source and audit coverage

Inventory: `repowire-paper-evidence-inventory-2026-10.md`, dated 14 October 2026. Line numbers below refer to that supplied inventory, not to the paper. Its named scripts and outputs were treated as read-only. This audit checks transcription, denominators, arithmetic and scope against the inventory; it does not rerun the private database extraction or independently recode incidents.

| Evaluation content retained | Inventory lines and derivation |
|---|---|
| Backup date; era dates and schema | 19, 31–40 |
| 150 memory files, 46 vault notes, two repository memory directories | 266 |
| 146 calendar days, 93 event days, 64 active-agent days | 49–51; 145 elapsed days and 146 inclusive calendar dates are different measures |
| 2,810 asks = 1,505 machine + 1,305 other | 53 |
| Activity figure: all 24 plotted values, June 925/1,321 and July machine dominance | 125–130; October peer-and-relay bar is 29 + 8 = 37 |
| Lifetime table: all 4 rows, counts and percentiles | 100–103; orchestrator p75 119.7 minutes rounds to 2.0 hours |
| Other-agent median 12.6 minutes; 30% seen once; orchestrator 16.1 minutes; job executor 35 minutes | 100–103; 248/823 = 30.13% |
| Seven names, spans 138–144 days, counts 49, 37, 30, 3 | 109–115 |
| 64 daemon starts on 14 days; 58 within 5 minutes; 41/60 bursts; burst threshold 5; 243 rows at six minutes | 302–306, 319 |
| 428 contradictions, component counts 130/126/5/167; 485 reaps; offline reasons 226/115/32 | 330; 428/146 = 2.93 per calendar day |
| 1,169 close records; 136 missing (131/5); complete closure table; five send failures | 168–169, 184–186; 909 + 260 = 1,169 |
| Sender latency medians 23/38/40 seconds; human p90 105 seconds; orchestrator p90 8.3 minutes | 182 |
| Flow table: every count and percentage | 148–156; added peer-to-machine row restores total 1,305 |
| Human share 53.5%, monthly 17–60%, zero human notifications, seven agent-to-human asks | 131, 137–142, 155, 160; 698/1,305 = 53.49% |
| 1,635 agent/orchestrator notifications in May–June | 160: 227 + 655 + 358 + 395 |
| Orchestrator sender shares 11% and 23%; largest flow 33.4%; peer-to-peer 26.7% | 137, 142, 148–149 |
| Repeat denominator 1,159, numerator 866, 74.7%; monthly 0.50–0.86; class shares .859/.651/.423 | 192–205 |
| Companion recency 333/555 = .60 and its distinct definition, including 24-hour expiry | 206 |
| 591 agent-to-agent asks, 307 cross-runtime, rounded 52%; pair counts 177/104/13/9 | 158; 307/591 = 51.95%; listed pairs are examples, not an exhaustive sum |
| Review counts 64/152, 37, 30, 23/7; rounds 27/5/3/2 at 1/2/3/5+; all merged | 236–250; timing caveat 260 |
| Estimated three-round review: 12 turns, 30k tokens, 75% saving | 351; operator estimate, not journal measurement |
| Review bot: 714 findings, 284 PRs, rounded 80% medium | 214, 226; repository-history scope |
| Incident count 76, class table, derived 34 and remaining 42, 10/7/25, four credential exposures | 270–280; 13 + 8 + 7 + 4 + 2 = 34; 76 - 34 = 42 |
| Three pre-May and four undated incidents; six signature matches/groups, 43 peers, eleven starts | 278, 286–291 |
| Roughly three shadow-routing days; cluster GBP 27–37; per-model USD .28/3.67/7.37 for 555 decisions | 336, 345, 350 |

The abstract and lessons reuse the checked evaluation values. Outside the evaluation, 370 names/86 projects/2,472 ids and six runtimes match lines 77–92. Tool counts match 361–371; appendix surface dates and PR numbers match 379–400; appendix lifecycle rows match 312–317. The eight incident examples remain the original draft's examples, not a new coding exercise.

## Numerical and population mismatches in draft 1

| # | Draft 1 claim | Inventory evidence and draft 2 action |
|---|---|---|
| 1 | Median *agent registration* 12.6 minutes | Line 102 is the **other-agent** class, not all agents. Class named explicitly throughout. |
| 2 | Human share 54–60% in abstract; 52–60% in every month with volume | Lines 137–142 include July 17% and August 18%. Use pooled 53.5% and full monthly 17–60%. |
| 3 | 1,268 orchestrator/peer notifications in May–June, described as reaching the phone | Line 160 sums to **1,635** and supplies sender classes, not recipient counts. Correct total and remove destination attribution. |
| 4 | 40% of asks to/from an orchestrator | Lines 148, 151–154 already sum to 687/1,305 = 52.6%, before orchestrator-to-human asks. Orchestrator sender total 196 (line 131) implies another six, yielding 693/1,305 = 53.1%. Drop the faulty aggregate; retain directly tabulated flows. |
| 5 | Orchestrator-to-peer dispatch rose 11% to 23% | Lines 137/142 describe **all asks sent by orchestrators**, not only those to peers. Correct the population label. |
| 6 | Repeat range 0.70–0.86 by month | Line 198 has July 0.500. Correct to 0.50–0.86 and make eligibility/denominator explicit. |
| 7 | Classifier .58 and frontier switch .57 in evaluation | Inventory line 206 supports recency .60, but does not record the other scores. They occur in the companion manuscript, but fail this task's inventory-only requirement. Remove from this evaluation; retain the sourced recency comparison and definition. |
| 8 | Three contradictions per active day | Lines 50/51/330: 428/146 = 2.93 per **calendar** day; 93 event days and 64 active-agent days are different denominators. Correct denominator in evaluation; lessons report the count. |
| 9 | Six multi-incident restart days; six individual incidents with signatures | Lines 286–291 contain six **matches**, the last covering incident IDs 63–67. They do not establish six multi-incident days. Remove the day count; describe matches to incidents or groups. |
| 10 | 30 cross-runtime review PRs all have a requester/reviewer split | Line 239 also admits verdicts from two backends. Restore the full classification definition. |
| 11 | PRs merged *after* the reported review rounds | Lines 247–250 count rounds and merges, not their ordering; line 260 identifies four first review messages after merge. Report observed rounds and merged status without temporal inference. |
| 12 | Bot's 714 findings on 284 PRs over the same journal period | Line 214 is repository-history data, unlike lines 236–250. Correct time scope. |
| 13 | 73 incidents within the study window | Line 278 includes four undated incidents; May is not split at May 22. Subtracting three March/April incidents does not establish in-window coverage. Remove 73 and state the date uncertainty. |
| 14 | About USD 11 for 555 decisions per model | Line 345 reports .28, 3.67 and 7.37 by model (11.32 combined). Report the separate model costs. |
| 15 | GBP 27–37 as a mesh hosting cost | Line 350 is a shared cluster total including other projects. Label it as shared-cluster cost, not mesh-attributable spend. |
| 16 | Flow table labeled n=1,305 but rows sum to 1,304 | Line 156 supplies the omitted peer-to-machine ask (1, 0.1%). Restore that row. |
| 17 | Repository's 828 commits and 362 PRs “all but 18 by the operator's account” | Inventory section 10 distinguishes 18 external PRs from bot PRs and commits. State only that 18 PRs came from external contributors. |

Also clarified: “146 days” means inclusive calendar dates (lines 49–51), not 146 elapsed 24-hour periods. The abstract's “75% of requests repeat” omitted eligibility; it now names the preceding-request condition. The paper does not establish continuous daily use: 53 calendar dates have no events.

## Claims that outran the evidence

These changes are explicit scope corrections, not additional results. Original claims are recorded here so the author can decide whether to gather evidence or retain the narrower wording.

- **Single-repository performance:** “A coding agent that holds one repository in context outperforms one that holds five.” The blog supplies motivation, not a controlled comparison. Replaced with the concrete multi-repository workflow.
- **Exhaustive novelty:** “Every existing remedy is push-based”; “three things no coding-agent runtime provides”; “A persistent mesh with a full journal has not previously been reported.” No systematic survey establishes these universal claims. Removed.
- **Identity continuity:** “identity outlives sessions.” Repeated names and churned peer ids do not measure runtime-session continuity. Renamed the subsection and report name reuse.
- **Incident causality:** “The era B gains come from three changes”; “the whole of the era A to era B closure gain”; “closure … is the sum of them”; reminders “keep closure at 98%.” No ablation or controlled workload comparison supports causal attribution. All occurrences now report the era difference with an observational limitation. Retired identities also appear in the inventory's June migration history, before the Go cutover, weakening the claimed between-era timing.
- **Workload explanations:** Job lifetimes are 35 minutes “because a fire is one turn and turns are long”; orchestrator latency is higher “because orchestrator asks are more often work than questions.” Neither explanation is tested in the inventory. Removed the causal clauses.
- **Restart interpretation:** “a restart killed whatever was in flight.” Reconciliation failures are not evidence of every process being killed. State what the rows record.
- **Timer universals and liveness bound:** “every polling loop”/“every timer” caused incidents; staleness is bounded by one minute; “all visible, none silent”; an idle mesh “costs nothing.” One recorded timer-related incident and contradiction logs cannot establish these claims. Describe request-triggered reconciliation without a delay or zero-cost guarantee.
- **MAST coding:** “coded each” of 76 incidents; 34 failures that single-run traces “cannot exhibit”; no MAST mode “for any of these.” Inventory supplies local classes, not a reproducible incident-by-incident MAST annotation. Its mapping is now explicitly proposed, with a red TODO. The table is retained for review. The removed 14-mode/1,642-trace/seven-framework sentence is external taxonomy context, not an inventory-derived evaluation result.
- **Majority of failures:** 34/76 is 44.7%, not a majority. Draft 1 repeatedly implied most failures were the identity/liveness/delivery class. Report the class counts without that inference; no defensible reasoning-versus-substrate partition exists for the 25 “other” incidents.
- **Human destinations and intent:** Sender-class notification counts do not establish that notifications went to the phone, or prove the operator's intent was fulfilled. Removed those inferences.
- **Routing prescription:** The orchestrator is the “only sender that switches often” and recipient selection is “not a human” problem. Repeat rates do not determine optimal deployment. Keep class-specific rates and the evaluation recommendation to report switches.
- **Framework exclusions:** All named frameworks supposedly lack persistence, history and mid-run human input. AutoGen's primary abstract explicitly includes human inputs. Replace the blanket claim with study scope; retain a red comparison TODO. CrewAI and Magentic-One were named without corresponding citations and are omitted from that list.
- **Protocol exclusions:** MCP/A2A supposedly lack lifecycle and delivery semantics. Surveys are not adequate evidence for this blanket assertion. Describe Repowire's local scope and leave a versioned-specification comparison TODO.
- **Cross-system causality:** Gastown/Agent Teams arrived at identity “by the same route”; no such causal history was verified. Remove the causal attribution. Agent Teams' verified resumption limitation is specifically for **in-process** teammates.
- **External scale details:** Gastown 20–30 workers, three-minute patrol and Wasteland claims were not needed for this paper's contrast and were not established by the checked metadata. Retain only the documented identity/session separation and supervision.
- **Generalisation:** “generalises to any system” is not tested by one deployment. Scope the lessons to registration/delivery across sessions and identify traffic patterns as operator-specific.
- **Publication state:** “the scripts and aggregate tables are [released]” conflicts with the visible artifact-URL TODO and README's future release language. State intended release and leave a visible artifact-release TODO.

Remaining design assertions were edited for readability, not validated against every current implementation path. In particular the identity table's “proof hierarchy” (which mixes routing, adoption and destructive-operation permissions), current bridge registration timing, reminder timing, and terminal retirement conditions merit an implementation review before submission. These are not established by this measurement inventory.

## Bibliography verification

All 33 entry keys are accounted for below. Primary metadata was checked on 15 October 2026 for paper-specific entries. The 13 shared entries were reused from the explicitly supplied, already-verified router-paper commit `37841cf5`; they were not reconstructed from memory.

| Keys | Verification and corrections |
|---|---|
| `decisionbench2026`, `twinrouterbench2026`, `provenance2026`, `masrouter2025`, `unoorchestra2026`, `orchbench2026`, `routerbench2024`, `llmrouterbench2026`, `sessionrec2018`, `templinkpred2020`, `selectivepred2017`, `cooperbench2026`, `marble2025` | Reused verified entries from `37841cf5:paper/latex/refs.bib`. Restores exact titles/full author lists for the routing papers, LLMRouterBench identifier, CooperBench authors, and article type for Temporal Link Prediction. Uncited shared entries remain available, as in draft 1. |
| `khatua2026cooperbench` | Same verified [CooperBench](https://arxiv.org/abs/2601.13295) metadata under the existing cited key; removes the malformed interior `others` author. |
| `cemri2025mast` | [arXiv](https://arxiv.org/abs/2503.13657) title/authors agree. [NeurIPS proceedings](https://papers.nips.cc/paper_files/paper/2025/hash/b1041e52d3be19f0a9bc491657488e4a-Abstract-Datasets_and_Benchmarks_Track.html) verifies venue. Added primary URL. |
| `pan2026measuring` | [arXiv](https://arxiv.org/abs/2512.04123): full 25-author list; **Riccardo Cogo**, not Filipe; **Marquita Ellis**, not Kevin. Metadata confirms ICML 2026 acceptance. |
| `agarwal2026velocity` | [arXiv](https://arxiv.org/abs/2601.13597): actual title is *AI IDEs or Autonomous Agents? Measuring the Impact of Coding Agents on Software Development*; Shyam Agarwal, Hao He, Bogdan Vasilescu. |
| `mazloomzadeh2026prs` | [arXiv](https://arxiv.org/abs/2607.21832): full title begins *How Do AI Coding Agents Contribute to Software Development?*; Iren Mazloomzadeh, Mohammad Mehdi Morovati, Foutse Khomh. |
| `msr2026survival` | [arXiv](https://arxiv.org/abs/2604.00917): *Investigating Autonomous Agent Contributions in the Wild: Activity Patterns and Code Change over Time*, all six named authors, MSR 2026 technical track. Replaces Anonymous and invented short title. |
| `routerpaper2026` | Title and full author name checked against adjacent manuscript; corrected author spelling and misleading “same journal” note. Retains repository manuscript link. |
| `vibebottleneck2026` | [Author's post](https://prassanna.io/blog/vibe-bottleneck/): title and January 23 date agree; corrected Prassanna spelling. |
| `gastown2026` | [Primary repository](https://github.com/gastownhall/gastown): follows redirect from steveyegge; title/Steve Yegge attribution retained. Removed unverified v1.0/April release note. |
| `agentteams2026` | [Official documentation](https://code.claude.com/docs/en/agent-teams): actual page title and real URL, dated access. |
| `cursor2026` | [Primary post](https://cursor.com/blog/scaling-agents): *Scaling long-running autonomous coding*, **Wilson Lin**, January 14, 2026. Replaces invented title/corporate byline and URL TODO. |
| `li2023camel` | [arXiv](https://arxiv.org/abs/2303.17760): expanded Hasan Abed Al Kader Hammoud; metadata confirms NeurIPS 2023. |
| `hong2024metagpt` | [arXiv](https://arxiv.org/abs/2308.00352): exact title and all 15 authors; ICLR 2024 [primary proceedings record](https://proceedings.iclr.cc/paper_files/paper/2024/hash/6507b115562bb0a305f1958ccc87355a-Abstract-Conference.html). |
| `qian2024chatdev` | [ACL's BibTeX](https://aclanthology.org/2024.acl-long.810.bib): full authors, precise venue, pages and DOI. |
| `wu2023autogen` | [arXiv](https://arxiv.org/abs/2308.08155): full 14-author list; retained 2023 preprint venue/year. |
| `openhands2024` | [arXiv](https://arxiv.org/abs/2407.16741): all 24 authors; updated venue/year to documented ICLR 2025 acceptance. Existing key retained. |
| `ehtesham2025survey` | [arXiv](https://arxiv.org/abs/2505.02279): full protocol-name title and four named authors. |
| `mcp2025survey` | [arXiv](https://arxiv.org/abs/2506.05364): *Survey of LLM Agent Communication with MCP: A Software Design Pattern Centric Review*, Anjana Sarkar and Soumyendu Sarkar. |
| `infiagent2026` | Found the intended [real paper](https://arxiv.org/abs/2601.03204): *InfiAgent: An Infinite-Horizon Framework for General-Purpose Autonomous Agents*, Chenglin Yu, Yuchen Wang, Songmiao Wang, Hongxia Yang, Ming Li. Its abstract explicitly describes file-centric persistent state. Retained with corrected title/authors and **arXiv preprint**, not the unsupported Findings venue. |
| `agentmemory2026` | [arXiv](https://arxiv.org/abs/2606.06448): *Agent Memory: Characterization and System Implications of Stateful Long-Horizon Workloads*, all nine named authors. |

## Representative prose changes

- “The alternative is pull.” → “An agent can instead ask the session working on the relevant repository.”
- “The design was not derived from a model of multi-agent coordination. It was derived from incidents.” → “The design changed in response to operational failures.”
- “The human is the habit engine.” → “Recipient repetition.” The paragraph now reports the two measured rates and their consequence for evaluation.
- “The re-registration storm … is the moment identity is tested” → the directly sourced restart counts, without the unsupported six-day causal claim.

## Validation

Build from `paper/repowire` with TeX Live on PATH: `latexmk -pdf -interaction=nonstopmode -halt-on-error main.tex`. Final PDF: 11 pages. No undefined citations/references or overfull boxes. The ICML style emits one harmless empty-anchor warning at the affiliation notice. No runtime code changed; Go/web gates do not apply. PDF pages were rendered with Poppler for layout inspection. Work remains local as requested.
