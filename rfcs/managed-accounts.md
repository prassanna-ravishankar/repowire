# Managed accounts

Status: revision 8, 2026-11-24. Revision 5 was approved by Codex review.
Revision 6 followed Prass's feedback: Clerk for identity, Stripe for every
money operation, an operator portal, and a design track. Codex's reviews of
revisions 6 and 7 requested changes. Revision 8 answers both:
- Stripe and Clerk events trigger a re-read of current state. They are
  never applied from their payloads.
- Reconciliation is serialised per lot, complete, versioned and recovered on
  request.
- Holds are consumed atomically with the reversal they become.
- One balance equation replaces separate debt.
- Every outbound and admin action is durable, with stable ids and
  exhaustive lookup.
- The link is frozen before first use.
- Access derives from per-source blockers.
- Owner status at the Worker is a short lease, with a durable abort list.
  Settlement stays open at the Worker and at the relay.
- Comps are removed (revision 8 dropped their expiry), and relay records get retention.

Prass's decisions (2026-11-24) are folded in:
- prepaid credits;
- no free credits, so comps are gone;
- markup and minimum top-up are config values with neutral defaults;
- every Clerk sign-in method at launch;
- `app.repowire.io` as the default portal host, pending his confirmation.

What remains open is listed at the end.

It follows
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
`managedFor` becomes two answers (see "Suspension at the Worker"):

```
admitFor:  namespace -> binding -> account; require no open blocker
settleFor: namespace -> binding (any state, tombstones included)
```

The caller is either a session, whose account resolves to its linked
namespace and then to the owner, or a namespace directly (a daemon, an
OAuth grant, a Worker callback), which resolves to an account through its
link. Money is not checked here; the Worker checks it at admission.

**The link is immutable after first use.** An account may link, unlink and
relink freely until it first uses managed: its first agent or its first
purchase. From then on the link is fixed, and the
relay refuses to change it (409).

Freezing is atomic, and it comes first. A link is a row in
`namespace_bindings(namespace PRIMARY KEY, account UNIQUE, state)`, with
state `linked`, `frozen` or `tombstoned`.
- **Freeze.** Every first-use path runs one SQLite transaction that moves the
  binding from `linked` to `frozen`, compare-and-set on the namespace the
  caller read. The purchase row or the agent-create action
  is written in that same transaction, before anything is sent to Stripe or
  the Worker. A concurrent unlink or relink loses the compare-and-set and
  gets 409. A first use that loses gets 409 and retries against the current
  link.
- **Uncertain outcomes.** A frozen binding stays frozen even when the
  external step's outcome is uncertain (a Worker timeout, a Stripe error).
  Recovery settles the action, not the binding.
- **Tombstones.** Deleting an account tombstones its binding, and the
  namespace can never be linked again. A namespace's Worker data outlives
  the account, so a re-link would revive it.
- **Uniqueness.** It holds in both directions: the primary key on the
  namespace, and the unique account.

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
- hosted sign-in and sign-up (GitHub, Google, email codes, passkeys) and MFA.
  Every method Clerk offers is enabled at launch (Prass, 2026-11-24);
- session management, bot protection, banning, and a user dashboard;
- a Go SDK for verifying sessions on the relay.

Its React components work in the static web export through
`@clerk/clerk-react`.

The relay's part:
- **Verify sessions.** The relay accepts only Clerk session tokens: never
  OAuth, M2M or API-key tokens, and never a relay key on an account route.
  It verifies them with `clerk-sdk-go` against the instance's PEM public key,
  with no Clerk call on the hot path. It checks:
  - `exp`, `nbf`, the issuer and `azp`, which must be the portal's own
    origin, `https://app.repowire.io` by default (see "Still open");
  - that the session status claim is `active`, so a pending session (an
    unfinished MFA or organisation step) is refused.
- **Revocation latency.** A revoked or banned session stops at the relay
  when its token expires. Two deployment requirements, which the relay
  checks at startup through the instance settings: the session token
  lifetime is at most 60 seconds, and clock skew is tolerated up to 5
  seconds. Every
  privileged mutation also re-checks the session against Clerk's Backend
  API: the link, a top-up, a refund, every admin action. For those,
  revocation is immediate.
