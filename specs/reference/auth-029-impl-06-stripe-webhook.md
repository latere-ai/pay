---
title: Billing — Stripe webhook receiver + dispatcher
status: complete
depends_on:
  - specs/.archive/028-impl-05-stripe-checkout-portal.md
affects:
  - internal/handler/billing_webhook.go
  - internal/billing/stripe/webhook.go
  - internal/handler/router.go
created: 2026-05-02
author: codex
---

# Stripe webhook receiver

The only path subscription state changes from `incomplete` →
`active` → `past_due`. Without this, paid plans cannot be made
real.

## Route

```go
mux.HandleFunc("POST /webhooks/stripe", h.handleStripeWebhook)
```

**No JWT.** Authenticated by HMAC over the request body using
`STRIPE_WEBHOOK_SECRET`.

CSRF is N/A (Stripe is not a browser). Body size cap raised to
1 MiB for this route only — Stripe events can be larger than
auth's default cap.

## Verification

```go
event, err := stripe.WebhookEvent(rawBody, r.Header.Get("Stripe-Signature"), webhookSecret)
if err != nil { writeErr(w, 400, "bad_signature", "..."); return }
```

`stripe.WebhookEvent` (from `stripe-go/webhook`) handles the
HMAC + tolerance window. Reject `400` on mismatch; do not log
the body on a failure (potential PII / forged content).

In **mock mode** (`STRIPE_WEBHOOK_SECRET == ""`), the handler
parses the JSON without HMAC verification. Mock callers can
push synthetic events into the dispatcher this way; cella's
`e2e_mock` harness (sandbox spec 60) relies on this.

## Idempotent staging

```go
created, err := store.InsertWebhookEvent(ctx, WebhookEvent{
    ID: event.ID, Type: event.Type, Payload: event.Data.Raw,
})
if err != nil { writeErr(w, 500, ...); return }
if !created { writeJSON(w, 200, map[string]any{"replay": true}); return }
```

PK on `stripe_webhook_events.id` makes replays no-ops.

## Dispatch

```go
switch event.Type {
case "customer.subscription.created", "customer.subscription.updated":
    upsertSubscription(ctx, event)
case "customer.subscription.deleted":
    markCanceled(ctx, event)
case "customer.updated":
    syncCustomer(ctx, event)
case "payment_method.attached", "payment_method.detached":
    setPaymentMethodAttached(ctx, event)
case "invoice.payment_failed":
    // status update arrives via subscription.updated; log only
default:
    // store but don't process
    return
}
store.MarkWebhookProcessed(ctx, event.ID, processErr)
```

### `upsertSubscription`

Read `customer`, `status`, `current_period_start`,
`current_period_end`, `cancel_at_period_end`. The opaque plan
string comes from `metadata.plan` if set (mock mode + Checkout-
created subs via impl-05); otherwise from
`items[0].price.lookup_key`; otherwise from `items[0].price.id`
as a last resort.

Resolve `org_id` by reverse-lookup on `billing_customers
(stripe_customer_id)`. If the customer is not in our DB, log a
warning and stage but do not process — the event is staged for
later inspection but should be rare (lost migration data).

### `markCanceled`

Update `status="canceled"`. Do **not** `DELETE` the row; the
admin UI needs the trail.

### `setPaymentMethodAttached`

Update `billing_customers.payment_method_attached`. Don't
trigger any subscription change.

## Replay safety

If `MarkWebhookProcessed` records an `error`, the next
operator-driven retry can re-process by clearing
`processed_at`. Add an admin operation in impl-08 for this.
Stripe also retries automatically when we return non-2xx, so
returning `500` on transient errors is the normal recovery
path.

## Cella cache

Cella's `PlanResolver` TTL is 30s; that's the upper bound on
how stale entitlement is after a webhook lands. v1 does not
push an invalidation back to cella. (Decision pinned in
sandbox spec 60.)

## Tests

`internal/handler/billing_webhook_test.go`:

- Bad signature → 400.
- Missing signature → 400.
- Replay (same `event.id`) → 200 with `{replay:true}`.
- `customer.subscription.created` upserts row with correct
  fields.
- `customer.subscription.updated(status="past_due")` updates
  the row in place.
- `customer.subscription.deleted` sets `status="canceled"` and
  keeps the row.
- Unknown `customer` (no `billing_customers` row) stages the
  event but does not process.

## Acceptance

- HMAC verification rejects forged events.
- Replays of the same event id are no-ops at the storage layer.
- Mock-mode webhook ingest works without a secret.
- `customer.subscription.*` events round-trip into the
  subscription table.
