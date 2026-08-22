---
title: Billing — pull-side authoritative reconciliation
status: superseded (pending redesign, see specs/060-billing-redesign.md)
depends_on:
  - specs/.archive/023-billing.md
  - specs/.archive/030-impl-07-meter-push-worker.md
  - specs/.archive/036-impl-10-envelope-and-reservations.md
affects:
  - migrations/000030_billing_reconciliations.{up,down}.sql
  - internal/billing/reconcile.go (new)
  - internal/billing/reconcile_test.go (new)
  - internal/billing/types.go
  - internal/handler/billing_admin.go
  - cmd/auth/main.go (worker wiring)
created: 2026-05-09
author: codex
trigger: cross-product billing memo §"Push vs pull" — invoice of record is the reconciled number, not the push log. Today's design treats the push log as authoritative; this spec adds the pull leg.
---

# Pull-side authoritative reconciliation

The push contract (`POST /internal/billing/usage`) is good for
real-time spend visibility and budget enforcement, but it is not
authoritative. A lost row, a clock skew, or a Cella bookkeeping
failure can desync auth's `billing_meter_pushes` total from the
product's actual usage. The cross-product billing memo's invoice
rule is: the reconciled number is what we charge.

This spec makes auth pull from each product on a schedule and
treat that number as the invoice of record.

## Wire contract (auth ← product)

Each product implements:

```
GET /internal/billing/reconcile?payer_kind=org&payer_id=<uuid>&period_start=<RFC3339>&period_end=<RFC3339>
Authorization: Bearer <product service JWT with scope billing:reconcile-serve>
```

Response:

```json
{
  "payer_kind": "org",
  "payer_id":   "uuid",
  "period_start": "2026-04-01T00:00:00Z",
  "period_end":   "2026-05-01T00:00:00Z",
  "items": [
    {
      "kind":       "sandbox.seconds",
      "meter_id":   "mtr_sandbox_seconds",
      "quantity":   3_245_678,
      "unit":       "seconds"
    }
  ],
  "computed_at": "2026-05-09T10:00:00Z"
}
```

The product side is the source of truth for these numbers (Cella
reads ClickHouse `billing_legs`). Auth does not re-aggregate.

The contract for the product side lives next to each product's
billing spec (Cella: `sandbox/specs/core/76`).

## Auth-side responsibilities

### Reconcile worker

`internal/billing/reconcile.go` — a periodic worker that, for each
known product (initially Cella; Lux and Workbench follow):

1. Lists payers with at least one push row in the target period.
2. For each `(product, payer, period)`, calls the product's
   `/internal/billing/reconcile` with the product's own service
   JWT. (Auth holds these JWTs; one per product.)
3. Compares the response total to the sum of
   `billing_meter_pushes.quantity` for the same key.
4. Persists both numbers and the diff in
   `billing_reconciliations` (see migration below).
5. If `|push_total - pull_total| > tolerance` (default 0), emit a
   `billing.reconcile.drift` audit event.

Cadence: configurable, defaults below.

| Cadence | Default | Purpose |
|---|---|---|
| Mid-period | every 6h | Early drift detection |
| Period close | once at `period_end + 1h` | Authoritative invoice number |

Period-close runs are the ones whose `pull_total` is treated as
the invoice quantity. The push log remains the engine for Stripe
Meter events; reconciliation does not retroactively mutate Stripe.
Drift outside tolerance becomes a manual investigation, not an
automatic write to Stripe.

### Migration `000030_billing_reconciliations.up.sql`

```sql
CREATE TABLE billing_reconciliations (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    product         TEXT         NOT NULL,
    payer_kind      TEXT         NOT NULL CHECK (payer_kind IN ('org','principal')),
    payer_id        UUID         NOT NULL,
    period_start    TIMESTAMPTZ  NOT NULL,
    period_end      TIMESTAMPTZ  NOT NULL,
    kind            TEXT         NOT NULL,        -- meter kind, e.g. sandbox.seconds
    meter_id        TEXT         NOT NULL,
    push_total      NUMERIC(18,6) NOT NULL,
    pull_total      NUMERIC(18,6) NOT NULL,
    delta           NUMERIC(18,6) GENERATED ALWAYS AS (pull_total - push_total) STORED,
    cadence         TEXT         NOT NULL CHECK (cadence IN ('mid_period','period_close')),
    computed_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (product, payer_kind, payer_id, period_start, kind, cadence)
);
CREATE INDEX billing_reconciliations_drift_idx
    ON billing_reconciliations (product, period_start)
    WHERE delta <> 0;
```

The index isolates non-zero-drift rows for the admin UI and the
audit emitter without scanning the full reconciliation log.

### Service-account scopes

Auth seeds a service account **per product** so each product can
publish its reconcile endpoint behind a dedicated JWT:

```sql
INSERT INTO service_accounts (id, name, scopes, created_at) VALUES
  ('00000000-0000-0000-0000-00000000B112', 'auth-reconcile-cella',     ARRAY['billing:reconcile'], now());
-- lux + workbench rows added when those products onboard
```

Each product validates the bearer at its `/internal/billing/reconcile`
handler with scope `billing:reconcile-serve` (product-side scope —
auth-side caller scope is `billing:reconcile`).

### Failure modes

| Failure | Behavior |
|---|---|
| Product 5xx / timeout | Retry with exponential backoff; after 3 failures emit `billing.reconcile.unavailable` audit, leave existing row intact. |
| Product 404 (no usage) | Insert reconciliation row with `pull_total=0`; drift is "real" if `push_total > 0`. |
| Product returns drift > tolerance at period close | Emit `billing.reconcile.drift_close`; do **not** automatically true-up Stripe. Manual investigation. |
| Auth restarts mid-pass | Worker resumes at next tick; UNIQUE constraint prevents duplicate rows for the same `(product, payer, period, kind, cadence)`. |

Open question (memo §5): "What if reconciliation is unavailable at
period close?" Recommendation: ship the `unavailable` audit and
hold invoice generation for the affected payer until reconciliation
succeeds. v1 prefers correctness over throughput here.

## Admin surface

`GET /internal/billing/reconciliations?period_start=…&drift_only=true`
returns the rows for an admin UI tab. Out of scope for this spec
to draw the UI; add to `impl-08-admin-ui.md` follow-up.

## Tests

`internal/billing/reconcile_test.go`:

- Mock product server returns `{items: [...], total}`; worker
  inserts row with `pull_total` matching.
- Mock product 5xx → retry → 200; one row inserted.
- Mock product 5xx three times → no row, audit emitted.
- Drift detection: push log shows 100, pull returns 95; row has
  `delta = -5`, drift index includes it, audit emitted.
- Idempotency: running the worker twice for the same period and
  cadence updates `computed_at` but leaves `pull_total` correct
  via `ON CONFLICT (... cadence) DO UPDATE`.

## Out of scope

- Mutating Stripe based on reconciliation drift. Drift is a
  signal, not an action. v1 surfaces it for ops.
- Aggregating across products into a unified invoice line. The
  Stripe Meter is per-product today; one invoice line per
  product is fine.
- `principal_id` payer mode (the worker handles it via the
  `payer_kind` column, but personal-payer reconciliation is not
  exercised until a personal-paid product ships).

## Acceptance

- `make migrate-up` lands `000030` cleanly.
- Reconcile worker boots from `cmd/auth/main.go`, starts on
  `onLeading` (mirrors meter-push worker pattern).
- Mock-mode CI exercises drift detection with a deterministic
  fake product server.
- Admin endpoint returns drift rows for the dashboard.
