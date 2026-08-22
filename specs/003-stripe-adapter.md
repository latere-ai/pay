---
title: pay/stripe — one proven integration, one design reference
status: drafted
repo: latere-ai/pay
package: latere.ai/x/pay/stripe
effort: medium
created: 2026-08-22
updated: 2026-08-22
author: changkun
trigger: replichai is the only Stripe integration in the family that has ever taken a payment. auth shipped a second one that was never used end to end and is being deleted. The adapter is built on the proven one, with the shapes the unproven one worked out carried over as design and proven here for the first time.
---

# pay/stripe

## One proven integration, one design reference

**replichai is the only Stripe integration in the family that has ever
taken a payment.** auth shipped a second one, on stripe-go v85, that was
never used end to end; its code is deleted by
[pay-02](pay-02-remove-dead-billing.md). What survives from it is design,
and it is labelled as such below because unproven code that reads as
proven is how a bug gets inherited with confidence.

| Capability | replichai (v82, **in production**) | auth (v85, **never used**, deleted) |
|---|---|---|
| Checkout mode | payment, one-off top-up | subscription |
| Save a method | no | setup-mode session |
| Billing portal | no | yes |
| Off-session charge | no | no |
| Refunds and disputes | yes | no |
| Async EU methods (SEPA, iDEAL, Bancontact) | yes | no |
| Managed Payments disable | yes | no |
| API-version skew tolerance | yes | yes |
| Webhook replay dedupe | in the ledger, by reference | a `stripe_webhook_events` table |
| Meter push (postpaid) | not applicable | yes, with a `SKIP LOCKED` outbox |
| Coverage | 57.7%, `CreateCheckout` untested | 100%, via an httptest stub |

Read that last row carefully. auth's 100% measures tests against a stub,
not behaviour against Stripe. It is evidence the *harness* works, not
that the *adapter* does.

## Requirements from replichai, which are load-bearing

Three behaviors learned from production failures. Each needs a regression
test named for the failure it prevents.

1. **Managed Payments must be explicitly disabled.**
   `params.AddExtra("managed_payments[enabled]", "false")`. It is
   default-on for new Stripe accounts, has no typed field in the SDK, and
   when left on it demands a product tax code and adds tax on top of the
   quoted total, so the customer is charged more than the app quoted. The
   exact parameter name came from Stripe's own error message.

2. **An async payment method pays twice and must credit once.** A card
   pays synchronously, so `checkout.session.completed` is already `paid`.
   SEPA Direct Debit, iDEAL and Bancontact, which is what EU customers
   reach for, leave `completed` unpaid and confirm later with
   `async_payment_succeeded`. Emit `KindPaid` only for a *paid* session,
   so two deliveries for one purchase credit exactly once and the
   ledger's dedupe is the second line of defence rather than the only one.

3. **A refund reverses the exact micro-USD credited, not the amount
   paid.** A EUR charge converted at purchase time and reversed at a
   later rate would leave a drift the ledger cannot account for. The
   reversal carries its own reference, distinct from the purchase's, so
   it dedupes independently.

## Requirements with no proven implementation behind them

These come from auth's integration, which was written carefully and never
ran against a real payment. Nothing is copied; each is a requirement to
be built here and proven here for the first time. They are separated from
the section above precisely so nobody mistakes a good idea for a tested
one.

- **The httptest harness.** `stripe-go` allows replacing the backend with
  one built over a custom `http.Client`, so tests point at an
  `httptest.Server` returning recorded payloads. This is the single most
  valuable thing auth's billing produced: it is what makes a 95% floor
  reachable on a vendor adapter, and it covers request shaping including
  the nested form encoding for `line_items[0][price_data][...]`, the part
  most likely to break silently on an SDK bump.
- **`IgnoreAPIVersionMismatch` on webhook construction.** The signature
  authenticates the delivery and only stable fields are read, so an
  endpoint on a different API version than the pinned SDK must not crash
  crediting. A bad signature still fails closed. Both implementations
  arrived at this independently, which is the strongest available signal
  that it is load-bearing.
- **A webhook-replay table.** replichai leans entirely on the ledger's
  unique index, correct for a credit but silent for an event that is not
  a ledger write. Carry the table so a replayed non-crediting event is
  also a no-op and an operator can retry a failed delivery.
- **Setup-mode sessions, the portal, and customer creation.** The shapes
  replichai never needed and auto-recharge does.
- **A `SKIP LOCKED` outbox.** Not needed now (there is no meter push any
  more) but the right pattern if postpaid metering returns.

## What the port adds beyond both

- **`ChargeSaved`**: a PaymentIntent with `off_session: true`,
  `confirm: true`, against the customer's default method. A `card_error`
  maps to `pay.ErrDeclined` and must never be retried; `requires_action`
  maps to `ChargePending` with a webhook to follow. Auto-recharge runs on
  this, and neither implementation has it.
- **Idempotency keys** on every mutating call. replichai has none, which
  is fine when a human clicks once and not fine when a daemon retries.
- **`ParseWebhook(payload, http.Header)`**: reads `Stripe-Signature` from
  the header set rather than a bare string, which is where the
  generalisation to PayPal costs nothing.
- **Capabilities**: `CapCheckout`, `CapSavedMethod`, `CapRefund`, and
  `CapTax` only when the deployment turns Stripe Tax on.

## Version

Pin **stripe-go v85**. replichai moves forward from v82; nothing in its
adapter depends on v82 specifics.

## Webhook signature

Verified without network by signing fixtures with a known secret:

$$
\text{sig} = \mathrm{HMAC\text{-}SHA256}\big(\text{secret},\; t \,\|\, \texttt{"."} \,\|\, \text{payload}\big)
$$

with `t` the timestamp from the `Stripe-Signature` header, compared in
constant time and rejected outside a tolerance window.

## Conformance

Runs `paytest.RunProviderContract` from spec 002, declaring the
capabilities it claims. An adapter that says it has `CapSavedMethod` and
returns `ErrUnsupported` fails the suite.

## Coverage

Target 95%, the same floor as the rest of the repo, reachable because of
the httptest harness. Any line that genuinely cannot be covered without a
live Stripe account is named in the spec's Outcome rather than left as an
unexplained gap.

## Out of scope

PayPal and Paddle. The port is shaped so they fit; neither is built now.

## Dependencies

- [002-payment-port](002-payment-port.md)
