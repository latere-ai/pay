---
title: Billing — mock-mode service
status: complete
depends_on:
  - specs/.archive/024-impl-01-persistence.md
affects:
  - internal/billing/service.go
  - internal/billing/mock.go
  - internal/billing/billing.go
created: 2026-05-02
author: codex
---

# Billing mock-mode service

The mock service is the unit-test backbone. Cella's CI runs
end-to-end against it without Stripe credentials. Implementing
this before the real Stripe wrapper de-risks the contract: any
ambiguity surfaces in the mock first.

> **Delta (2026-05-08):** Customer/subscription methods take a
> `Payer` (org-or-principal) instead of `orgID uuid.UUID`. See
> `billing.md` §"Spec deltas" for rationale and `impl-01` for the
> `Payer` type. Meter/webhook methods are unchanged — Slice A
> only meters orgs.

## Service interface

`internal/billing/service.go`:

```go
type Service interface {
    // User-facing — payer comes from JWT context (active-org or principal)
    GetMyBilling(ctx context.Context, p Payer) (MyBilling, error)
    CreateCheckout(ctx context.Context, p Payer, plan string) (string, error)
    CreatePortal(ctx context.Context, p Payer) (string, error)

    // Internal — cella always passes org payer in Slice A
    GetAccount(ctx context.Context, p Payer) (Account, error) // wire shape
    StageUsage(ctx context.Context, u UsageReport) (StageResult, error)

    // Worker hooks
    DrainMeterPushes(ctx context.Context, limit int) (drained int, err error)
    DispatchWebhook(ctx context.Context, raw []byte, sig string) error
}
```

Construction:

```go
type Config struct {
    StripeSecret      string                        // empty → mock mode
    WebhookSecret     string
    PlanPrices        map[string]string             // plan name → Stripe Price id
    CheckoutSuccess   string
    CheckoutCancel    string
    PortalReturn      string
    MockBaseURL       string                        // default "https://billing.mock"
    Now               func() time.Time              // injectable for tests
}

func New(store Store, cfg Config, log *slog.Logger) Service {
    if cfg.StripeSecret == "" {
        return &mockService{store: store, cfg: cfg, log: log}
    }
    return &stripeService{store: store, cfg: cfg, log: log, sc: stripe.NewClient(cfg.StripeSecret)}
}
```

`stripeService` is empty in this phase; impl-05 fills it. Handlers
written in impl-03 / impl-04 take `Service`, so they're agnostic
to mock vs real.

## Mock semantics

`internal/billing/mock.go`:

- `CreateCheckout(p Payer, plan)`:
  1. Upserts a `billing_customers` row with
     `stripe_customer_id = "cus_mock_<kind>_<id>"` (e.g.
     `cus_mock_org_019dc...` or `cus_mock_principal_018f...`).
  2. Returns `"<MockBaseURL>/checkout?<kind>_id=<id>&plan=<plan>"`.
- `CreatePortal(p Payer)`:
  1. Returns `"<MockBaseURL>/portal?<kind>_id=<id>"`.
- `DispatchWebhook(raw, sig)`:
  1. Mock mode bypasses signature verification when
     `WebhookSecret == ""`.
  2. Decodes `{ "id": "...", "type": "...", "data": {...} }`.
  3. Routes the same way the real dispatcher does (impl-06).
- `StageUsage`:
  1. Same INSERT-ON-CONFLICT path as real mode.
- `DrainMeterPushes`:
  1. Marks all `pushed_at IS NULL` rows as pushed with `now()`.
  2. No external call.

Mock-mode callers can drive subscription state by POSTing a
synthetic webhook to `/webhooks/stripe`:

```json
{
  "id": "evt_mock_001",
  "type": "customer.subscription.created",
  "data": {
    "object": {
      "id": "sub_mock_001",
      "customer": "cus_mock_<orgID>",
      "items": { "data": [ { "price": { "id": "price_pro" } } ] },
      "status": "active",
      "current_period_start": 1730419200,
      "current_period_end": 1733011200,
      "cancel_at_period_end": false,
      "metadata": { "plan": "pro" }
    }
  }
}
```

The dispatcher uses `metadata.plan` (when present) as the
opaque plan string, falling back to `items[0].price.lookup_key`
in real mode. Mock callers always set `metadata.plan`.

## Determinism

`Now` injection lets tests assert exact `current_period_*`
timestamps and idempotency-key roundtrips. Without injection,
`time.Now().UTC()` is used.

## Tests

`internal/billing/mock_test.go`:

- Checkout creates customer + returns deterministic URL.
- Portal returns deterministic URL.
- Webhook dispatch upserts subscription; replay (same `id`) is a
  no-op.
- StageUsage round-trip; replay returns existing.
- DrainMeterPushes marks all unsent rows.
- Account fallback: `GetAccount(payer)` with no rows returns
  `ErrNotFound` (handler maps to 404). Run for both `PayerOrg`
  and `PayerPrincipal`.

## Cella consumption

Cella's `e2e_mock` harness (spec 60, Verification) boots auth
with `STRIPE_SECRET_KEY=""` and exercises the full flow.

## Acceptance

- Mock service compiles and passes its own tests.
- Cella's billing client tests can be re-pointed at a mock-mode
  auth instance and pass without modification.
