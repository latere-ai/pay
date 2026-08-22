---
title: Billing — across-products credit application
status: superseded (pending redesign, see specs/060-billing-redesign.md)
depends_on:
  - specs/.archive/023-billing.md
  - specs/.archive/030-impl-07-meter-push-worker.md
  - specs/.archive/036-impl-10-envelope-and-reservations.md
  - specs/.archive/038-impl-12-budget-enforcement.md
affects:
  - migrations/000032_billing_meter_prices.{up,down}.sql
  - internal/billing/credits.go (new)
  - internal/billing/credits_test.go (new)
  - internal/billing/worker.go
  - internal/handler/billing_admin.go
  - internal/handler/billing_user.go
created: 2026-05-09
author: codex
trigger: cross-product billing memo §"Credits, free tiers, promotions" — across-products is the v1 commitment (impl-10 §"Credit application policy"). This spec lands the worker, the meter→price view, the audit trail, and the user/admin readback.
---

# Across-products credit application

[`impl-10`](036-impl-10-envelope-and-reservations.md) shipped the
`billing_credits` table and the `credits_applied` column on
`billing_meter_pushes` and pinned the policy: **across-products,
chronological by event timestamp.** This spec is the worker that
turns the policy into actual deductions before Stripe sees the
quantity.

No new behavior at deploy time when zero credit rows exist. The
worker is a no-op until the first credit lands.

## Policy (locked)

- **Across-products.** A credit is keyed on `(payer_kind, payer_id)`,
  not on a product. Any product's billable event can consume it.
- **Chronological.** Events drain credits in `period_start ASC,
  staged_at ASC` order. The first event that arrives in a period
  consumes credits first; later events see whatever is left.
- **Oldest credit first.** Within a payer, credits drain in
  `created_at ASC` order. Promotional credits with shorter
  `expires_at` should be inserted as older `created_at` if you want
  them to drain first; otherwise the engine picks the oldest by
  creation time. (Memo §3 doesn't require expiry-aware ordering;
  add it later if needed.)
- **One currency in v1: USD.** Mismatched currency on a credit row
  is a config error and is rejected by the worker with an audit
  emit; the row sits in the table unconsumed.
- **No retroactive application.** Credits inserted *after* a push
  has already been sent to Stripe do not retroactively reduce that
  push. They apply to the next eligible event.
- **No partial Stripe events.** When a credit fully covers an event,
  the worker pushes `quantity = 0` to Stripe (Stripe accepts 0-quantity
  meter events; they show as zero on the invoice line). When a credit
  partially covers an event, the worker pushes the remainder.

## Data shape

### Migration `000032_billing_meter_prices.up.sql`

`billing_meter_prices` is the meter→unit-price mapping the worker
needs to convert event quantity into a dollar amount. impl-12
referenced this view; this spec is where it actually lands.

```sql
CREATE TABLE billing_meter_prices (
    meter_id     TEXT          PRIMARY KEY,
    unit_price   NUMERIC(18,6) NOT NULL CHECK (unit_price >= 0),
    currency     TEXT          NOT NULL DEFAULT 'USD',
    description  TEXT          NULL,
    updated_at   TIMESTAMPTZ   NOT NULL DEFAULT now()
);
```

Seed mirrors the existing Stripe Price configuration. The seed
file lives next to the existing plan / scope seeds:

```sql
INSERT INTO billing_meter_prices (meter_id, unit_price, description) VALUES
    ('mtr_sandbox_seconds',  0.0000277778, '$0.10/hour Cella sandbox compute'),
    ('mtr_sandbox_storage',  0.0000001157, '$0.10/GB-month Cella workspace storage');
-- additional rows added when Lux + Workbench onboard
```

Operators can adjust prices via admin handler (out of scope for
this spec; manual SQL is fine in v1).

`billing_credits` and `billing_meter_pushes.credits_applied` already
exist (impl-10). No schema change beyond the price table.

## Worker integration

`internal/billing/worker.go` already drains
`billing_meter_pushes WHERE pushed_at IS NULL`. The new behavior
sits **between** "row read" and "Stripe POST":

```
1. SELECT row FROM billing_meter_pushes WHERE pushed_at IS NULL ORDER BY id LIMIT N
2. For each row:
   2a. quantity_dollars = row.quantity * meter_prices.unit_price
   2b. credits_to_apply = ApplyCredits(payer, row.period_start, quantity_dollars)
   2c. credits_applied_units = credits_to_apply / meter_prices.unit_price
   2d. quantity_after_credits = row.quantity - credits_applied_units
       (clamped at 0; rounded to meter resolution; never negative)
   2e. POST Stripe Meter event with quantity_after_credits
   2f. UPDATE billing_meter_pushes SET pushed_at=now(), credits_applied=credits_to_apply WHERE id=row.id
   2g. (in same tx as 2b/2f) UPDATE billing_credits SET consumed_amount += credits_to_apply WHERE id=...
```

### Concurrency

The worker is leader-gated (existing pattern; impl-07). Only one
process drains pushes at a time, so concurrent credit consumption
is not a v1 concern. The credit reservation step `2b` runs in a
single transaction with the push update `2f` and the credit-row
update `2g` so a crash between Stripe success and DB commit is
the only failure mode worth thinking about.

