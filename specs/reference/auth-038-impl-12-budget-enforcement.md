---
title: Billing — budget enforcement (auth tracks, products honor)
status: superseded (pending redesign, see specs/060-billing-redesign.md)
depends_on:
  - specs/.archive/023-billing.md
  - specs/.archive/030-impl-07-meter-push-worker.md
  - specs/.archive/036-impl-10-envelope-and-reservations.md
affects:
  - migrations/000031_billing_budgets.{up,down}.sql
  - internal/billing/budget.go (new)
  - internal/billing/budget_test.go (new)
  - internal/billing/worker.go
  - internal/handler/billing_user.go
  - internal/handler/billing_internal.go
created: 2026-05-09
author: codex
trigger: cross-product billing memo §"Spending limit / budget enforcement" — bidirectional contract auth must own; today neither auth nor cella has any budget primitive. Lands the contract before Lux ships token spend, which is the highest-leverage trigger for "give me a cap."
---

# Budget enforcement

A budget is a declared cap on cumulative spend for a payer in a
period (initially monthly). Only auth has the cross-product view
to know when the cap is approached; only products can stop
generating new charges. So the contract is bidirectional:

1. Auth tracks cumulative spend (already does, via push log).
2. Auth signals each product when a payer crosses warning or
   exhausted thresholds.
3. Each product extends its entitlement gate to honor the
   `exhausted` signal and refuse new billable operations.

Charging users past their declared cap is unacceptable; bill-after-
the-fact violates the user's expectation. The system must refuse
new charges before the cap is breached.

## Data shape

### Migration `000031_billing_budgets.up.sql`

```sql
CREATE TABLE billing_budgets (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    payer_kind   TEXT         NOT NULL CHECK (payer_kind IN ('org','principal')),
    payer_id     UUID         NOT NULL,
    period_kind  TEXT         NOT NULL CHECK (period_kind IN ('monthly')),  -- room to grow
    cap_amount   NUMERIC(18,6) NOT NULL CHECK (cap_amount > 0),
    warn_at_pct  INTEGER      NOT NULL DEFAULT 80 CHECK (warn_at_pct BETWEEN 1 AND 100),
    currency     TEXT         NOT NULL DEFAULT 'USD',
    enabled      BOOLEAN      NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (payer_kind, payer_id, period_kind) WHERE enabled
);

CREATE TABLE billing_budget_signals (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    budget_id    UUID         NOT NULL REFERENCES billing_budgets(id) ON DELETE CASCADE,
    product      TEXT         NOT NULL,
    period_start TIMESTAMPTZ  NOT NULL,
    status       TEXT         NOT NULL CHECK (status IN ('normal','warning','exhausted')),
    sent_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (budget_id, product, period_start, status)
);
```

`billing_budget_signals` is a write log: one row per
`(budget, product, period, status)`. The UNIQUE constraint makes
re-emission a no-op; the worker uses it as a "did we already tell
this product about this state for this period" check.

`down.sql` drops both tables.

## Spend computation

Cumulative spend per `(payer, period)` is the sum of
`billing_meter_pushes.quantity * price_per_unit` minus
`credits_applied`. Per-meter price comes from auth's existing
plan→Stripe-Price mapping; credits come from `billing_credits`
(empty in v1, but the path exists per impl-10).

Implementation: `internal/billing/budget.go` exposes

```go
type Spend struct {
    Cumulative decimal.Decimal
    Cap        decimal.Decimal
    Pct        int
}

func ComputeSpend(ctx context.Context, payer Payer, period Period) (Spend, error)
```

Naive query for v1 (no row count optimization needed at our scale):

```sql
SELECT COALESCE(SUM(quantity * price_per_unit) - SUM(credits_applied), 0)
FROM   billing_meter_pushes p
JOIN   billing_meter_prices m ON m.meter_id = p.meter_id  -- new view; see below
WHERE  p.payer_kind   = $1
  AND  p.payer_id     = $2
  AND  p.period_start = $3;
```

`billing_meter_prices` is a view (or seeded table) mapping
`meter_id` → `unit_price`. v1 sources prices from env config the
same way `impl-09-rollout.md` already wires Stripe Price IDs.

## Worker integration

The meter-push worker (`internal/billing/worker.go`) already
drains `billing_meter_pushes WHERE pushed_at IS NULL`. Extend its
post-push step:

1. After a successful Stripe meter event, recompute `Spend` for
   the `(payer, period)` of the row just pushed.
2. Resolve threshold: `exhausted` if `Pct >= 100`,
   `warning` if `Pct >= warn_at_pct`, else `normal`.
