# Managed accounts

Status: draft for Prass, 2026-10-27, revision 4 after three Codex reviews. It follows
[managed-machine](managed-machine.md) and
[managed in the mesh](managed-in-the-mesh.md). Nothing here is built
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
Any other key gets 403 and never sees `managed`. This gates use of the
feature, not knowledge of it: the `/managed` page shell is static and public.
This RFC replaces the list with accounts, but the check stays the only place
where the answer is decided.

## Decision

### Accounts, one linked namespace each

- **Account.** `acct_<random>`, created by signing in with an identity
  provider. It holds a verified email, its status (`active`, `suspended`) and
  a Stripe customer id.
- **Identity.** A row of `(provider, subject)` linked to one account. A second
  provider is added only from a signed-in session. Two identities are never
  merged because their verified emails match.
- **Linked namespace.** At most one relay key namespace (`token-...`) per
  account, and at most one account per namespace. The account proves it holds
  the namespace by pasting the key while signed in, which is adequate proof
  for a bearer capability. Linking is how an account's daemons and its managed
  agents meet in the mesh.
- **Session.** A random server-side session id in an `HttpOnly`, `Secure`,
  `SameSite=Lax` cookie. It is rotated on every sign-in. Today's `rw_token`
  cookie carries the relay key itself; account sessions never do.

One namespace per account is a deliberate limit. Daemon ids are unique only
within a namespace: the relay keys connections by `userID/daemonID`. If two
namespaces linked to one account each had a `laptop`, the Worker could not
tell them apart. Both directions identify a machine by bare id, and the
Gateway authorizes remote asks by `from_machine` and `from_peer`. Allowing
several namespaces needs account-scoped machine ids carried in both
directions; that waits until someone needs it.

**The Worker owner stays the namespace.** The Worker addresses agents,
workspaces and the Gateway by owner (`scopedName(owner, id)` and
`GATEWAYS.getByName(owner)`), and each agent persists its owner. Changing the
owner therefore means moving storage. With one namespace per account, the
account does not need to be the owner: it is the authorization and billing
record above it. The mesh paths stay as built, with no mapping table, and
`managedFor` becomes:

```
namespace -> link -> account; require account.status == active
```

The caller is either a session, whose account resolves to its linked
namespace and then to the owner, or a namespace directly (a daemon, an
OAuth grant, a Worker callback), which resolves to an account through its
link. Money is not checked here; the Worker checks it at admission.

**The link is immutable after first use.** An account may link, unlink and
relink freely until it first uses managed: its first agent, its first
purchase or its first credit grant. From then on the link is fixed, and the
relay refuses to change it (409).

This avoids a fencing protocol. Purchases, credits, reservations, open asks
and pending deliveries all live in that one owner's Gateway. If the link could
move while any of them existed:
- a Stripe retry could grant the same purchase into a second owner;
- work admitted just before the move could lose its settlement path;
- a later relink could revive stale work.

A leaked key is handled by suspending the account (`managedFor` refuses it at
once, and the Worker refuses the owner). After that, starting fresh means a
new account and a new key; credits move by an admin refund and a new
purchase. Proper rotation, with a drain-and-fence barrier and a link
generation checked at the Worker, is phase 3 work.

Unlinking does not revoke the key itself. Anonymous relay features (daemon
tunnel, dashboard, shares) still accept it, and the portal says this.

### Sign-in

The relay acts as an OAuth/OIDC client for GitHub, and later Google, using
`golang.org/x/oauth2`, already in go.mod. It needs no new vendor. Each
authorization request carries:
- single-use `state`, stored server-side, bound to a pre-auth browser cookie
  and to its purpose (sign in, or add an identity to the signed-in account);
- PKCE;
- for OIDC providers, `nonce`, issuer and audience validation.

The callback refuses state that is missing, reused, from another browser or
for another purpose. This stops login CSRF, where an attacker's finished
callback signs a victim into the attacker's account and the victim then links
their key there. Mutations (link, unlink, top-up) need the session plus the
existing same-origin check.

Email comes only from the provider's verified address. Magic-link email is
left out for now; it can be added later as one more provider.

### Billing: prepaid credits, reserved at admission