If Stripe accepts the meter event but the DB transaction fails to
commit afterwards, the next worker tick will re-read the same row
(`pushed_at IS NULL`) and re-POST. Stripe Meter is idempotent on
`(event_name, identifier)` — auth's existing pattern uses the push
row id as the identifier, so the duplicate is a no-op on Stripe's
side. The credit row's `consumed_amount` is what could double-up,
so the credit-deduction transaction should be **committed before**
the Stripe POST when ordering matters; v1 keeps the existing
"Stripe first, DB second" order because Stripe idempotency is
strong and credit double-spend is bounded by the row size.

### Pseudocode for `ApplyCredits`

```go
// ApplyCredits reserves up to want_dollars from the payer's credit
// balance and returns the dollar amount actually applied. The
// caller is responsible for committing the matching push update.
func ApplyCredits(ctx, tx, payer, periodStart, wantDollars) (decimal, error) {
    rows := SELECT id, amount, consumed_amount, currency, expires_at
           FROM billing_credits
           WHERE payer_kind=$1 AND payer_id=$2
             AND consumed_amount < amount
             AND (expires_at IS NULL OR expires_at >= $3)
           ORDER BY created_at ASC
           FOR UPDATE
    applied := 0
    for r := range rows {
        if r.currency != "USD" {
            audit("billing.credit.skipped_currency", r.id)
            continue
        }
        room   := r.amount - r.consumed_amount
        take   := min(room, wantDollars - applied)
        if take <= 0 { break }
        UPDATE billing_credits SET consumed_amount = consumed_amount + take WHERE id=r.id
        applied += take
        audit("billing.credit.applied", { credit_id: r.id, amount: take, period_start: periodStart })
        if applied >= wantDollars { break }
    }
    return applied
}
```

`FOR UPDATE` serializes credit consumption within the worker
transaction. The leader-gate already serializes across replicas.

## Admin surfaces

### `POST /internal/billing/credits`

Service-token only; scope `billing:credits-write`. Used by the
admin UI and (later) any promo-issuance pipeline. Inserts a row in
`billing_credits`.

```json
{
  "payer_kind":  "org",
  "payer_id":    "uuid",
  "amount":      50.00,
  "currency":    "USD",
  "source":      "promo" | "refund" | "manual",
  "note":        "free credit for design partner",
  "expires_at":  "2026-12-31T23:59:59Z"
}
```

Response: `201 Created { id: "uuid" }`.

### `GET /internal/billing/credits?payer_kind=...&payer_id=...`

Lists active and consumed credits for a payer. Admin readback for
disputes and support.

## User surfaces

### `GET /me/billing/credits[?scope=org|principal]`

Returns the resolved scope's outstanding credit balance, with a
short list of recent applications:

```json
{
  "currency": "USD",
  "balance":  37.50,
  "rows": [
    { "id": "...", "amount": 50.00, "consumed_amount": 12.50,
      "source": "promo", "note": "...", "expires_at": "..." }
  ]
}
```

Permission: `billing:read`. No write surface for users in v1 —
they can't create their own credits. The dashboard renders this
beside the existing payment summary.

## Tests

`internal/billing/credits_test.go`:

- Single payer, one credit, one event larger than credit:
  push goes through with `quantity_after_credits = qty - credit_in_units`,
  credit fully consumed.
- Single payer, one credit, one event smaller than credit:
  push goes through with `quantity = 0`, credit partially consumed,
  remainder still available.
- Two events in one period: chronological — first event consumes
  credit first; second event sees whatever's left.
- Two credits, oldest-first ordering: drain credit A before credit B.
- Expired credit is skipped.
- Mismatched currency credit is skipped + audit emitted.
- No credits → worker behaves identically to pre-impl-13 (regression
  check).
- Stripe idempotency: simulate Stripe success + DB rollback;
  re-tick re-pushes; Stripe sees the same identifier and no double
  charge; `consumed_amount` does not double.

## Operational notes

- **First-time payer migration.** When credits start landing on
  payers who already have a Stripe subscription, the next event in
  the new period is the first one to see credit deduction. No
  retroactive backfill of past invoices; documented to support.
- **Credit balance display lag.** The user-facing balance is
  recomputed on every request (no cache); for an active heavy user
  this can read up to 30s old via the existing
  `BillingPlanResolver` cache. Acceptable for v1.
- **Reconciliation interaction.** impl-11's pull-side reconcile
  reports product-side aggregates. The pull total is in
  *quantity*, not dollars; it ignores credits (which are an
  auth-side construct). When auth computes drift it must remember
  that `pull_total - credits_in_units` ≠ `Stripe meter total` only
  before credit application; after the worker pushes, Stripe's
  total reflects the post-credit quantity. The drift check
  compares `push_total` (pre-credit, which is what impl-11 already
  records) against `pull_total` — both pre-credit. No change to
  impl-11.

## Out of scope

- Per-product credits (rejected; across-products is locked).
- Per-meter-kind credits ("$50 of model spend"). Single balance
  per payer.
- Auto-issued promo credits triggered by signup, etc. v1 issues
  manually.
- Currency conversion. v1 is USD-only.
- User-visible "credit will expire on" warnings. Defer.
- Refund-credit issuance from disputes (`source="refund"` is
  recognized but no automatic flow creates them).

## Acceptance

- `make migrate-up` lands `000032` cleanly.
- An admin inserts a $50 USD `promo` credit for an org; the next
  meter push for that org sees the credit deducted and Stripe
  receives `quantity_after_credits`.
- `GET /me/billing/credits` returns the running balance.
- Mock-mode CI exercises full-cover, partial-cover, expired,
  mismatched-currency, and zero-credit cases.
- Stripe idempotency test confirms no double-spend on simulated
  failures.