3. If the resolved status differs from the most recent row in
   `billing_budget_signals` for the same `(budget, product, period)`,
   POST `/internal/billing/budget/signal` to **each known product**
   for that payer (today: Cella; later: Lux, Workbench).
4. Insert into `billing_budget_signals`. The UNIQUE constraint
   suppresses double-fire on rapid pushes.

### Auth → Product wire

```
POST <product>/internal/billing/budget/signal
Authorization: Bearer <auth-budget-signal service JWT, scope billing:budget-signal>
Body:
{
  "payer_kind":   "org",
  "payer_id":     "uuid",
  "period_start": "RFC3339",
  "status":       "warning" | "exhausted" | "normal",
  "cap_amount":   500.00,
  "currency":     "USD",
  "cumulative":   412.30,    // for product-side UX
  "pct":          82
}
```

Products acknowledge with `204 No Content` on success. Auth
retries on 5xx with backoff; on persistent failure the signal log
shows the last successful state, and the next push will re-attempt.

Endpoint URLs are looked up from auth config:

```
BILLING_PRODUCT_ENDPOINTS = cella=https://cella.latere.ai/internal/billing
```

…with one entry added per product as it onboards.

## User-facing surfaces

### `GET /me/billing/budget[?scope=org|principal]`

Reads the active `billing_budgets` row for the resolved scope.
Returns:

```json
{
  "enabled": true,
  "cap_amount": 500.00,
  "currency": "USD",
  "warn_at_pct": 80,
  "period_kind": "monthly",
  "current": { "cumulative": 412.30, "pct": 82, "status": "warning" }
}
```

Returns `{"enabled": false}` when no row.

### `PUT /me/billing/budget`

Permission: `billing:write`. Body matches the response shape minus
`current`. Upserts the budget row.

### Audit

Writes to `billing_budgets` emit
`billing.budget.set` / `billing.budget.unset`. Signals emit
`billing.budget.signal_sent` with the payload for replayability.

## Product-side obligations (for Cella; mirrored in `sandbox/specs/core/76`)

Each product:

1. Implements `POST /internal/billing/budget/signal` with the
   shape above and validates the auth JWT.
2. Stores the latest signal per `(payer, period)` in product
   memory (or table — Cella's `BillingPlanResolver` already has a
   30s cache; extend it to carry budget status).
3. Extends the entitlement gate to refuse new billable operations
   when status is `exhausted`. Reuse the existing
   `402 payment_required` envelope with a new `reason`:

   ```json
   {
     "code":   "payment_required",
     "reason": "budget_exhausted",
     "message": "Budget for this period has been reached. Raise the cap or wait for the next period.",
     "request_id": "...",
     "portal_url":   "https://..."
   }
   ```

4. Honors `warning` only as a hint (e.g. dashboard banner). It is
   not a refusal.
5. Does **not** retroactively kill running sandboxes / agents on
   `exhausted` in v1. Refuses new ones only. Killing in-flight
   work is a v2 conversation.

## Tests

- Unit: `ComputeSpend` with seeded pushes and credits.
- Worker: pushing a row that crosses the warning threshold fires
  one signal; another push at the same status fires nothing.
- Worker: pushing a row that crosses 100% fires `exhausted`.
- Worker: budget rolls to next period — a signal for the new
  period fires fresh `normal` once spend resumes.
- Handler: `PUT /me/billing/budget` round-trip; permission gate.
- Mock product receives signals with the expected shape.

## Rollout

| Phase | Action |
|---|---|
| 1 | Migration + tables + handlers + admin readback. No signals fire (no `BILLING_PRODUCT_ENDPOINTS` configured). |
| 2 | Cella ships `core/76` consumer. Wire endpoint into `BILLING_PRODUCT_ENDPOINTS`. Warning-only signals. |
| 3 | Enable `exhausted` enforcement in cella. Document `reason=budget_exhausted` in the public API error catalog. |
| 4 | Lux + Workbench onboard the same way as they ship metering. |

## Out of scope

- Daily / weekly / per-product caps. v1 is monthly cumulative
  across all products under one payer.
- Auto-cap (anomaly detection). Manual user-set caps only.
- Killing running sandboxes / agents at `exhausted`. Refuse new
  only.
- Cap-by-meter-kind (e.g. "$50 of model spend, $50 of compute").
  Single cap per payer per period in v1.
- Suspension cooldown / grace credit. Hitting `exhausted` blocks
  until the user raises the cap or the next period begins.

## Acceptance

- `make migrate-up` lands `000031`.
- `PUT /me/billing/budget` round-trips; signal log shows
  `billing.budget.set`.
- Mock product server in CI receives `warning` and `exhausted`
  signals at the expected thresholds.
- Cella's entitlement test suite (sandbox spec 76) refuses new
  sandboxes when `exhausted` is the active state.
