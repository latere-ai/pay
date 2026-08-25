---
title: pay/stripe — one proven integration, one design reference
status: implemented
repo: latere-ai/pay
package: latere.ai/x/pay/stripe
effort: medium
created: 2026-08-22
updated: 2026-08-22
author: changkun
trigger: the origin product is the only Stripe integration in this family of services that has ever taken a payment. A second one was written elsewhere, never used end to end, and is being deleted. The adapter is built on the proven one, with the shapes the unproven one worked out carried over as design and proven here for the first time.
---

# pay/stripe

## One proven integration, one design reference

**The origin product is the only Stripe integration in this family of services
that has ever taken a payment.** A second one, on stripe-go v85, was written
elsewhere and never used end to end; that code has since been removed. What
survives from it is design, and it is labelled as such below because unproven
code that reads as proven is how a bug gets inherited with confidence.

| Capability | The origin product (v82, **in production**) | The second integration (v85, **never used**, removed) |
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

Read that last row carefully. the second implementation's 100% measures tests against a stub,
not behaviour against Stripe. It is evidence the *harness* works, not
that the *adapter* does.

## Requirements learned in production

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

These come from the second implementation's integration, which was written carefully and never
ran against a real payment. Nothing is copied; each is a requirement to
be built here and proven here for the first time. They are separated from
the section above precisely so nobody mistakes a good idea for a tested
one.

- **The httptest harness.** `stripe-go` allows replacing the backend with
  one built over a custom `http.Client`, so tests point at an
  `httptest.Server` returning recorded payloads. This is the single most
  valuable thing the second implementation's billing produced: it is what makes a 95% floor
  reachable on a vendor adapter, and it covers request shaping including
  the nested form encoding for `line_items[0][price_data][...]`, the part
  most likely to break silently on an SDK bump.
- **`IgnoreAPIVersionMismatch` on webhook construction.** The signature
  authenticates the delivery and only stable fields are read, so an
  endpoint on a different API version than the pinned SDK must not crash
  crediting. A bad signature still fails closed. Both implementations
  arrived at this independently, which is the strongest available signal
  that it is required.
- **A webhook-replay table.** The origin product leans entirely on the ledger's
  unique index, correct for a credit but silent for an event that is not
  a ledger write. Carry the table so a replayed non-crediting event is
  also a no-op and an operator can retry a failed delivery.
- **Setup-mode sessions, the portal, and customer creation.** The shapes
  the origin product never needed and auto-recharge does.
- **A `SKIP LOCKED` outbox.** Not needed now (there is no meter push any
  more) but the right pattern if postpaid metering returns.

## What the port adds beyond both

- **`ChargeSaved`**: a PaymentIntent with `off_session: true`,
  `confirm: true`, against the customer's default method. A `card_error`
  maps to `pay.ErrDeclined` and must never be retried; `requires_action`
  maps to `ChargePending` with a webhook to follow. Auto-recharge runs on
  this, and neither implementation has it.
- **Idempotency keys** on every mutating call. The origin product has none, which
  is fine when a human clicks once and not fine when a daemon retries.
- **`ParseWebhook(payload, http.Header)`**: reads `Stripe-Signature` from
  the header set rather than a bare string, which is where the
  generalisation to PayPal costs nothing.
- **Capabilities**: `CapCheckout`, `CapSavedMethod`, `CapRefund`, and
  `CapTax` only when the deployment turns Stripe Tax on.

## Version

Pin **stripe-go v85**. The origin product moves forward from v82; nothing in its
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

## Outcome

Implemented 2026-08-22 as `latere.ai/x/pay/stripe`, on stripe-go v85.2.0,
at **100% statement coverage** of the package.

