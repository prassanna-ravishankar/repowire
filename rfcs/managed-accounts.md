# Managed accounts

Status: draft for Prass, 2026-10-27. Follows [managed-machine](managed-machine.md)
and [managed in the mesh](managed-in-the-mesh.md). Nothing here is built
beyond the interim gate below.

## Problem

Relay keys are bearer capabilities anyone can mint (`POST /api/v1/register`).
A key gives an isolated namespace (`token-<sha256 of the key>`), which is
right for the relay: tunnelling your own daemon costs nothing and exposes
only what that key's daemons share.

Managed agents cost money (model tokens and container time on our Cloudflare
account). A key authenticates a caller, but it says nothing about who pays.
Until ef614d96, any minted key could create managed agents. Per-owner caps
($2 a day, $20 a month) did not bound the total, because each new key was a
new owner.

To charge users, managed needs authorization based on an account that a
person signed in to and that has a way to pay.

## Interim (built, ef614d96)

`Server.managedFor(userID)` is the single authorization check on every path to
the Worker:
- the managed API and SSE;
- `/me` and sign-in;
- relay MCP;
- daemon `machine_request` and `machine_list`;
- the Worker's callbacks to daemons.

Only user ids in `REPOWIRE_MANAGED_USERS` or `REPOWIRE_ADMIN_USERS` pass it.
Any other key gets 403 and never sees `managed`. This RFC replaces the list
with accounts, but the check stays the only place where the answer is decided.

## Decision

### Accounts are separate from keys

- **Account.** `acct_<random>`, created by signing in with an identity
  provider. It holds a verified email, its status (`active`, `suspended`) and
  a Stripe customer id.
- **Identity.** A row of `(provider, subject)` linked to one account. Several
  providers may sign in to one account.
- **Linked namespace.** A relay key namespace (`token-...`) that the account
  has proven it holds, by pasting the key while signed in. Linking is how an
  account's daemons and its managed agents meet in the mesh. A namespace links
  to at most one account.
- **Session.** A random server-side session id in an `HttpOnly`, `Secure`,
  `SameSite=Lax` cookie. Today's `rw_token` cookie carries the relay key
  itself; account sessions never do.

The owner on the Worker becomes the account id, not the namespace. That way:
- **Rotation.** A leaked key can be revoked by unlinking it and linking a
  fresh one, and the account's agents, runs and balance stay put. With
  owner = namespace, rotating a key would orphan everything.
- **Several keys.** An account may link several namespaces, for example one
  per machine group. Its agents see the daemons of all of them.

The Worker treats the owner as an opaque string, so nothing changes there.
The relay maps between the two:

| Path | Today | With accounts |
| --- | --- | --- |
| Browser and API to Worker | owner = caller's namespace | owner = session's account |
| Daemon `machine_request` to `managed` | owner = daemon's namespace | owner = account linked to the daemon's namespace |
| Worker callback `/machines`, `/machine-request` | daemons of the owner namespace | daemons of every namespace linked to the account |

`managedFor` becomes: resolve the caller (a session, or a namespace through
its link) to an account, then require `active`. Money is not checked here.
The Worker checks it at admission, below.

### Sign-in

The relay acts as an OAuth/OIDC client for GitHub and Google, using
`golang.org/x/oauth2`, already in go.mod. It needs no new vendor. Email
comes only from the provider's verified address. Recommendation: GitHub
first, since the users are developers, then Google.

Magic-link email is left out: it needs an email sender and its
deliverability. It can be added later as one more identity provider.

### Billing: prepaid credits, Worker-side ledger

Money comes in at the relay and is spent on the Worker.

- **Top-up.** The portal opens a Stripe Checkout session in `payment` mode for
  a fixed or custom amount, with the account id in `client_reference_id` and
  metadata. Stripe sends the webhook to `POST /api/v1/billing/stripe`, where
  the relay verifies the `Stripe-Signature` header with the endpoint secret.