- **Webhook trust.** Clerk webhooks (Svix) are verified on the raw body:
  signature, timestamp tolerance, and the signing secret. Each event is
  recorded once, keyed by its `svix-id`.
- **Webhooks trigger; Clerk's current state decides.** A `user.*` event is
  never applied from its payload. The relay re-reads the user from Clerk's
  Backend API and applies what it finds, so order, duplicates and lost
  events cannot leave the wrong state:
  - an unknown user is created from what Clerk returns (an upsert, so an
    update that arrives first bootstraps the row);
  - a 404 means deleted.

  Each user's read-and-apply cycle is serialised under a per-user lease
  in SQLite, as reconciliation is for lots. A trigger that finds the lease
  held marks the user dirty, and the holder reads again after it commits.
  Two reads therefore never commit out of order.

  Deletion is final whatever arrives later. Its tombstone keeps the user
  id, so no later event can revive the account or reset its invitation.
  The same re-read runs on the account's next session and on the operator
  portal's account view. A lost webhook is repaired by the next request,
  never by a poll.
- **Access is derived from independent blockers.** Each blocker is a row in
  `account_blocks`, keyed by its source:
  - `clerk:banned`, `clerk:locked`, `clerk:deleted`;
  - `invite:pending`;
  - `operator:<action id>`;
  - `dispute:<dispute id>`;
  - `security:<action id>`.

  An account is active only when it has no open blocker. Each blocker is
  opened and cleared by its own source: an operator action clears only its
  own operator blocker, and only Stripe's resolution of that dispute clears
  a dispute blocker. Clearing one therefore never overrides another. Every
  change to an account's blockers, in the same transaction, recomputes the
  owner status, bumps its version and queues its propagation to the Worker
  (below).
- **Admins.** The role comes from Clerk's `public_metadata.role`, carried
  into the session token by the instance's session-token template. Only
  Clerk's backend API or dashboard can set it, never the browser. This
  replaces `REPOWIRE_ADMIN_USERS`.
  - Every admin route needs a Clerk session with that claim. A relay key,
    an OAuth grant or a namespace credential never carries admin
    authority, whoever holds it.
  - Admin mutations re-read the role from the Backend API, so a demotion
    takes effect at once for mutations, and within one token lifetime for
    reads.
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

**Money in integer units, tax apart.** Amounts are integer minor units of
one currency, set per deployment. Credits are integer micro-credits: one
credit, or 1,000,000 micro-credits, is one unit of the currency charged to
the customer. Tax is never credit: a purchase stores its principal, the
credits it buys, its tax and its total separately.

**Purchase.**
1. The relay creates a purchase row in SQLite: `pur_<random>`, the account,
   the destination owner (the account's linked namespace, frozen in this
   same transaction), the principal and currency the server chose, the
   credits it buys, and state `created`. An account without a link cannot
   buy.
2. It opens a Stripe Checkout session in `payment` mode for the account's
   Stripe customer, as a durable action (below): one line item priced at
   the principal, `client_reference_id = pur_...`, and invoice creation on,
   so every top-up gets a Stripe invoice and receipt. Stripe Tax computes the
   tax, exclusive of the principal.
3. A purchase has at most one open session. A retry first recovers the
   session the action may already have created. Only once that session has
   expired, or has been expired through the API, does the relay open a new
   one.

The browser never chooses the account or the credit amount. Stripe's
customer portal, linked from the billing page, shows invoices, receipts and
payment methods. We build none of that.

**Grant: one credit lot per paid session.**
- Stripe calls `POST /api/v1/billing/stripe`. The relay verifies
  `Stripe-Signature`, and records the event id once, for audit only.
- **Events trigger; Stripe's current state decides.** A money event marks
  its lot dirty (`lots.dirty_since`) and starts a reconciliation of that
  lot. The event's payload is never applied.
- **The lot.** A paid Checkout session (`payment_status` `paid`) is a credit
  lot keyed by the session id. The relay persists purchase, session,
  PaymentIntent and charge, and checks Stripe's `amount_subtotal` against
  the principal and `currency` against the row. Tax (`total_details`) is
  recorded and never granted. A second paid session for one purchase is
  still a lot of its own, so a payment is never left without credit.