`New(Config)` never returns nil; without both secrets every operation is
`ErrUnconfigured`. `CreateCheckout` is payment-mode with inline
`price_data`, Managed Payments disabled per session, automatic tax off
unless the deployment turned Stripe Tax on *and* the checkout asked for
it, `setup_future_usage: off_session` with `customer_creation: always`
when a method is being saved, and the caller's idempotency key when there
is one. `EnsureCustomer` searches then creates under a key derived from
the address. `ChargeSaved` confirms an off-session PaymentIntent.
`ParseWebhook` maps exactly the five events listed in
[the Stripe operations guide](../docs/stripe-operations.md).

The SDK client is per-adapter (`stripe.NewClient` with injected backends)
rather than the package-level globals both references used: two products
in one process can then hold different keys, and the test harness needs
no global mutation.

### What the spec got wrong

- **"no typed field in the SDK"** is true of v82 and stale for v85, which
  has `CheckoutSessionCreateManagedPaymentsParams`. `AddExtra` is kept —
  it is what the proven integration sends and the wire bytes are
  identical — and the regression test asserts the parameter appears
  **exactly once**, so adopting the typed field later without removing
  the `AddExtra` fails rather than silently sending two values.
- **`requires_action` is not how an off-session challenge arrives.** The
  spec describes it as a result; Stripe answers 402 with a `card_error`
  whose code is `authentication_required`, carrying the intent on the
  error. Mapping every `card_error` to `ErrDeclined` would turn each EU
  3-D Secure challenge into a permanent decline. Both shapes are handled
  and both are covered.
- **The port documents the adapter following a charge to its balance
  transaction** for the USD a converted charge is worth. `ParseWebhook`
  takes no context and no network, so the figure comes from the session's
  `currency_conversion.amount_total`, which is the total in the creation
  currency and is what Adaptive Pricing puts there. No API call, same
  number, and a delivery still parses with the account unreachable.
- **`payment_intent.payment_failed` has no `Kind`.** The port models
  money moving; a failed charge moved none. It reduces to `KindIgnored`
  carrying the intent's reference and metadata, which `WebhookHandler`
  drops. A consumer wanting auto-recharge telemetry has to call
  `ParseWebhook` itself, or the port needs a kind for it.

### Where the two references disagreed

- **Which refund a `charge.refunded` is about.** The origin product takes
  `refunds.data[n-1]`. Stripe returns list objects newest-first, so that
  is the *oldest* refund: a second partial refund would re-emit a
  reference the ledger already posted and the clawback would vanish into
  the dedupe. This adapter picks the refund with the greatest `created`.
  The regression fixture carries two refunds, because a one-refund
  fixture passes under either rule and pins nothing.
- **Idempotency on customer creation.** Neither reference has any.
  Stripe search is eventually consistent, so a retry seconds after the
  first call can miss and reach the create; the key derived from the
  address is what makes that create return the original customer.

### Not built here

The webhook-replay table, the billing portal, setup-mode sessions and the
`SKIP LOCKED` outbox are consumer- or storage-side and have no method on
`pay.Provider`. Saving a method is covered by `SaveMethod` on a payment
session, which is what auto-recharge needs; a portal would be a new port
method rather than an adapter detail.

### What is not covered, and why

Nothing in the package is uncovered. Three behaviours are *asserted
against a stub rather than against Stripe*, and only a live test-mode run
closes that gap. Named here rather than left implicit, with the card from
[the Stripe operations guide](../docs/stripe-operations.md) that exercises
each:

| Behaviour | Card |
|---|---|
| Managed Payments actually off, so the charge equals the quote | any, checked on the resulting PaymentIntent |
| `authentication_required` really is a 402 card_error | `4000 0025 0000 3155` |
| An async method really leaves `completed` unpaid | a SEPA or iDEAL test payment |

### Found by the fuzzer

`FuzzParseWebhook` signs each input with the harness secret, so it
exercises decoding rather than the HMAC. It found two crashes in the
first minute, both now regression tests: an event with no `data` member
nil-dereferenced the endpoint, and a paid session with no payment intent
produced a credit with no reference, which the ledger cannot dedupe and
which would therefore post again on every redelivery. Both now fail
closed, and reversals gained the same guard.
