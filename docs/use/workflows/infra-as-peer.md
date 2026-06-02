# Infrastructure-as-peer

A dedicated peer whose project is infrastructure — k8s manifests, Terraform, DNS, helm charts. Other project peers ask it to provision, deploy, or update infra without leaving their own session.

## Why this works

Infra repos are like any other repo from the mesh's perspective: a working directory, an agent in it, knowledge of the code in that directory. The difference is that the *output* of an infra agent is real-world state (a deployed pod, a DNS record), so its workflow is mostly:

1. Receive a request from another peer.
2. Edit infra code or run an `apply`.
3. Confirm the change landed.
4. Notify the requester.

## Setup

Spawn the infra peer in its repo:

```bash
cd ~/infra && claude
```

Give it a clear `set_description`:

```text
set_description("infra peer; owns k8s manifests, helm, cluster DNS")
```

Now any project peer can address it by name.

## Examples

A project peer needing a new namespace:

```text
ask("infra", "I need a namespace 'docs-staging' with image-pull secret X")
```

The infra peer edits its manifests, applies, and acks `"namespace docs-staging up, secret X present, /docs reachable from cluster-internal"`.

A project peer notifying after merging:

```text
notify_peer("infra", "release/0.13.4 merged, ready to deploy")
```

The infra peer picks up the notification and runs the deploy on its own schedule.

## When this beats "just edit the infra repo yourself"

- The asker doesn't have credentials to the infra repo, or the cluster.
- The asker doesn't know the infra repo's layout; the infra peer does.
- You want an audit trail. Every infra change starts from a mesh message that you can search later in the dashboard or events log.

## Guardrails

The infra peer has whatever credentials are in its environment. Treat it like any privileged shell:

- Don't run it under a user with cluster-admin if a narrower role would do.
- Use circles to keep non-infra peers out of accidental contact (`circle: infra`).
- Consider locking down `spawn` on the infra side so other peers can't spawn shells into the infra working tree.

## See also

- [Circles](../../concepts/peers-and-circles.md#circles) for scoping who can address the infra peer.
- [Orchestrator coordination](orchestrator-coordination.md) — the infra peer is often what an orchestrator dispatches to.