**Reconciliation is serialised per lot and complete, or it publishes
nothing.**
1. **Claim.** A reconciler claims the lot's lease in SQLite
   (compare-and-set on `lease_until`), and clears `dirty_since` in the same
   transaction. A trigger that finds the lease held only marks the lot
   dirty. The holder runs again after it commits, so no observation is lost
   and two are never interleaved.
2. **Fetch.** It fetches exhaustively, through every page:
   - the session, the PaymentIntent and the charge;
   - every refund on the charge (`GET /v1/refunds?charge=`), not the
     charge's embedded list, which stops at ten;
   - every dispute on the charge.

   Any failed or partial fetch abandons the pass. The lot stays dirty, and
   nothing is published.
3. **Compute.** It computes the lot's reversed principal from what it
   fetched:
   - a refund counts while it is `pending` or `succeeded`, and stops when
     it is `failed` or `canceled`;
   - a dispute's amount counts while the dispute is open or lost, and stops
     once it is won;
   - the total is capped at the principal. Stripe refuses to refund a
     disputed amount, so refunds and disputes do not overlap, and the cap
     makes it safe if they ever did.
4. **Commit.** It writes the result and increments the lot's version, in
   one transaction, and only while it still holds the lease.

**From cash to principal.** Stripe's refund and dispute amounts include
tax, and the lot holds principal. Cash converts to principal in proportion,
computed from the lot's cumulative reversed cash:

```
reversed principal = round_half_up(cumulative reversed cash × principal / total paid)
```

If the cumulative reversed cash equals the total paid, the reversed
principal is exactly the principal. For example, £60 refunded from £100
principal plus £20 tax reverses £50 of principal.

**Entitlement, rounded once per lot.** A lot's reversed credits are
`ceil(credits × reversed principal / principal)` in micro-credits,
computed from the cumulative reversed principal, never summed refund by
refund. Many partial refunds therefore never reverse more than the lot's
credits, and a full refund reverses exactly all of them. The lot's
entitlement is its credits minus its reversed credits.

**To the Worker.** The relay sends the lot's absolute state:
`PUT /admin/owners/{owner}/lots/{session} {credits, entitlement, refunds,
version}`.
- The owner is the one stored on the purchase.
- `refunds` lists each refund with its `metadata.action_id`, its state and
  the credits it reverses. Those credits are the difference its cumulative
  principal makes to the lot's rounded reversal.
- The Gateway applies the update only if the version is newer.
- **In the same `transactionSync`, holds are consumed.** A refund that
  names an action and is `pending` or `succeeded` consumes that action's
  hold. A `failed` or `canceled` one releases the hold. The entitlement
  update and the hold change land together, so a refund is never both held
  and reversed, and never neither.
- The relay records the version the Worker acknowledged (`worker_version`).
- It returns 2xx to Stripe only after the Worker acknowledges. Any failure
  is non-2xx, so Stripe retries.

**Recovery from lost events, by request.** Stripe's retries are finite, so
the relay also keeps its own record of what is unfinished: dirty lots, and
lots whose `worker_version` is behind their version. Both are worked off in
small batches on requests that care: the account's billing page, the
operator portal, and the Stripe webhook route itself.
- The operator portal also reads Stripe's Events API from a stored cursor.
  It is a catch-up for events that never arrived (Stripe keeps 30 days of
  them), and it runs as part of a request, never on a timer.
- A lot still unreconciled after that shows in the portal with a replay
  action.

**Refunds are durable actions with a credit hold.**
- **From the admin portal.** An operator refund is given in cash, in the
  currency's minor units, which is what Stripe accepts.
- **The hold.** The relay computes the hold exactly: the increase that
  amount's principal share makes to the lot's rounded reversal, with the
  lot's lease held. Then it holds those credits at the Gateway:
  `POST .../lots/{session}/holds {id: act_..., credits}`. In one
  `transactionSync`, the hold succeeds only if two things cover it: the
  lot's refundable credits, and the owner's `available` balance. A
  voluntary refund can therefore never take back credits that are
  repaying a shortfall. Dashboard refunds and disputes are not voluntary;
  they can still make `available` negative, as described below.
