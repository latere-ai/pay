---
title: Billing — Stripe identity, subscription state, and meter bridge
status: complete
depends_on:
  - specs/.archive/001-design-v1.md
  - specs/.archive/021-admin-ui.md
affects:
  - internal/billing (new package)
  - internal/handler (new endpoints)
  - migrations (new migration 000025)
created: 2026-04-26
updated: 2026-05-08
author: changkun
trigger: ship paid plans; sandbox/specs/core/60 is the cella-side counterpart
---

# Billing — parent spec

## Status

Parent index. Implementation is decomposed into
`specs/.archive/billing/impl-XX-*.md`. Read this file first for the
boundary, contract, and design decisions; then read the impl tree
in order.

## Spec deltas (2026-05-08)

Decisions taken at Slice-A kickoff that override the original
draft. The impl specs below have been amended to match.

- **Migration number `000021` → `000025`.** Original draft predated
  `000021_oauth_clients_audiences`, `000022_drop_sandbox_client_config`,
  `000023_drop_sandbox_tokens`, `000024_fosite_tokens_audiences`. The
  billing migration is `000025_billing.{up,down}.sql`. `impl-01`
  reflects this.
- **Two payer paths, not one.** The original draft scoped billing
  rows to `org_id` only and routed personal billing through each
  user's personal org. Decision is to support **both org-pays and
  principal-pays as separate code paths** in `billing_customers`
  and `billing_subscriptions` (one-of `org_id` / `principal_id`).
  The `/me/billing*` handlers resolve the active scope from the
  JWT — active-org context if present, otherwise personal-principal.
  Wire contract for `/internal/billing/usage` stays org-only (Slice
  A doesn't ship metered personal products); `/internal/billing/account`
  gets a parallel `/internal/billing/account/principal/{principal_id}`
  variant — see "Wire contract" below.
- **Trial mode deferred.** Free-tier-via-cella *is* the v1 trial.
  Stripe `trialing` is not wired in Slice A or B. Open question
  stays open; revisit before marketing launch.
- **Stripe SDK pin: latest `stripe-go`.** Pinned at `v85.1.0`
  as of 2026-05-09. Stripe ships major versions on a fast cadence
  (typically one per month); upgrade as v86+ becomes available.
  Note: v85 introduced "thin event notifications" — the webhook
  parser now requires `"object": "event"` at the payload top
  level; production payloads from Stripe always have it but our
  test fixtures had to be updated.
- **`impl-08-admin-ui.md` is partly stale.** It assumes
  `ui/templates/admin.html` and `ui/static/admin.js` (deleted in
  the Vue SPA migration). The Go endpoints in `impl-08` still
  apply unchanged; the UI surface lands as a new Vue component in
  `frontend/src/views/AdminView.vue`'s tab switch (mirroring how
  Users / Orgs / Sessions tabs are structured today).
- **Slice phasing confirmed.** Slice A = `impl-01..04` (mock-only,
  no Stripe dep, lights up the existing `PaymentView`). Slice B
  = `impl-05..06`. Slice C = `impl-07..09`.
- **`/me/billing` JSON path moved to `/api/me/billing`.** Spec
  originally said `GET /me/billing` returns JSON. Reality: the SPA
  serves the page at `/me/billing`, so the JSON surface lives at
  `/api/me/billing` per the codebase `/api/*` convention (cf.
  `/api/me`, `/api/config`, `/api/consent`). POST routes
  (`/me/billing/checkout`, `/me/billing/portal`) keep their original
  paths since they don't collide with SPA GETs.
- **Generated query code not used.** The store package uses raw
  `pgx` queries, matching every other package in the repo. The
  obsolete generator config and unused store query definitions were
  removed. Older billing persistence drafts that referenced generated
  query files are obsolete.

## Problem

Auth has no durable payment state today. Cella owns product
policy (plan catalog, quota ceilings, free-tier accounting,
billing aggregation, create-time entitlement). Auth must own
payment identity (Stripe Customer, Subscription, Checkout, Portal,
webhooks, meter pushes) and expose a small contract cella
consumes:

1. Cella asks auth for the active org's billing account state.
2. Cella sends users to auth-backed Checkout/Portal with the
   user's auth access token.
3. Cella reports per-tick **delta** usage to auth.
4. Auth forwards each delta to a Stripe Meter, idempotently.

The product split is fixed. Cella never holds Stripe credentials
and never imports a Stripe SDK; auth never interprets plan-name
semantics.

## Boundary

Authoritative — every subspec resolves to this.

| Concern | Owner |
|---|---|
| Stripe Customer, Subscription, payment method | auth |
| Checkout / Portal session creation | auth |
| Stripe webhook ingestion + dispatch | auth |
| Meter event push to Stripe | auth |
| Plan catalog (names, quotas, sizing) | cella |
| Plan→Stripe Price ID mapping | auth (env config) |
| Plan→Stripe Meter ID mapping | cella (per-plan in catalog) |
| Free-tier accounting | cella |
| Per-period usage aggregation | cella |
| Create-time entitlement enforcement | cella |
| Payment-required UX (link, copy) | cella |

`subscription.plan` is stored as an opaque string in auth and
never gates resources on its value. Stripe Price ID is the
source of truth for money; the cella plan catalog is the source
of truth for product.

## Cross-repo coordination

The cella-side punch list, design rationale, and end-to-end
acceptance criteria are in
[`sandbox/specs/core/60-billing-end-to-end-completion.md`](../../../sandbox/specs/core/60-billing-end-to-end-completion.md).
Read it before changing the cella-facing wire shape below.

## Wire contract (cella → auth)

### `GET /internal/billing/account/{org_id}`

Service-token call; scope `billing:read`. Cella consumes via
`sandbox/internal/billing.AuthClient.Account`.

```json
{
  "payer": {"kind": "org", "id": "019dc..."},
  "plan": "pro",
  "status": "active",
  "payment_method_attached": true,
  "period_start": "2026-05-01T00:00:00Z",
  "period_end": "2026-06-01T00:00:00Z",
  "checkout_url": "https://...",
  "portal_url": "https://..."
}
```

`404` is a valid response shape — cella treats it as "use free
plan." Do **not** synthesize a fake free row server-side; the
absent-row case lets cella test fallback.

The legacy top-level `"org_id"` key is also emitted for one
cella release to avoid a coordinated cut-over; new readers should
prefer `payer.id`.

### `GET /internal/billing/account/principal/{principal_id}`

Same shape, principal-scoped. Cella does **not** call this in
Slice A — the personal-billing surface is auth's UI only, and
metered usage is org-scoped. Endpoint exists so admin tooling
and the `/me/billing` handler share a single backing query.
Permission: `billing.admin` (superadmin) or self (`sub` matches
`{principal_id}`).

### `POST /internal/billing/usage`

Service-token call; scope `billing:report`. Cella sends one row
per `(org, kind, tick)`:

```json
{
  "org_id": "019dc...",
  "kind": "sandbox.seconds",
  "meter_id": "mtr_sandbox_seconds",
  "quantity": 1234.0,
  "period_start": "2026-05-01T00:00:00Z",
  "idempotency_key": "019dc...:sandbox.seconds:2026-05:0007"
}
```

