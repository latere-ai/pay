---
title: Billing — Stripe Checkout + Portal wrapper
status: complete
depends_on:
  - specs/.archive/026-impl-03-user-handlers.md
  - specs/.archive/027-impl-04-internal-handlers.md
affects:
  - internal/billing/stripe/client.go
  - internal/billing/stripe/checkout.go
  - internal/billing/stripe/portal.go
  - internal/billing/service.go
created: 2026-05-02
updated: 2026-05-09
author: codex
---

# Stripe Checkout + Portal

> **Delta (2026-05-09):** added `CreateSetup` (Stripe Checkout in
> `mode=setup`) so users can attach a payment method without
> committing to a plan. Powers the dashboard's "Add payment method"
> CTA. Both Checkout and Setup sessions advertise `card` and
> `paypal` in `payment_method_types` so PayPal is a first-class
> option alongside cards. Future direction: native Stripe Elements
> for first-party card UX (no redirect) and a separate PayPal SDK
> button — both are PaymentView v2 work, not in Slice B.

The first piece that talks to a real Stripe API. Activated when
`STRIPE_SECRET_KEY` is non-empty; otherwise mock mode (impl-02)
remains in effect.

## Stripe SDK

Use the official `github.com/stripe/stripe-go/v85` (or whichever
major is current). Pin the version in `go.mod`. Wrap construction
so the rest of the package never imports `stripe.*` directly:

```go
// internal/billing/stripe/client.go
type Client struct {
    sc        *client.API
    plans     map[string]string // plan name → Price id
    successURL string
    cancelURL  string
    portalURL  string
    log        *slog.Logger
    now        func() time.Time
}
```

## Customer creation: lazy

`CreateCheckout(orgID, plan)`:

1. Look up `billing_customers` for `orgID`.
2. If absent: call `customer.New(&stripe.CustomerParams{
   Metadata: { "org_id": orgID, "platform": "latere" }
   })`. Insert into `billing_customers` (`stripe_customer_id`,
   `payment_method_attached=false`).
3. If present: reuse `stripe_customer_id`.
4. Resolve `priceID := plans[plan]`. Reject `400 unknown_plan`
   if missing.
5. Create a Checkout Session with mode `subscription`, line
   item `{Price: priceID, Quantity: 1}`, and metered Prices
   passed via `subscription_data.items[].price` if the plan
   includes metered components. Pass `metadata.plan = plan` so
   the webhook dispatcher can read it back as the opaque plan
   string.
6. Return `session.URL`.

This is **lazy** customer creation by design (see spec 60's
"Other boundary decisions"). No background sync from
`organizations` to Stripe at org-create time.

## `CreatePortal(orgID)`:

1. Require `billing_customers` row; `404 no_subscription`
   otherwise.
2. `billingportal.Session.New(&stripe.BillingPortalSessionParams{
   Customer: cust, ReturnURL: portalURL })`.
3. Return `session.URL`.

## Plan price map

Env var `BILLING_PLAN_PRICES_JSON`:

```json
{ "starter": "price_...", "pro": "price_...", "enterprise": "price_..." }
```

Loaded once at boot. Plan names must match cella's catalog
(`migrations/000012_plans.up.sql` in sandbox).

The metered price IDs (per-meter) live alongside the
subscription price as `recurring.usage_type=metered` Stripe
Prices and are configured in Stripe Dashboard. Auth doesn't
need to know individual metered Price IDs at Checkout time —
Stripe attaches them from the Product's default subscription
items, OR cella pre-configures a Subscription Schedule. v1:
default subscription items only; impl-05 verifies one
end-to-end pilot.

## Errors

- Stripe transient errors → `503 stripe_unavailable`. Cella's
  client surfaces this as a banner.
- Plan unknown → `400 unknown_plan`. Cella's `apiBillingCheckout`
  forwards.
- Org not a member of caller principal → `403 forbidden`
  (handled in the user handler, not here).

## Tests

Stripe wrapper tests use the Stripe Go SDK's
`stripe.SetBackend(stripe.APIBackend, stripe.GetBackend(...))`
hook to swap in a mock HTTP transport. Don't hit the real Stripe
API in CI.

- Lazy customer create: with no row, `customer.New` is called
  exactly once; the `metadata.org_id` matches.
- Repeat call: customer is reused; no second `customer.New`.
- Unknown plan rejected.
- Portal without customer returns `ErrNoCustomer`.

## Pilot

Before flipping `STRIPE_SECRET_KEY` in prod, run one pilot org
end-to-end against Stripe Test Mode:

1. Create org `pilot-acme` in auth.
2. Hit `POST /me/billing/checkout {plan:"pro"}` with a real
   user JWT.
3. Pay with Stripe test card.
4. Wait for `customer.subscription.created` webhook (impl-06).
5. Assert `billing_subscriptions(org_id="pilot-acme",
   status="active")` exists.
6. Run a sandbox in cella; observe a `billing_meter_pushes` row
   land via the cella reporter (sandbox spec 60); observe the
   meter-push worker (impl-07) report it to Stripe; verify in
   Stripe Dashboard.

This pilot is the gating criterion for the prod cutover in
impl-09.

## Acceptance

- Mock mode tests still pass (impl-02 untouched).
- Real-mode unit tests pass against mocked Stripe transport.
- Manual pilot in Stripe Test Mode succeeds end-to-end.