- **Then Stripe.** The relay calls Stripe's refund API as the same action,
  with `metadata.action_id`. The hold ends only through reconciliation,
  consumed or released atomically as above, or through the action failing
  for certain.
- **Refundable credits.** A lot's refundable credits are its entitlement,
  minus what has been spent from it, minus what is held or reserved against
  it. Spending is allocated deterministically:
  - admission draws from lots in a fixed order, oldest purchase first;
  - settlement charges the lots the reservation drew from;
  - a lot's spent amount is the sum of the charges against it.
- **From Stripe's dashboard.** A refund issued there needs no hold. It
  reaches the lot through reconciliation, like any other refund.
- **Dispute.** `charge.dispute.created` opens the `dispute:<id>` blocker and
  alerts the admin. The disputed amount leaves the lot's entitlement while
  the dispute is open, comes back if it is won, and stays out if it is
  lost. Stripe's resolution of that dispute clears its blocker.

**One balance equation, no separate debt.** At the Gateway:

```
available = Σ lot entitlements − spent − reserved − held
```

`spent` is the owner's settled charges. There is no separate debt field and
no negative lot: a lot's entitlement floors at zero, and a shortfall shows
as a negative `available`. Any reversal makes it more negative while it
counts: a dashboard refund, a pending refund, an open or lost dispute. A
top-up raises the sum of entitlements, which repays the shortfall before
anything becomes spendable. A won dispute or a failed refund restores
entitlement the same way. A released reservation returns only what it did
not spend. So one equation covers every transition, and nothing is counted
twice.

**Durable outbound actions.** Every call the relay makes to Stripe is a row
in `billing_actions` before it is made: creating a Checkout session,
expiring one, refunding. The row holds:
- the action id `act_...`;
- the kind and the request parameters, immutable once written;
- the state: `requested`, `succeeded`, `failed`, `uncertain` or
  `unresolved`;
- a lease, and the Stripe object id once it is known.

The action id is the Stripe `Idempotency-Key`, and the metadata on the
created object. The browser gets the action id when the form loads and sends
it with the request, so a resubmitted form names the same action. A request
whose parameters differ from the stored ones is refused (409).
- **One executor.** An action runs only under its lease. A `requested` row
  whose lease has passed (the relay crashed mid-call) becomes `uncertain`.
- **Uncertain outcomes.** A timeout, a 5xx, a lost response or a crash
  leaves the action `uncertain`. Before anything is created again, the
  relay looks the object up, exhaustively and through every page:
  - **Checkout sessions:** the customer's sessions created since the
    action, matched on `metadata.action_id`. The list API cannot filter by
    `client_reference_id`, so the match is local.
  - **Refunds:** the charge's refunds, matched on `metadata.action_id`.
  - **Expiring a session:** retrieving the session and reading its status;
    an expired session is success.
- **Past Stripe's window.** Stripe keeps idempotency keys for 24 hours.
  Past that window, recovery uses the lookup only, and never repeats the
  call. If the lookup cannot establish an outcome, the action becomes
  `unresolved`. That blocks further actions of its kind on the same lot or
  purchase, and the operator resolves it in the portal against Stripe's
  dashboard.

**Admin actions follow the same model.** Suspensions, breaker and
limit changes, and refunds are rows in `admin_actions`, with the requested
parameters, the admin's id, the state (`requested`, `succeeded`, `failed`,
`uncertain`) and the external ids.
- **Stable ids.** Each action's id is also the id of its downstream
  operation: a refund carries it as `metadata.action_id`, and a status
  change carries its owner-status version. A retry reaches the same operation,
  and a retry with different parameters is refused (409).
- **Recovery.** It reads back the Worker's state for that id and finishes
  or settles the action. It runs on the operator portal's next request and
  on the account's next request.
- A row records what was attempted and how it ended. It is not, by itself,
  proof that the action took effect.

**Units.** The owner ledger is kept in micro-credits charged to the customer,
not in provider cost. Each operation fixes its price version (cost tables
and markup) when it is admitted. Reservations, holds, settlement and lots all
use micro-credits. Provider cost is recorded beside them for the admin view.

