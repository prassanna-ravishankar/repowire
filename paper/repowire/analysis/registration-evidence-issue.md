# Record registration evidence and identity-selection decisions in the journal

Filed by the lead as [issue #493](https://github.com/prassanna-ravishankar/repowire/issues/493). Retained proposal text:

The systems-paper audit cannot replay identity rules from the 14 October state backup. All 4,203 era-B peer_online payloads are empty, and events.session_id repeats peer_id. Final mappings and session_bindings cannot reconstruct event-time inputs. Observed display names are also outputs of collision handling, so they cannot recover the unsuffixed name proposed at registration.

Proposed change: emit a versioned registration-decision record when AllocateAndRegister selects or mints an identity, correlated with peer_online. Include:

- backend, normalized path or stable analysis-safe path key, proposed/default base name, preferred name if supplied, selected display name and circle;
- supplied pid and runtime session id, claimed peer id, and whether runtime identity was verified;
- selected peer id and the rule that fired (live-process holder, explicit claim, session binding, tuple adoption or mint), including relevant rejection/ambiguity reasons and candidate ids;
- retirement state, process-evidence result and observation timestamp, and a decision sequence that orders events sharing a timestamp;
- daemon run id and schema/policy version so restart boundaries and rule changes can be identified.

Do not journal birth-certificate nonces, tokens or message bodies. Analysis exports should replace local paths and runtime identifiers with stable opaque keys. Raw input fields should follow the journal's local retention/access policy.

Acceptance criteria: tests exercise unique tuple adoption, ambiguous candidates, live sibling forks, soft and hard retirement, pid-only registration followed by a session claim, and daemon restart. For each case, journal records should allow an offline reader to reconstruct which evidence was present and which rule selected the id without consulting mutable final-state tables. Preserve absent versus unverified evidence explicitly.

This would enable policy replay. It would not by itself establish actual misdelivery; that additionally needs a transport receipt correlated with the consuming runtime session and independent liveness observations.
