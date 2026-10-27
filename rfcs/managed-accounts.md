# Managed accounts

Status: revision 6, 2026-10-27. Revision 5 was approved by Codex review. Revision 6 follows Prass's feedback:
Clerk for identity, Stripe for every money operation, an operator portal,
and a design track. It is under re-review. It follows
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

- **Account.** A Clerk user (`user_...`). Clerk owns sign-in, identities,
  verified email, MFA and banning. The relay keeps a small row per account:
  status (`active`, `suspended`), its Stripe customer id and its link.
- **Linked namespace.** At most one relay key namespace (`token-...`) per
  account, and at most one account per namespace. The account proves it holds
  the namespace by pasting the key while signed in, which is adequate proof
  for a bearer capability. Linking is how an account's daemons and its managed
  agents meet in the mesh.
- **Session.** Clerk's short-lived session JWT (the `__session` cookie, or a
  bearer token). The relay verifies it without a network call against the
  instance's public key, using `clerk-sdk-go`. Today's `rw_token` cookie
  carries the relay key itself; account sessions never do.

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

### Sign-in: Clerk

Identity is a solved problem, and the product should not carry its own
OAuth client, session store or MFA. Clerk provides:
- hosted sign-in and sign-up (GitHub, Google, email codes, passkeys) and MFA;
- session management, bot protection, banning, and a user dashboard;
- a Go SDK for verifying sessions on the relay.

Its React components work in the static web export through
`@clerk/clerk-react`.

The relay's part:
- **Verify sessions.** Every account route verifies the session JWT with the
  instance's PEM public key (networkless, no Clerk call on the hot path). It
  checks `exp`, `nbf`, the issuer and `azp` (authorized parties: the portal's
  own origin only).
- **Act on Clerk webhooks.** `user.created` creates the relay row (suspended
  until invited during beta). `user.deleted` suspends the account. Banning a
  user in Clerk ends their sessions there; the relay also refuses a banned
  user's next request, through the account row.
- **Admins** are Clerk users whose `public_metadata.role` is `admin`. Only
  Clerk's backend API or dashboard can set it, never the browser. This
  replaces `REPOWIRE_ADMIN_USERS`.
- **Mutations** (link, top-up, every admin action) also pass the existing
  same-origin check.

**What it costs.** A vendor in the sign-in path, and a CNAME for Clerk's
frontend API under `repowire.io` in production. When Clerk is down, people
cannot sign in. Running agents, the mesh and anonymous relay features don't
depend on Clerk.

**Why not Clerk Billing.** It sells recurring subscriptions only, and its
plans are not Stripe Billing objects. Prepaid credits need Stripe directly.
If plans arrive later, they become recurring credit grants, either through
Clerk Billing or Stripe Billing.

### Billing: prepaid credits, reserved at admission

Money comes in at the relay and is spent on the Worker. The Worker's owner
`Gateway` holds the authoritative balance.

Stripe handles every money operation: checkout, receipts and invoices, tax,
refunds, disputes and the customer's payment history. The relay never
moves money. It only turns Stripe's webhooks into credit grants and debits
on the Worker.

