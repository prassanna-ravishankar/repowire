# Address-key exposure and traffic context audits

These are retrospective audits of retained records, not a replay of registration rules or a measurement of wrong-session delivery. Python 3.11+ and the standard library suffice. Run from the repository root:

```sh
python3 paper/repowire/analysis/identity_exposure.py --db /path/to/state-copy-2026-10-14.db --starts paper/repowire/analysis/go_daemon_starts.txt --output identity-results.json
python3 paper/repowire/analysis/traffic_context.py --db /path/to/state-copy-2026-10-14.db --output traffic-results.json
python3 -m unittest discover -s paper/repowire/analysis -p 'test_*.py'
```

Both scripts open the completed backup with `mode=ro&immutable=1`. Do not use an actively written database. Outputs are aggregate counts, not message text, paths, names, peer ids or runtime session ids. `results.json` includes the database SHA256; `traffic_results.json` records the second script's results. `go_daemon_starts.txt` contains only the 64 start timestamps extracted by the evidence inventory's daemon_log.py, not raw logs. COACH.md is not part of this artifact.

## Population and keys

The identity window starts 2026-08-10 UTC and ends at the final journal event, 2026-10-14 11:51:36.170 UTC. It contains 4,203 online events for 232 ids. Every online payload is empty, and the event session_id repeats peer_id. The backup has no peers table.

Observed names come from online events, overridden by retained mapping names where present. Sources agree in this backup; no online peer has multiple observed names. Names are outputs of the daemon's suffixing, not independent inputs. Name + circle or exact path uses final peer_session_mappings, covering 72 peers. Reap, stale eviction and unregister delete mappings (daemon-go/peer/registry.go, reconcile.go, registry_lifecycle_byname.go), so coverage has survivorship bias: 38/72 mapped ids are right-censored in the event-only model versus 83/232 overall.

Derived base name uses the sanitized final folder component plus backend, following registry.go's baseFolder, sanitizeFolder and default buildDisplayName branch. Numeric folder suffixes are preserved. Preferred-name inputs cannot be reconstructed; this is an alternative default-name policy. Base name, base + circle and base + exact path share the same mapped cohort.

Backend + runtime session uses final session_bindings with exactly one usable key per peer. We exclude 31 online peers with multiple keys, leaving 171 covered peers. UpsertObservation replaces the owner of a binding matched first by source URI, then by runtime session scoped to backend and project. It therefore loses earlier ownership. created_at and last_seen_at bound observations, not historical assignment validity. The resulting zero is a snapshot artifact, not policy evidence or a claim of universal backend/session uniqueness. Peer id is a tautological uniqueness control. Both belong with structural controls rather than alternative-policy results.

## Intervals and sensitivities

An online event opens an interval, offline or reaped closes it, and repeated online events leave it open. We infer no state before the first observed online event. Intervals are half-open. The event-only model yields 499 intervals and 83 right-censored ids.

The paper reports three scenarios:

1. **Last-observation cap (primary)**: each interval ends no later than the peer's maximum timestamp in events or delivery_traces, using each table's peer_id column. This removes silent tails. A row associated with a peer is not proof of process liveness, and this heuristic can still bridge silence within an interval.
2. **Restart closure**: add a closing boundary for every peer at each daemon start, then rebuild intervals from raw events. Subsequent online events reopen intervals. The naive log times are interpreted using ZoneInfo('Europe/London'), UTC+1 for all 64 starts. The studied host uses Europe/London; the offset was cross-checked against same-peer journal observations. A UTC interpretation is also emitted as a sensitivity (267 rather than 298 eligible observed-name attempts).
3. **Uncapped**: retain open intervals until the last journal event. This is the stale-record upper scenario within this reconstruction, not a statistical upper bound on true live concurrency. Missing metadata can hide competitors.

Restart closure yields 936 intervals and 13 right-censored ids, while preserving all 298 eligible observed-name attempts. Seventy previously unclosed ids acquire a restart boundary. This supports an instrumentation gap, not missing post-restart registrations. **Do not clip already-collapsed intervals at their first restart**: that discards later online events and incorrectly reduces eligibility to 132. A regression test protects this distinction.

Twenty-one peer/timestamp groups contain both opening and closing events. The main model closes and waits for the next unambiguous online event; an additional online-last sensitivity leaves uncapped exposure counts unchanged.

## Attempts, exposure and concentration

Earliest resolved_peer per (kind, trace_id), ordered by timestamp, sequence and row id, defines an attempt: 267 asks and 36 notifications. These include failures. Three created notification traces have no resolved target and are excluded. This is separate from cross-era ask deduplication, which counts 265 new era-B asks.

An exposure requires an eligible target and another interval sharing its computable key. Missing-key peers cannot compete. A target with no observed interval is unknown, not orphaned. Actual misdelivery, orphaning and session splits are output as null, not zero.

