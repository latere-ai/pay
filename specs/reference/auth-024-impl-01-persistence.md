---
title: Billing — persistence (migration 000025 + store)
status: complete
depends_on:
  - specs/.archive/023-billing.md
affects:
  - migrations/000025_billing.up.sql
  - migrations/000025_billing.down.sql
  - internal/billing/store.go
created: 2026-05-02
updated: 2026-05-08
author: codex
---

# Billing persistence — migration + store

Foundation for everything else. Tables and a narrow Go store
interface that the service and worker layers consume. No HTTP,
no Stripe SDK.

> **Schema delta (2026-05-08):** original draft scoped customers
> and subscriptions to `org_id` only. Per `billing.md` §"Spec
> deltas", both payer kinds (`org` and `principal`) are
> first-class. The schema below uses two nullable FK columns
> with a one-of `CHECK` and partial unique indices instead of
> a single `org_id PRIMARY KEY`.

## Migration `000025_billing.up.sql`

```sql
CREATE TABLE billing_customers (
    id                      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id                  UUID        REFERENCES organizations(id) ON DELETE CASCADE,
    principal_id            UUID        REFERENCES principals(id)    ON DELETE CASCADE,
    stripe_customer_id      TEXT        UNIQUE NOT NULL,
    payment_method_attached BOOLEAN     NOT NULL DEFAULT false,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_customers_payer_oneof
        CHECK ((org_id IS NOT NULL) <> (principal_id IS NOT NULL))
);
CREATE UNIQUE INDEX billing_customers_org_uniq
    ON billing_customers (org_id) WHERE org_id IS NOT NULL;
CREATE UNIQUE INDEX billing_customers_principal_uniq
    ON billing_customers (principal_id) WHERE principal_id IS NOT NULL;

CREATE TABLE billing_subscriptions (
    id                      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id                  UUID        REFERENCES organizations(id) ON DELETE CASCADE,
    principal_id            UUID        REFERENCES principals(id)    ON DELETE CASCADE,
    stripe_subscription_id  TEXT        UNIQUE NOT NULL,
    plan                    TEXT        NOT NULL,
    status                  TEXT        NOT NULL CHECK (status IN (
                                'active', 'trialing', 'past_due',
                                'canceled', 'unpaid', 'incomplete',
                                'incomplete_expired')),
    current_period_start    TIMESTAMPTZ NOT NULL,
    current_period_end      TIMESTAMPTZ NOT NULL,
    cancel_at_period_end    BOOLEAN     NOT NULL DEFAULT false,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_subscriptions_payer_oneof
        CHECK ((org_id IS NOT NULL) <> (principal_id IS NOT NULL))
);
CREATE UNIQUE INDEX billing_subscriptions_org_uniq
    ON billing_subscriptions (org_id) WHERE org_id IS NOT NULL;
CREATE UNIQUE INDEX billing_subscriptions_principal_uniq
    ON billing_subscriptions (principal_id) WHERE principal_id IS NOT NULL;
CREATE INDEX billing_subscriptions_status_idx
    ON billing_subscriptions (status);

CREATE TABLE stripe_webhook_events (
    id           TEXT        PRIMARY KEY,
    type         TEXT        NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ,
    error        TEXT,
    payload      JSONB       NOT NULL
);
CREATE INDEX stripe_webhook_events_unprocessed_idx
    ON stripe_webhook_events (received_at)
    WHERE processed_at IS NULL;

CREATE TABLE billing_meter_pushes (
    idempotency_key TEXT        PRIMARY KEY,
    org_id          UUID        NOT NULL REFERENCES organizations(id),
    meter           TEXT        NOT NULL,
    quantity        BIGINT      NOT NULL CHECK (quantity >= 0),
    period_start    TIMESTAMPTZ NOT NULL,
    period_end      TIMESTAMPTZ NOT NULL,
    pushed_at       TIMESTAMPTZ,
    error           TEXT,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX billing_meter_pushes_unsent_idx
    ON billing_meter_pushes (received_at)
    WHERE pushed_at IS NULL;
CREATE INDEX billing_meter_pushes_org_idx
    ON billing_meter_pushes (org_id, period_start);
```