`quantity` is a **delta**, not a cumulative. The idempotency key
ends in a per-period sequence so retries collapse to no-ops while
genuine forward progress lands new rows. (Rationale and the
cella-side reporter change are in spec 60's "Meter delta
semantics" section.)

Auth `INSERT … ON CONFLICT (idempotency_key) DO NOTHING` into
`billing_meter_pushes` and returns:

- `202 Accepted { "queued": true }` — newly staged.
- `200 OK { "queued": false, "already_pushed_at": "..." }` —
  replay (key existed; `pushed_at` may or may not be set).

The push worker drains `WHERE pushed_at IS NULL` and posts each
row's `quantity` to Stripe Meter `meter_id` for the org.

### `POST /me/billing/checkout` and `POST /me/billing/portal`

User-token calls; permissions `billing.write` (already seeded —
`migrations/000007_seed_data.up.sql` grants `billing.write` to
`owner` and `admin`, `billing.read` to `member`). Cella's
dashboard already calls `BillingAuth.CheckoutWithToken(sess.AccessToken,
plan)` and `BillingAuth.PortalWithToken(sess.AccessToken)` —
auth must accept these without falling back to the service token.

Response: `{ "url": "https://..." }`.

### `GET /me/billing[?scope=org|principal]`

User-token call; permission `billing.read`. Returns the nested
form for the dashboard.

Active scope resolution:
- Default (`scope` absent): use the JWT's active-org context if
  the `org_id` claim is non-empty; otherwise use principal scope.
- Explicit `?scope=org`: requires non-empty `org_id` in JWT;
  `400 invalid_request` otherwise.
- Explicit `?scope=principal`: always uses the JWT's `sub`.

Frontend `PaymentView.vue` already passes `scope` as a prop; the
two router entries (`/me/billing` and `/orgs/:id/billing`) supply
the right value.

```json
{
  "payer": {"kind": "org", "id": "019dc..."},
  "stripe_customer_id": "cus_...",
  "payment_method_attached": true,
  "subscription": {
    "plan": "pro",
    "status": "active",
    "current_period_start": "2026-04-01T00:00:00Z",
    "current_period_end":   "2026-05-01T00:00:00Z",
    "cancel_at_period_end": false
  }
}
```

`subscription` is `null` when the payer has no row. Caller
decides what to render.

### `POST /webhooks/stripe`

Public; verified by `Stripe-Signature` against
`STRIPE_WEBHOOK_SECRET`. See `impl-06-stripe-webhook.md`.

## Migration number

**000025_billing.up.sql.** `000021` through `000024` are taken
(audiences and sandbox-client cleanup). See `impl-01-persistence.md`
for the schema; this number is authoritative — earlier draft
references to `000021` are obsolete.

## Service identity

Cella authenticates to `/internal/billing/*` with a service-account
JWT. Add to seed:

- Service account `cella-billing` with scopes:
  - `billing:read` — `GET /internal/billing/account/{org_id}`.
  - `billing:report` — `POST /internal/billing/usage`.

`/me/billing/*` rejects this service token (no `sub` matching a
human principal, no org membership).

## Implementation tree

| File | Phase | Title |
|---|---|---|
| [`billing/impl-01-persistence.md`](024-impl-01-persistence.md) | 1 | Migration 000021 + store package |
| [`billing/impl-02-mock-service.md`](025-impl-02-mock-service.md) | 1 | Mock-mode service for cella CI |
| [`billing/impl-03-user-handlers.md`](026-impl-03-user-handlers.md) | 2 | `/me/billing*` |
| [`billing/impl-04-internal-handlers.md`](027-impl-04-internal-handlers.md) | 2 | `/internal/billing/*` + service-account scopes |
| [`billing/impl-05-stripe-checkout-portal.md`](028-impl-05-stripe-checkout-portal.md) | 3 | Stripe wrapper for Checkout + Portal |
| [`billing/impl-06-stripe-webhook.md`](029-impl-06-stripe-webhook.md) | 4 | Webhook receiver + dispatcher |
| [`billing/impl-07-meter-push-worker.md`](030-impl-07-meter-push-worker.md) | 5 | Drain `billing_meter_pushes` to Stripe |
| [`billing/impl-08-admin-ui.md`](031-impl-08-admin-ui.md) | 6 | Admin billing tab |
| [`billing/impl-09-rollout.md`](032-impl-09-rollout.md) | 7 | Config, deploy, pilot org |
| [`billing/impl-10-envelope-and-reservations.md`](036-impl-10-envelope-and-reservations.md) | 8 | Envelope `user_id` + `metadata`; reserve `cost_center`/`project_tag`; `billing_credits` table; pin across-products credit policy |
| [`billing/impl-11-pull-reconciliation.md`](037-impl-11-pull-reconciliation.md) | 9 | Pull-side authoritative reconciliation + drift detection |
| [`billing/impl-12-budget-enforcement.md`](038-impl-12-budget-enforcement.md) | 10 | Budget tables, worker signals, product-side `/budget/signal` consumer contract |
| [`billing/impl-13-credit-application.md`](039-impl-13-credit-application.md) | 11 | Across-products credit-application worker, meter→price view, admin/user readback |

Phases 8–11 are downstream of the
[Latere Identity strategy — billing aggregator](https://github.com/latere-ai/specs/blob/main/products/identity.md#billing-aggregator-role)
and land before Lux + Workbench start metering. impl-10 is purely
additive and pins the credit policy (across-products); impl-11 adds
a reconciliation worker; impl-12 introduces the bidirectional
budget contract; impl-13 turns impl-10's empty `billing_credits`
table into a working deduction path before Stripe.

## Out of scope

- **Plan policy.** Plan tiers, quotas, free-tier sizes, sizing
  defaults — cella's product datastore.
- **Per-principal budgets, free-tier accounting, entitlement
  enforcement on product resources.** Cella, spec 47.
- **Usage aggregation.** Cella reads `billing_legs` from
  ClickHouse and sends per-tick deltas. Auth never aggregates.
- **Invoicing UI, dunning automation, tax calculation.** Stripe
  Hosted Checkout + Customer Portal own all of these.
- **Multi-currency, prepaid credits, card-less trials.** v1 is
  USD-only, subscription + metered. Trials are
  free-tier-via-cella, not Stripe `trialing`.
- **Webhook → cella cache invalidation hook.** Cella's resolver
  TTL is 30s; that's the upper bound on entitlement staleness.
- **CLI billing commands** beyond payment-required rendering.

## Acceptance (parent)

Each impl spec restates relevant subset; this is the union:

- An org owner clicks Upgrade in the cella dashboard. Auth
  creates a Checkout Session; user pays; Stripe fires
  `customer.subscription.created`; auth upserts
  `billing_subscriptions(plan="pro", status="active")`.
- Cella's `GET /internal/billing/account/{org_id}` reads
  `plan="pro", status="active"` and gates accordingly.
- Cella reports a `delta` to `/internal/billing/usage` with
  key `<org>:<kind>:<period>:<seq>`. The next push-worker tick
  reports the quantity to Stripe; replays of the same key are
  no-ops.
- `invoice.payment_failed` →
  `customer.subscription.updated(status="past_due")`. Auth
  updates the row; cella's next `Resolve` (within 30s) gates.
- Auth admin UI lists orgs by plan/status with last meter-push
  errors and a Stripe-Dashboard deep-link.

## Open questions

- **Cella → auth invalidation.** v1 accepts 30s lag; revisit if
  ops sees support tickets about "I paid but it's still locked."
- **Org-pays vs principal-pays.** v1 is org-pays (personal org
  covers solo). Per-principal billing is enterprise-only.
- **Trial mode.** Free-tier-via-cella is the v1 trial; Stripe
  `trialing` adds webhook complexity for marginal value. Decide
  before launch if marketing wants a "14-day free trial" frame.
- **Checkout session reuse.** Stripe allows expiring + reusing
  Checkout Sessions; v1 always creates a fresh session per
  `POST /me/billing/checkout`. Revisit if conversion analytics
  cares.

## Related

- Cella counterpart:
  [`sandbox/specs/core/60-billing-end-to-end-completion.md`](../../../sandbox/specs/core/60-billing-end-to-end-completion.md).
- Cella resource sizing:
  [`sandbox/specs/archive/53-on-demand-resources.md`](../../../sandbox/specs/archive/53-on-demand-resources.md).
- Architecture rule:
  [`sandbox/docs/platform-boundaries.md`](../../../sandbox/docs/platform-boundaries.md).
- Cella billing-stats spec (shipped, no money):
  [`sandbox/specs/archive/product/28-dashboard-billing-stats.md`](../../../sandbox/specs/archive/product/28-dashboard-billing-stats.md).