| Key | Eligible | Last-observation cap | Restart closure | Uncapped |
|---|---:|---:|---:|---:|
| Observed name | 298 | 0 | 0 | 66 |
| Observed name + circle | 131 | 0 | 0 | 0 |
| Observed name + path | 131 | 0 | 0 | 50 |
| Derived base | 131 | 4 | 4 | 83 |
| Base + circle | 131 | 4 | 4 | 7 |
| Base + path | 131 | 4 | 4 | 83 |

The same four residual attempts survive both sensitivities. They have competitors sharing path and circle but with different observed names. This is consistent with sibling concurrency distinguished by suffixing, not proof of misdelivery. The base-name range [4,83] and observed-name range [0,66] are sensitivity ranges, not confidence intervals or proven process-liveness bounds.

Uncapped base exposure involves nine targets and seven keys. The derived key `orchestrator-claude-code` contributes 47/83; its observed name contributes 47/66. The author requested naming this dominant key; aggregate JSON does not export names. The artifact reports concentration and shared-path/circle counts without peer identifiers.

Concurrent pairs are distinct unordered pairs with positive overlap; sets are distinct complete live groups of at least two members. Pair-overlap seconds sum over pairs, key-collision seconds once per colliding key. Durations accumulate in integer microseconds. Base pair-days are 61.18 capped, 115.76 with restarts and 2,347.22 uncapped. These mostly measure stale registry tails in the uncapped model, not process concurrency. Reused keys count keys associated with multiple ids, including sequential reuse; they are not a session-split rate. Partial-coverage cohorts must not be ranked against full coverage.

## Traffic context

traffic_context.py preserves journal.py's identity classification and cross-era ask reconstruction. It exports no message text. The original journal.py was independently rerun from a temporary copy with read-only database access and redirected outputs; its journal.md matched the inventoried output byte for byte.

For 348 peer-to-peer asks, the ask-only human/orchestrator check gives 230 with no predecessor in 30 minutes. The expanded proxy used in the paper counts human/orchestrator/machine asks and notifications (event rows and resolved notify traces), plus every job_state_changed event naming an assigned executor. It yields 220 with no predecessor at 30 minutes and 184 at two hours. Assigned-job records include status reports, not just initial assignment; notifications may be replies or status rather than dispatch. These are conservative candidate-dispatch associations, not verified causal triggers. The window includes its 30-minute boundary; only preceding asks count. This is a weak initiation proxy: earlier or unrecorded dispatch is invisible. There are 114 ordered sender-id/recipient-id pairs, four on multiple UTC dates. Identity churn can split one logical relationship across pairs. Using the most frequently observed name for each id instead gives 89 alias pairs; 11 recur across UTC dates and carry 134/348 asks. The largest number of distinct active dates for one alias pair is six; its elapsed span is a different statistic, and the maximum elapsed span across pairs is 130 days. Alias recurrence can join different sessions and does not establish carried-over context. Peer-sender later-day repeats keyed by alias are 13/35 versus 3/11 keyed by id.

Both endpoints have a unique retained working path for 224 asks; paths differ in 99. Different paths may be worktrees of the same repository. We do **not** report this as a cross-repository fraction or infer project identity from suffix-stripped names. Historical task boundaries are also absent. These results cannot establish carried-over context.

Peer senders alone repeat recipients in 196/301 opportunities (65.1%). The descriptive iid null draws recipients from each sender's own empirical distribution: sum(p_i squared), weighted by that sender's n-1 opportunities. It is 53.8%, not a randomized experiment or significance test. Mutually exclusive gap buckets are: same UTC day under one hour, 181/269; same day at least one hour, 12/21; later UTC date, 3/11 (including midnight crossings under an hour). Thus 269 of 301 registration-keyed repeat opportunities occur within one hour on the same day: most repetition is conversational bursts. Humans are excluded from this analysis. Their surface counts are 680 Telegram and 18 dashboard, zero Slack; a surface identifies a service, not a physical device.

## Supporting evidence, tests and references

The identity output independently recounts 706 runtime-evidence retention decisions across 33 ids, 226 session_end and 32 transport_failure events among other offline reasons, and five send_failed ask closes. Existing descriptive numbers trace to the evidence inventory's journal.py: J0/J1 for calendar/event/active days; J2 for 137 orchestrator identities and 49 ids behind one name; J3 for request classes and cross-runtime exchanges; J4 for closure.

Eleven synthetic tests cover overlap arithmetic, exact boundaries, missing keys, repeated online events, simultaneous open/close events, censoring, base-name sanitization, sequential reuse, multi-session exclusion, silent-tail capping, restart re-registration, dispatch windows including notifications and job events, alias recurrence across registration churn, and the recipient-frequency null. Full outputs are deterministic across hash seeds.

The task-tree contrast is an organizational pattern, not a market-dominance claim. Official Claude Code subagent and Agent Teams documentation was checked on 16 October 2026; the paper acknowledges resumption and named-subagent messaging.

The registration instrumentation proposal has been filed as [issue #493](https://github.com/prassanna-ravishankar/repowire/issues/493). The local issue draft is retained as background. [Issue #494](https://github.com/prassanna-ravishankar/repowire/issues/494) records the later certificate-renewal attribution defect; it is a limitation, not a result from the journal window.
