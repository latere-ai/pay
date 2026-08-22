---
title: Billing — wire envelope extensions + enterprise/credits column reservations
status: superseded (pending redesign, see specs/060-billing-redesign.md)
depends_on:
  - specs/.archive/023-billing.md
  - specs/.archive/024-impl-01-persistence.md
  - specs/.archive/027-impl-04-internal-handlers.md
affects:
  - migrations/000029_billing_envelope_and_reservations.{up,down}.sql
  - internal/billing/store.go
  - internal/billing/types.go
  - internal/handler/billing_internal.go
created: 2026-05-09
author: codex
trigger: Identity strategy billing-aggregator section (specs repo, products/identity.md#billing-aggregator-role) — lock the envelope shape before Lux + Workbench start reporting; pre-seat enterprise allocation and credits columns while the table is small.
---

# Billing — envelope extensions + reservations

Additive-only changes to the wire contract and persistence shape.
No behavior change at v1 deploy time. The point is to land the
columns and fields **before** a second product (Lux, then Workbench)
starts reporting usage, so we don't pay a multi-product migration
later.

## Why now

The Identity strategy's billing-aggregator section
([`specs/products/identity.md`](https://github.com/latere-ai/specs/blob/main/products/identity.md#billing-aggregator-role))
flags four reservations that are cheap today and expensive after
several million `billing_meter_pushes` rows:

1. `user_id` on every billable event for attribution and dispute
   investigation. Today the wire is org-only.
2. Free-form `metadata` per event for product-specific detail
   (sandbox id, model name, run id).
3. Enterprise allocation: `cost_center` and `project_tag` on
   payer rows and push rows.
4. Credits: an empty `billing_credits` table plus a
   `credits_applied` column on push rows. The credit-application
   policy is pinned (across-products, chronological — see
   "Credit application policy" below) but the worker that
   consumes the table lives in
   [`impl-13`](039-impl-13-credit-application.md); v1 deploy ships
   with zero rows.

This spec lands all four as one migration (`000029`). No code
path consumes them yet beyond persistence and round-trip on the
wire; impl-11, impl-12, and impl-13 are where they earn their
keep.

## Credit application policy

Decision (memo §"Credits, free tiers, promotions"):
**across-products, chronological by event timestamp.**

- A credit row is keyed on `(payer_kind, payer_id)`, **not** on a
  product. Any product's billable event can consume it.
- Cella's free tier (`free_tier_seconds`) is a *usage allowance*,
  not a credit balance — left untouched. Credits are for promo,
  refund, and enterprise commitment use cases.
- Pinned now, before any credit ships, because the migration cost
  of a per-product → across-products switch (data backfill, policy
  reconciliation, customer-expectation breakage) is much higher
  than the cost of an empty table sitting in v1 production.

The worker design and Stripe-deduction integration live in
[`impl-13`](039-impl-13-credit-application.md). This spec only lands
the schema reservations.

## Migration `000029_billing_envelope_and_reservations.up.sql`

```sql
-- Enterprise allocation hints. NULLable; no behavior change today.
ALTER TABLE billing_customers
    ADD COLUMN cost_center  TEXT NULL,
    ADD COLUMN project_tag  TEXT NULL;

ALTER TABLE billing_subscriptions
    ADD COLUMN cost_center  TEXT NULL,
    ADD COLUMN project_tag  TEXT NULL;

-- Per-event additions. user_id is the actor that triggered the event;
-- nullable because Cella's reporter today aggregates org-only and
-- backfills NULL until cella spec 76 lands.
ALTER TABLE billing_meter_pushes
    ADD COLUMN user_id        UUID         NULL REFERENCES principals(id) ON DELETE SET NULL,
    ADD COLUMN metadata       JSONB        NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN cost_center    TEXT         NULL,
    ADD COLUMN project_tag    TEXT         NULL,
    ADD COLUMN credits_applied NUMERIC(18,6) NOT NULL DEFAULT 0;

CREATE INDEX billing_meter_pushes_user_idx
    ON billing_meter_pushes (user_id) WHERE user_id IS NOT NULL;

-- Credits: empty in v1. Shape only.
CREATE TABLE billing_credits (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    payer_kind      TEXT         NOT NULL CHECK (payer_kind IN ('org','principal')),
    payer_id        UUID         NOT NULL,
    amount          NUMERIC(18,6) NOT NULL,
    currency        TEXT         NOT NULL DEFAULT 'USD',
    source          TEXT         NOT NULL,        -- 'promo' | 'refund' | 'manual' | …
    note            TEXT         NULL,
    expires_at      TIMESTAMPTZ  NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    consumed_amount NUMERIC(18,6) NOT NULL DEFAULT 0,
    CONSTRAINT billing_credits_amount_positive CHECK (amount > 0),
    CONSTRAINT billing_credits_consumed_le_amount CHECK (consumed_amount <= amount)
);
CREATE INDEX billing_credits_payer_idx
    ON billing_credits (payer_kind, payer_id);
```

`down.sql` drops the new columns, the index, and the table. Safe
because no row consumes credits in v1.

## Wire contract additions

`POST /internal/billing/usage` body grows two optional fields:

```json
{
  "org_id":          "uuid",
  "user_id":         "uuid",         // NEW — actor that triggered the event
  "kind":            "sandbox.seconds",
  "meter_id":        "mtr_...",
  "quantity":        1234,
  "period_start":    "RFC3339",
  "idempotency_key": "<org>:<kind>:<period>:<seq>",
  "metadata":        { "sandbox_id": "..." }   // NEW — JSON object, ≤ 4 KiB
}
```

Validation deltas (handler in `internal/handler/billing_internal.go`):

- `user_id`: optional, must parse as UUID when present; rejected
  with `400 bad_request` if malformed. Stored verbatim.
- `metadata`: optional, must be a JSON object (not array, not
  scalar); ≤ 4 KiB serialized; rejected with `400 metadata_too_large`
  or `400 metadata_not_object` otherwise. Stored verbatim. Auth
  never inspects keys.

Existing semantics unchanged: PK on `idempotency_key`,
`INSERT … ON CONFLICT DO NOTHING`, replays return
`200 {"queued": false}`. The new columns are populated on the
**first** insert only; replays do not overwrite.

`GET /internal/billing/account/*` envelope is unchanged in v1.
Allocation hints are write-only until enterprise rollout; do not
expose `cost_center` / `project_tag` on the read path until that
work lands.

## Store changes

`internal/billing/store.go`:

- `MeterPush` struct grows `UserID *uuid.UUID`,
  `Metadata json.RawMessage`, `CostCenter *string`,
  `ProjectTag *string`, `CreditsApplied decimal.Decimal`.
- `StageMeterPush` accepts the new fields; existing callers pass
  zero values until cella spec 76 ships.
- New no-op store method `ListCredits(ctx, payer)` for impl-12 to
  build on (returns empty in v1).

## Mock-mode parity

`internal/billing/mock.go` records the new fields in its
in-memory event log so cella's CI can assert round-trip.

## Tests

`internal/handler/billing_internal_test.go` additions:

- `POST /internal/billing/usage` with `user_id` + `metadata`
  round-trips (verify via store).
- `metadata` rejection at 4 KiB + 1.
- `metadata` rejection on array/scalar payloads.
- Replay of an event whose first insert carried `metadata` does
  not overwrite the row.
- `billing_credits` rows insert and round-trip via `ListCredits`.

## Out of scope

- **Applying** credits during meter push (impl-12 territory).
- **Reading** `cost_center` / `project_tag` on any API surface.
- Cella reporter-side envelope changes (sandbox spec 76).
- `metadata` schema validation per `kind` — kept opaque.

## Acceptance

- `make migrate-up` lands `000029` cleanly; `make migrate-down`
  reverses it.
- Existing `cella-billing` clients that omit `user_id` / `metadata`
  continue to succeed (additive contract).
- Once sandbox spec 76 deploys, `user_id` and `metadata` populate
  on every new row; old rows stay NULL / `'{}'`.