**Spend: reserve, then settle.** Admission checks the reported spend today
(`Gateway.admit`), and model cost lands only when a run settles. With a small
balance, many concurrent submissions across agents would all pass. Credits
therefore move to reservations, keyed by the identity usage already uses,
`run:<agent>:<operationId>` within the owner's Gateway. Operation ids are
chosen per agent, so the agent is part of the key. A repeated submit with the
same key must carry the same parameters and reuses its reservation;
different parameters are refused.

- **Reserve.** Admission reserves R credits, in one `transactionSync` with
  the submission record, only if `available` (above) covers it. That
  includes the owner's status generation (below): a reservation made under
  a suspended or stale status is refused.
  - The reservation records which lots it draws from, in the allocation
    order above.
  - R is a fixed per-operation reservation from the plan.
  - Otherwise the submission is refused with `credits_exhausted`.
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


Invariant (an admission guarantee): a reservation is admitted only if
`available` is at least R. So no admitted work spends credits that were not
bought. A reversal that lands afterwards can still make
`available` negative. That shortfall is the owner's alone, and it blocks
admission until entitlements cover it.

**Price.** Credits charged are cost × a markup of at least 1. The markup is a
relay config value (`billing.markup`), default 1, so credits are charged at
cost. It is versioned with the cost tables in `prices.ts`, so the ledger
shows both, and the portal shows charged amounts. The minimum top-up is a
config value too (`billing.min_topup`), defaulting to Stripe's own minimum
charge for the currency. These defaults are placeholders, not neutral in
money terms. At cost, Stripe's fee (about 2.9% + $0.30) comes out of the
operator's pocket on every top-up: a $0.50 top-up loses about 60% of its
value. Both must be set before anyone is charged ("Still open").

**No free credits.** Every lot is a paid Checkout session. There are no
comps, trial credits or goodwill grants, and admins cannot mint credit.
Goodwill is a refund. Adding grants later would be a new lot source, with
its own durable action.

Why prepaid rather than a subscription with metered usage:
- No invoicing logic. A shortfall arises only from a reversal that lands
  after the credits were spent, and it blocks admission until covered.
- A stolen session can spend at most the balance.
- A monthly plan can be added later as a recurring lot through the same
  lots endpoint.

Stripe's own credit products would move the balance off the Worker, which
then could not reserve synchronously.

### Suspension at the Worker

`managedFor` refuses a suspended account at the relay at once, but the Worker
must enforce it too. Queued operations, managed-to-managed asks and reply
deliveries start work without another relay check.

**Owner status is a lease.** The Gateway holds `{state, version,
valid_until}`. The relay sets it through `PUT /admin/owners/{owner}/status`.
The version is the per-account counter bumped with every blocker change, and
an older version changes nothing.
- **What the lease means.** An `active` status authorises admission only
  until `valid_until`, 15 minutes after it was issued.
- **Renewal on demand.** An admission that finds the lease expired asks the
  relay for a fresh status, `GET /api/v1/worker/owners/{owner}/status` with
  the control token, and admits under that.
- **If the relay can't be reached,** new admission fails closed with
  `authorization_unavailable`. A suspension the Worker never received
  therefore stops new work within one lease, even if nothing ever touches
  the account at the relay again.
- **Propagation.** The relay records the desired status first, then sends
  it. A failed send is retried on the account's next request and on every
  renewal. The operator portal shows the status as pending until the Worker
  acknowledges it. This is lazy repair, never a poll.

**Admission gates, in order.** Every admission point applies them in this
order. The admission points are:
- relay requests, and creates on the operator's direct daemon link;
- remote inbound creates;
- managed-to-managed asks;
- a queued operation becoming active;
- a reply delivery that would start a run.

1. **A known operation id answers its existing receipt first,** with the
   parameter-conflict check, as `managed-machine.md` and
   `managed-in-the-mesh.md` require. Suspension never makes a recorded
   operation look unaccepted.
2. **Then the status gate.** A new operation under a suspended or stale
   status is refused with `owner_suspended`, before anything is recorded.
3. **Accepted work that cannot start** gets a durable terminal disposition
   instead: a queued operation that would become active settles as
   `owner_suspended`, and its reservation is released, since it never ran.
4. **Replies due to a suspended owner's agents** are held, not dropped.
   They are delivered on reactivation, or expire with the cross-machine
   window.