Money comes in at the relay and is spent on the Worker. The Worker's owner
`Gateway` holds the authoritative balance.

**Purchase.**
1. The relay creates a purchase row in SQLite: `pur_<random>`, the account,
   the destination owner (the account's linked namespace, fixed at creation),
   the amount and currency the server chose, and state `created`. An account
   without a link cannot buy.
2. It opens a Stripe Checkout session in `payment` mode with
   `client_reference_id = pur_...` and stores the session id on the row.

The browser never chooses the account or the credit amount.

**Grant.**
- Stripe calls `POST /api/v1/billing/stripe`. The relay verifies
  `Stripe-Signature` and acts on a paid session only:
  - `checkout.session.completed` with `payment_status` `paid`;
  - or `checkout.session.async_payment_succeeded`.
- It loads the purchase by session id and checks the paid `amount_total` and
  `currency` against the row.
- The monetary key is the purchase id, not the Stripe event id. Stripe can
  send duplicates as distinct Event objects, so the event id only goes in the
  audit log.
- The relay calls the Worker
  `POST /admin/owners/{owner}/credits {id: pur_..., credits}`, always for the
  owner stored on the purchase, never the account's current link. The link is
  fixed from the first purchase on, so these are the same in practice.
- The Gateway inserts the credit row and updates the balance in one
  `transactionSync`. The same id with the same amount answers its earlier
  receipt; with a different amount it answers 409.
- The relay returns 2xx to Stripe only after the Worker answers. Any failure
  is non-2xx, so Stripe retries and the same purchase id keeps the retry safe.
  The relay's `created`/`granted` state is bookkeeping for the portal, not
  the dedupe.

**Units.** The owner ledger is kept in credits, where one credit is one
dollar charged to the customer, not dollars of provider cost. Each operation
fixes its price version (cost tables and markup) when it is admitted.
Reservations, settlement and the balance all use credits. Provider cost is
recorded beside them for the admin view.

**Spend: reserve, then settle.** Admission checks the reported spend today
(`Gateway.admit`), and model cost lands only when a run settles. With a small
balance, many concurrent submissions across agents would all pass. Credits
therefore move to reservations, keyed by the identity usage already uses,
`run:<agent>:<operationId>` within the owner's Gateway. Operation ids are
chosen per agent, so the agent is part of the key. A repeated submit with the
same key must carry the same parameters and reuses its reservation;
different parameters are refused.

- **Reserve.** Admission reserves R credits, in one `transactionSync` with the
  submission record, only if `granted - settled - reserved >= R`. R is a fixed
  per-operation reservation from the plan. Otherwise the submission is
  refused with `credits_exhausted`.
- **Spend within R, checked before each step.** The runtime checks before each
  step, not after it:
  - **Model request.** It is sent only if its worst-case charge fits in what
    is left of R. Worst case = input bounded by the request's UTF-8 byte
    length, which is at least its token count, priced as uncached input, plus
    the request's `max_tokens` priced as output.
  - **Exec call.** It is sent only if its fixed worst-case charge fits: the
    shell timeout (45 s) plus a fixed lifecycle allowance L for container
    start, snapshot and teardown. The customer is charged
    `min(metered, 45 s + L)` at the operation's price. Metered time beyond
    that (slow snapshots, cleanup, a Workspace that cannot report) is
    operator overhead, recorded apart from the credit ledger and shown on
    the admin view, never charged.

  When the next step would not fit, the operation ends with
  `reservation_exhausted` and keeps its partial transcript. A billed request
  can therefore never exceed R, since every charge was covered before it was
  incurred.
- **Release.** Settlement replaces the reservation with the actual charge. A
  withdrawn or fenced operation releases its reservation. Uncertain dispatch
  keeps its reservation until recovery (built in phase 2) settles or fences
  it. A reservation is released only on proof that the operation settled or
  never ran.
- **One active operation per agent.** Usage is reported per agent and
  cumulatively today, and the report gives everything since the last report
  to whichever operation settles first (`index.ts` says overlapping runs
  share costs approximately). That cannot settle per-operation reservations
  or price versions. Billing therefore serialises operations per agent:
  - A later submission waits in the agent's queue and reserves only when it
    becomes active. If credits are exhausted at that point, it settles
    `credits_exhausted` without running.
  - All usage between an operation's start and its settlement belongs to it,
    at its price version.
  - Concurrency across agents is bounded by a per-owner cap on active
    operations.


Invariant: settled + reserved never exceeds granted, so a customer can never
spend more credits than they bought or were granted.

**Price.** Credits charged are cost × a markup of at least 1, set in
`prices.ts` next to the cost tables and versioned, so the ledger shows both. The portal shows charged amounts.

Why prepaid rather than a subscription with metered usage:
- No debt to collect, and no invoicing logic.
- A stolen session can spend at most the balance.
- A monthly plan can be added later as a recurring grant through the same
  credits endpoint.

Stripe's own credit products would move the balance off the Worker, which
then could not reserve synchronously.

### Limits

- **Per account.** The daily, monthly and per-agent caps stay as abuse
  guards, read from the account's plan, and are checked against settled plus
  reserved spend.
- **Global breaker.** Charged provider cost is covered by credits bought at
  a markup of at least 1, or granted by an admin. What is not covered is
  operator overhead: container lifecycle time beyond each call's allowance,
  and failure cleanup. That overhead is bounded per call by the Workspace's
  own timeouts, not by credits, so the breaker is a guard against bugs and
  overhead, not the money guard. It does not
  reserve. The `registry` Durable Object sums settled provider cost,
  overhead included, per UTC day. Over the ceiling it opens a breaker: Gateways refuse new admissions with
  `service_capacity`, and the admin is alerted. Work admitted before the
  breaker opened finishes, so the overshoot is bounded by reservations
  already held, each of them covered by credits. The breaker closes at the
  next UTC day or by an admin. A second reservation protocol across two
  Durable Objects would need a cross-object failure and rollover design for
  no added safety.
- **Comps.** A free account gets an admin credit grant (`id: comp_...`), not a
  bypass. Once credits are enforced, `REPOWIRE_MANAGED_USERS` is removed;
  until then it remains the beta gate.

### Storage and deployment

The accounts tables live in the relay's SQLite on a PVC: `accounts`,
`identities`, `namespace_links`, `sessions`, `auth_states`, `purchases` and
`billing_events` (an audit log only). The chart's single-replica, `Recreate`
and PVC requirements currently hang off `oauth.enabled`. They move to a
shared `persistence` setting, which both OAuth and accounts require, and the
chart fails if accounts are on without it.

New secrets go in the `managed.secretName` Secret:
- the GitHub (and later Google) client id and secret;
- the Stripe secret key and webhook secret.

The relay stays on Kubernetes and the agents on Cloudflare. The Worker keeps
trusting only the relay's control token.

### Portal

The pages are served by the relay from the existing web export:
- `/account`: sign in, email, the linked key (link, unlink) and status.
- `/account/billing`: balance, reserved amount, top-up, purchase history and
  usage per agent per day.

`/managed` requires an account session. `/dashboard` and the relay's daemon,
MCP and share features keep working with anonymous keys, unchanged.

## Migration

None. Today's only Worker owner is Prass's test namespace. When accounts
ship, Prass signs in and links that same key, and the owner is unchanged.
There is no owner-move route: owner is part of Durable Object addressing, so
moving one would need a coordinated export and import of agents, workspaces
and the Gateway.

## Phases

1. **Accounts.** GitHub sign-in with state, PKCE and session rotation; the
   namespace link, immutable after first use; suspension; `managedFor` through
   the link. While in beta, only accounts on an invite list are active.
2. **Billing.** Purchases, the Stripe webhook, credits, reservation-based
   admission with per-step pre-checks, the concurrency cap, the global
   breaker, comps, and the billing page. This is the first point where users are
   charged, and every money guard ships in it.
3. **Later.** Per-plan caps, Google sign-in, recurring plans, key rotation
   with drain-and-fence, and several namespaces per account with
   account-scoped machine ids.

## Out of scope, to decide before charging anyone

- The merchant entity, terms of service, privacy policy and refund policy
  (a refund removes unspent credit by a negative grant).
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