**Purchase.**
1. The relay creates a purchase row in SQLite: `pur_<random>`, the account,
   the destination owner (the account's linked namespace, fixed at creation),
   the amount and currency the server chose, and state `created`. An account
   without a link cannot buy.
2. It opens a Stripe Checkout session in `payment` mode for the account's
   Stripe customer, with `client_reference_id = pur_...` and invoice creation
   on, so every top-up gets a Stripe invoice and receipt. Stripe Tax computes
   tax. The session id is stored on the row.

The browser never chooses the account or the credit amount. Stripe's
customer portal, linked from the billing page, shows invoices, receipts and
payment methods. We build none of that.

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

**Refunds and disputes are Stripe operations.**
- **Refund.** An operator refunds in the Stripe dashboard, or from the admin
  portal, which calls Stripe's refund API and changes no balance itself. The
  balance changes in one place: Stripe's refund webhook. Each refund becomes
  a negative credit row keyed by the Stripe refund id (`ref_...`), against the
  purchase's owner, through the same idempotent credits endpoint.
- **Policy.** The default refund is the unspent part of a purchase. A refund
  larger than the remaining balance takes it below zero, which blocks new
  admissions until the account tops up. Reservations already held settle as
  normal.
- **Dispute.** A dispute (`charge.dispute.created`) suspends the account and
  alerts the admin. A lost dispute is debited like a refund. A won dispute
  needs no ledger change, and the admin lifts the suspension.

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
- **Global breaker (best effort).** Charged provider cost is covered by
  credits bought at a markup of at least 1, or granted by an admin. Operator
  overhead is not covered: container lifecycle time beyond each call's
  allowance, and failure cleanup.
  - **How it works.** The `registry` Durable Object sums settled provider
    cost per UTC day, overhead included. Over the ceiling it opens a breaker:
    Gateways refuse new admissions with `service_capacity`, and the admin is
    alerted. The breaker does not reserve, and it closes at the next UTC day
    or by an admin.
  - **What it bounds.** Overshoot past the ceiling is the charged cost of
    work already admitted, which reservations cap, plus that work's overhead.
    Today nothing bounds the overhead: `workspace.ts` has a 45 s shell timeout
    and an inactivity timeout, but no deadline over the whole container
    lifecycle (start, snapshot, teardown).
  - **Phase 2 work.** A whole-lifecycle deadline per exec call that
    terminates the container when it passes, and records a failed snapshot
    as a failed call. Overhead per call is then finite, and the breaker's
    overshoot is bounded by reservations plus admitted calls × that deadline.
  - **Why not a second reservation protocol.** Reserving across two Durable
    Objects would need a cross-object failure and day-rollover design, and
    it still would not bound overhead.
- **Comps.** A free account gets an admin credit grant (`id: comp_...`,
  minted by the relay, with a reason and the admin's id in the audit log), not
  a bypass. Once credits are enforced, `REPOWIRE_MANAGED_USERS` is removed;
  until then it remains the beta gate.

### Storage and deployment

The relay's tables live in its SQLite on a PVC:
- `accounts`: Clerk user id, status, Stripe customer and link;
- `purchases`;
- `admin_actions`: an append-only audit log of credits, refunds and
  suspensions;
- `billing_events`: an audit log of Stripe and Clerk webhooks.

Clerk holds identities and sessions. The chart's single-replica, `Recreate`
and PVC requirements currently hang off `oauth.enabled`. They move to a
shared `persistence` setting, which both OAuth and accounts require, and the
chart fails if accounts are on without it.

New secrets go in the `managed.secretName` Secret:
- the Clerk secret key, the session PEM public key and the webhook signing
  secret;
- the Stripe restricted key and webhook secret.

The relay stays on Kubernetes and the agents on Cloudflare. The Worker keeps
trusting only the relay's control token.

### Customer portal

The relay serves these pages from the existing web export, with Clerk's
components for sign-in and the user menu:
- `/account`: profile (Clerk's `UserProfile`), the linked key and status.
- `/account/billing`: balance, the amount reserved by running work, top-up,
  usage per agent per day, and a link to Stripe's customer portal for
  invoices and payment methods.
- `/managed`: the agents. It requires a signed-in, active account.

`/dashboard` and the relay's daemon, MCP and share features keep working with
anonymous keys, unchanged.

### Operator portal

`/admin` already exists, with the Worker summary, owners, sandboxes, usage
and a costs view that sets the estimate against Cloudflare's actual spend
(`/api/v1/admin/costs`). It grows into the place where the business is run:
- **Accounts.** Search by email or user id. Each row shows Clerk user,
  linked namespace, status, balance, reserved, lifetime purchased and spent,
  and recent runs. Actions: suspend or reactivate, and open the account in
  Clerk or Stripe.
- **Credits.** Issue a comp or goodwill grant with an amount, reason and
  optional expiry. Each grant is a `comp_...` row with the admin's id in
  `admin_actions`. Credits can't be edited; a correction is another grant.
- **Refunds.** Refund a purchase. This calls Stripe; the ledger follows from
  the webhook, as above.
- **Finance.** Per day and per month:
  - revenue: Stripe gross, fees, refunds and net, from balance transactions;
  - infra spend: Cloudflare actual, model cost from the ledger, and operator
    overhead;
  - margin, credits outstanding (sold but not yet spent, a liability), and
    the global breaker's state.
- **Breaker and limits.** See the ceiling, open or close the breaker, and
  change plan caps.

Every admin action needs an admin session plus the same-origin check, and
writes to `admin_actions` before it takes effect.

### Design language

Managed is a product, and the landing page, docs, dashboard, `/managed`,
`/account` and `/admin` should read as one. That is a separate workstream,
with its own design doc, that this RFC depends on. Its scope:
- brand: name usage, logo and mark, voice;
- tokens: colour, type, spacing, radius, elevation and motion, for light and
  dark;
- components: shared across every surface;
- theming of third-party surfaces: Clerk's `appearance` API, and Stripe
  Checkout and the customer portal's branding settings.

The web app already has a token layer (`globals.css`, the dashboard's
surface and on-surface tokens), which is the starting point.

## Migration

None. Today's only Worker owner is Prass's test namespace. When accounts
ship, Prass signs in and links that same key, and the owner is unchanged.
There is no owner-move route: owner is part of Durable Object addressing, so
moving one would need a coordinated export and import of agents, workspaces
and the Gateway.

## Phases

0. **Design system**, in parallel with phase 1: brand, tokens and core
   components, then applied to the existing pages.
1. **Accounts.** Clerk sign-in and session verification on the relay, Clerk
   webhooks, the namespace link (immutable after first use), suspension,
   `managedFor` through the link, admins by Clerk role, and the accounts view
   of the operator portal. While in beta, only invited accounts are active.
2. **Billing.**
   - Purchases through Stripe Checkout (invoices and Stripe Tax), the Stripe
     webhook, credits, and refunds and disputes through webhooks.
   - Reservation-based admission with per-step pre-checks, one active
     operation per agent, and the concurrency cap.
   - The whole-lifecycle exec deadline and the global breaker.
   - Comps, the billing page and the operator portal's credits, refunds and
     finance views.

   This is the first point where users are charged, and every money guard
   ships in it.
3. **Later.** Per-plan caps, recurring plans, key rotation with
   drain-and-fence, and several namespaces per account with account-scoped
   machine ids.

## Out of scope, to decide before charging anyone

- The merchant entity, terms of service, privacy policy, and the refund
  policy's wording (the mechanics are above).
- Tax registration (Stripe Tax calculates and collects once you are
  registered).
- Data retention and deletion of an account's agents and transcripts.

## Open questions for Prass

1. Prepaid credits as recommended, or a subscription from day one?
2. Markup over cost, and a minimum top-up (Stripe fees make small top-ups
   lossy; $10 is a reasonable floor).
3. Free first credit: yes or no, and how much?
4. Portal host: `relay.repowire.io`, or `app.repowire.io` in front of the
   same relay? This also places Clerk's frontend API CNAME.
5. Sign-in methods to enable in Clerk at launch: GitHub only, or GitHub,
   Google and email?