`billing_meter_pushes.org_id` stays `NOT NULL` and references
`organizations` only — Slice A doesn't ship metered personal
products, and cella's existing wire (`POST /internal/billing/usage`)
sends per-org rows. If principal-scoped metering arrives later,
add a parallel nullable `principal_id` column and a one-of
`CHECK`, mirroring the customers/subscriptions shape.

`billing_meter_pushes.quantity` is `BIGINT` — cella sends
floating-point seconds but multiplies by the meter's resolution
(milliseconds for `sandbox.cpu_milli_seconds`, whole seconds for
`sandbox.seconds`) before sending. Auth rejects fractional
quantities at the handler layer.

The `WHERE pushed_at IS NULL` and `WHERE processed_at IS NULL`
partial indices keep the worker drain queries cheap as the
tables grow.

## `000025_billing.down.sql`

Reverses in dependency order:

```sql
DROP TABLE IF EXISTS billing_meter_pushes;
DROP TABLE IF EXISTS stripe_webhook_events;
DROP TABLE IF EXISTS billing_subscriptions;
DROP TABLE IF EXISTS billing_customers;
```

## Payer abstraction (Go side)

`internal/billing/payer.go`:

```go
type PayerKind string

const (
    PayerOrg       PayerKind = "org"
    PayerPrincipal PayerKind = "principal"
)

type Payer struct {
    Kind PayerKind
    ID   uuid.UUID
}

func OrgPayer(id uuid.UUID) Payer       { return Payer{Kind: PayerOrg, ID: id} }
func PrincipalPayer(id uuid.UUID) Payer { return Payer{Kind: PayerPrincipal, ID: id} }
```

Every `Customer` and `Subscription` carries a `Payer`. The store
translates that to whichever column to filter on.

## Store interface

`internal/billing/store.go`:

```go
type Store interface {
    // Customers — keyed by Payer
    GetCustomer(ctx context.Context, p Payer) (Customer, error)
    UpsertCustomer(ctx context.Context, c Customer) error
    SetPaymentMethodAttached(ctx context.Context, p Payer, attached bool) error

    // Subscriptions — keyed by Payer
    GetSubscription(ctx context.Context, p Payer) (Subscription, error)
    UpsertSubscription(ctx context.Context, s Subscription) error
    DeleteSubscription(ctx context.Context, p Payer) error

    // Webhook events
    InsertWebhookEvent(ctx context.Context, e WebhookEvent) (created bool, err error)
    MarkWebhookProcessed(ctx context.Context, id string, perr error) error
    UnprocessedWebhookEvents(ctx context.Context, limit int) ([]WebhookEvent, error)

    // Meter pushes (org-scoped only in Slice A)
    StageMeterPush(ctx context.Context, p MeterPush) (created bool, existing MeterPush, err error)
    UnsentMeterPushes(ctx context.Context, limit int) ([]MeterPush, error)
    MarkMeterPushed(ctx context.Context, key string, perr error) error
}
```

Returns `ErrNotFound` (sentinel) for missing rows; handlers map
that to `404` for `/internal/billing/account/*`.

`StageMeterPush` returns `(created=false, existing, nil)` on PK
conflict so the handler can return the `already_pushed_at` value
the cella contract specifies.

Implementation backed by raw `pgx` queries in
`internal/billing/store.go`, matching the rest of the auth repo.

## Tests

`store_pgtest.go` (against the test Postgres helper used by other
auth integration tests):

- Customer/subscription CRUD round-trips for **both** payer kinds.
- One-of `CHECK` rejects rows with both `org_id` and `principal_id`
  set, and rows with neither.
- Partial unique index lets the same `org_id` and `principal_id`
  values coexist across separate rows (an org and a principal
  with the same UUID — pathological but legal).
- Webhook PK collision returns `created=false`.
- Meter-push PK collision returns existing row + `nil` error.
- Status `CHECK` rejects unknown strings.
- `Customer.Payer` round-trips (insert org-payer, read back as
  `PayerOrg`; same for principal).

## Acceptance

- Migration applies cleanly on a fresh DB and reverses cleanly.
- Store interface compiles; integration tests pass against the
  shared Postgres test container.
- No package outside `internal/billing` imports the store
  directly — all callers go through the service layer.