**Stopping running work is durable.** Status lives in the Gateway, and
execution in each agent's Durable Object.
- **The abort worklist.** Suspension writes an abort row for every active
  operation in the same transaction as the status. The rows are worked off
  on the Gateway's request paths (reports, connects, admissions), and on
  the next status renewal. Each row is cleared only once that operation's
  terminal report lands.
- **Agents check the status generation too.** Before each step, the
  per-step pre-check that already guards the reservation compares the
  owner's status generation. An agent that missed its abort, or was evicted
  and restarted, stops at its next step.
- **Settling aborted work.** An aborted operation settles as aborted and
  is charged what its terminal usage shows. Its reservation is released
  only once that terminal report lands. Starting the abort is not proof
  that the work has settled.

**Settlement stays open, at the Worker and at the relay.** Suspension
denies new authority, never the completion of work recorded before it.
- **At the Worker.** While an owner is suspended, it still allows:
  - reading existing records;
  - `/remote/settled` pushes and read-backs;
  - recovery passes;
  - report delivery to the daemon;
  - lot reconciliation.

  None of these start billed work.
- **At the relay.** `managedFor` splits into two answers.
  - `admitFor` requires an active account. It gates the managed API, SSE,
    relay MCP sends and machine listing.
  - `settleFor` requires only that the namespace has, or had, a binding,
    tombstoned ones included. It gates the Worker's callbacks and the
    daemons' `/remote/*` calls that settle or read back existing records,
    in both directions.

  The callback route parses the operation before it authorises, so only
  settlement and read-back go through `settleFor`. A new create still
  needs `admitFor`.
- **Streams.** The relay closes a suspended account's managed SSE
  subscriptions, and the Gateway closes its `ui` sockets.

### Limits

- **Per account.** The daily, monthly and per-agent caps stay as abuse
  guards, read from the account's plan, and are checked against settled plus
  reserved spend.
- **Global breaker (best effort).** Charged provider cost is covered by
  credits bought at a markup of at least 1. Operator
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
- **The beta gate.** Once credits are enforced, `REPOWIRE_MANAGED_USERS` is
  removed; until then it remains the beta gate. There is no free tier: an
  invited account still buys credits before it can run anything.

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
trusting only the relay's control token. The Worker's direct daemon
credential (`HOSTED_TOKEN`) belongs to the operator's own owner
(`HOSTED_OWNER`), and is never issued to customers. It is subject to the
same owner status as everything else.

**Retention is per account.** An account's data, its agents, transcripts,
workspaces and records, belongs to the account. So does the policy that
expires it.
- The policy (`content_days`, `records_days`, with the defaults from
  `daemon-go/proto/retention.json`) is set on the account at the relay.
- The relay sends it to the account's Gateway on the owner-status channel,
  with the status version.