- **Grant.** Credit is granted only for a paid session:
  - `checkout.session.completed` with `payment_status` `paid`;
  - or `checkout.session.async_payment_succeeded`.

  Stripe documents that `completed` can arrive before an asynchronous payment
  succeeds. The relay forwards the grant to the Worker as
  `POST /admin/owners/{account}/credits {id: <stripe event id>, usd}`.
  The grant is idempotent on the event id, because Stripe retries webhooks.
- **Spend.** The owner `Gateway` already records every op's cost in its
  ledger. Admission adds one rule: refuse a new submission when
  `granted - spent <= 0`, with the reason `credits_exhausted`. Runs already
  in flight finish, so a balance can dip below zero by at most one run's
  remaining steps (`stepsPerRun`). The next top-up covers it.
- **Price.** The amount charged is cost × a markup, set in `prices.ts` next
  to the cost tables, so the ledger shows both. The portal shows charged
  amounts.

Why prepaid rather than a subscription with metered usage:
- No debt to collect. The Worker already refuses before a run, so this adds
  one comparison and no invoicing logic.
- A stolen session can spend only the balance.
- A subscription or monthly plan can be added later as a recurring grant
  through the same credits endpoint.

Stripe's own credit products (billing credits, Metronome commits) would move
the balance off the Worker, which then could not refuse synchronously. Not
recommended.

### Limits

- **Per account.** The daily, monthly and per-agent caps stay as abuse
  guards, read from the account's plan rather than hard-coded.
- **Global ceiling.** A Worker-wide daily spend total, kept in the existing
  `registry` Durable Object and checked once per admission. Over the ceiling,
  new runs are refused with `service_capacity` and the admin is alerted. This
  covers a pricing bug or a compromised account.
- **Optional free credit.** A small first grant for new accounts (for
  example $1), gated on a provider account older than N days. Off by default.

### Storage and deployment

The accounts tables live in the relay's SQLite on the existing PVC, in the
same database as OAuth: `accounts`, `identities`, `namespace_links`,
`sessions` and `billing_events` (the webhook dedupe log). The chart already
pins `replicaCount: 1` with `Recreate` for OAuth, and the same constraint
covers this. New secrets go in the `managed.secretName` Secret:
- the GitHub and Google client id and secret;
- the Stripe secret key and webhook secret.

The relay stays on Kubernetes and the agents on Cloudflare. The Worker keeps
trusting only the relay's control token.

### Portal

The pages are served by the relay from the existing web export:
- `/account`: sign in, email, linked keys (link, unlink, rotate) and status.
- `/account/billing`: balance, top-up and a history view of the ledger
  (per agent, per day).

`/managed` requires an account session. `/dashboard` and the relay's daemon,
MCP and share features keep working with anonymous keys, unchanged.

## Migration

- `REPOWIRE_MANAGED_USERS` stays as the operator override: listed namespaces
  pass `managedFor` without an account, for comps and testing. It can be
  removed once you have an account.
- Existing Worker owners (`token-...`) are moved to accounts by an admin route
  that re-keys the owner, `POST /admin/owners/{from}/move {to}`. Today that is
  only Prass's test owner, so recreating the agents instead is acceptable.

## Phases

1. Accounts, GitHub sign-in, sessions, namespace linking, owner = account.
   `managedFor` checks for an active account. While in beta, only accounts on
   an invite list are active.
2. Stripe top-ups, the credits endpoint, `credits_exhausted` admission and
   the billing page.
3. Global ceiling and per-plan caps.
4. Google sign-in, optional free credit, recurring plans.

Phase 1 alone closes the "who is this" gap. Phase 2 is the first point where
users are charged.

## Out of scope, to decide before charging anyone

- The merchant entity, terms of service, privacy policy and refund policy.
- Tax (Stripe Tax can collect it, but registration is yours).
- Data retention and deletion of an account's agents and transcripts.

## Open questions for Prass

1. Providers: GitHub only for launch, or GitHub and Google?
2. Prepaid credits as recommended, or a subscription from day one?
3. Markup over cost, and a minimum top-up (Stripe fees make small top-ups
   lossy; $10 is a reasonable floor).
4. Free first credit: yes or no, and how much?
5. Portal host: `relay.repowire.io/account`, or a separate
   `app.repowire.io` in front of the same relay?