- The Gateway sweeps by it, and so does each agent and workspace Durable
  Object (`rfcs/retention.md`, "Hosted"; #587).
- Until accounts exist, the Gateway keeps each connected daemon's policy and
  sweeps by the most conservative of them (#600), so no machine can shorten
  retention for another.

**Account deletion is required, before anyone is charged.** It is the
first privacy question a paying customer asks. Deleting an account, through
Clerk's `user.deleted` or an operator action, is a durable admin action.
Each step is recovered on request until done:
1. The account's blockers close admission at once, through the
   owner-status lease and abort worklist above.
2. Once nothing is running or held, the Worker deletes the owner's agents
   with their conversations and per-operation records, its workspaces with
   their snapshots, and its Gateway content (runs, requests and
   cross-machine records).
3. Kept as the law requires: the ledger (lots, usage, versions, refund and
   dispute ids) and the dedupe tombstones, as under "Retention of relay
   records" below. They hold no conversation content.
4. The relay records each step's acknowledgement. The account shrinks to
   its tombstone only when every step is done.

**Retention of relay records.** The relay's store follows the same model as
the daemon's (`rfcs/retention.md`):
- Each table declares its retention beside its queries, and a test fails
  for a table without a declaration.
- The policy is the relay's `retention` config, with defaults from
  `daemon-go/proto/retention.json`.
- The sweep runs at most hourly, on a request that finds one due.

By class:
- **Dedupe and identity, kept forever.** These are compact: lot and
  purchase ids, lot versions, refund, dispute and action ids with their
  amounts and outcomes, and the tombstones of users and namespace bindings.
  Without them a replayed Stripe or Clerk event could grant or revive
  something.
- **Financial detail,** kept for the operator's legal retention period:
  invoices are referenced, never copied. The period depends on the merchant
  entity, which is out of scope below.
- **Records** (`records_days`):
  - `billing_events`, which keeps only each event's id, type, object id,
    time and outcome, never a raw payload;
  - abandoned purchases whose sessions all expired unpaid;
  - the request parameters of terminal actions, compacted to their ids,
    amounts and outcome.
- **Exempt while live:** dirty or unacknowledged lots, holds, `requested`,
  `uncertain` and `unresolved` actions, pending status propagation, and any
  account with reservations or held credits at the Worker. Age never removes
  live state.
- **Account deletion.** Personal fields go at once: the Stripe customer
  link is detached, and the relay never held email. The routing and status
  state stays until nothing is unresolved: the binding, the owner status,
  its version and pending propagation, and unresolved actions. That lasts
  until the Worker has acknowledged the final status, every action is
  terminal, and no reservation or hold remains. Then the account shrinks to
  its tombstone.

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
- **Refunds.** Refund credits from a lot. The credits are held first, then
  Stripe is called, and the lot follows from reconciliation, as above.
- **Finance.** Per day and per month:
  - revenue: Stripe gross, fees, refunds and net, from balance transactions;
  - infra spend: Cloudflare actual, model cost from the ledger, and operator
    overhead;
  - margin, credits outstanding (sold but not yet spent, a liability), and
    the global breaker's state.
- **Breaker and limits.** See the ceiling, open or close the breaker, and
  change plan caps.

Every admin action needs an admin Clerk session plus the same-origin check.
It is recorded in `admin_actions` as a durable action (above) before it
takes effect, and the record keeps its outcome.

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
1. **Accounts.**
   - Clerk sign-in, and session verification on the relay.
   - Verified, ordered Clerk webhooks.
   - Namespace bindings, frozen atomically at first use.
   - The independent access states, and suspension enforced at the Worker
     with settlement kept open.
   - `admitFor` and `settleFor` through the binding, the owner-status
     lease with its abort worklist, and admins by Clerk role.
   - The accounts view of the operator portal.
   - Account deletion and the account's retention policy (below). Both
     ship in this phase, before anyone is charged.

   While in beta, only invited accounts are active.
2. **Billing.**
   - Purchases through Stripe Checkout (invoices and Stripe Tax), as
     durable actions.
   - Credit lots reconciled from Stripe's current state, with versioned
     entitlements.
   - Refunds with credit holds, disputes, the single balance equation, and
     request-triggered recovery of unreconciled lots.
   - Reservation-based admission with per-step pre-checks, one active
     operation per agent, and the concurrency cap.
   - The whole-lifecycle exec deadline and the global breaker.
   - The billing page and the operator portal's refunds and finance views.

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

## Decided (Prass, 2026-11-24)

1. **Prepaid credits.** The ledger above assumes them.
2. **Markup and minimum top-up** are config values with neutral defaults:
   no markup, and Stripe's own minimum charge. The real values are later
   decisions.
3. **No free credits.** Comps and trial credits are removed.
4. **Clerk sign-in:** every method Clerk offers at launch (GitHub, Google,
   email).

## Still open for Prass

- **Portal host.** Not answered. The default is `app.repowire.io`: the
  portal is the user-facing app, and the relay is infrastructure. It places
  Clerk's frontend-API CNAME and the `azp` origin the relay accepts, and
  stands unless Prass objects.
- **Markup and minimum top-up, before charging.** Prass called them not
  important for now, so the config defaults (no markup, Stripe's minimum)
  are placeholders. Before the first charge, set a markup that at least
  covers processing fees and a minimum top-up that makes the fixed fee
  small.
- **Legal retention period for financial records.** It depends on the
  merchant entity (above).
- **Deploy and account creation:** the relay deploy with the
  `managed.secretName` Secret, and creating the Clerk and Stripe accounts.
