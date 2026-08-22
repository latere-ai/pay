# Reference: the auth billing design

**None of this ever took a payment.** `latere-ai/auth` built a complete
Stripe integration, wired it to nothing, and it was removed unused in
August 2026. These documents are its design record, recovered from that
repo's history and moved here so the thinking survives where money now
lives, rather than sitting in an identity provider that no longer has a
billing concern.

Read them as design input. Anything here that reads as a proven pattern
is a proven *idea* at best: the code that implemented it was never
exercised against a real charge.

**The one Stripe integration in the family that has taken a payment is
replichai's**, and its specs stay in `latere-ai/replichai/specs` until
its code migrates onto this repo. That is the behaviour to trust when
these documents and replichai disagree.

## What is worth mining

| Topic | File |
|---|---|
| The parent design: Stripe identity, subscription state, the meter bridge | `auth-023-billing.md` |
| Operator setup: products, prices, meters, webhook config, the customer portal | `auth-stripe-setup.md` |
| Persistence and the four tables | `auth-024-impl-01-persistence.md` |
| A mock-mode service that boots without credentials | `auth-025-impl-02-mock-service.md` |
| Checkout and portal wrappers | `auth-028-impl-05-stripe-checkout-portal.md` |
| Webhook receiver, dispatcher, and the replay table | `auth-029-impl-06-stripe-webhook.md` |
| The meter-push outbox | `auth-030-impl-07-meter-push-worker.md`, `auth-052-billing-worker-locking.md` |
| Admin surface | `auth-031-impl-08-admin-ui.md` |
| Rollout and configuration | `auth-032-impl-09-rollout.md` |

## Superseded thinking, kept deliberately

`auth-036` through `auth-039` (`impl-10` envelope and reservations,
`impl-11` pull reconciliation, `impl-12` budget enforcement, `impl-13`
across-products credit application) were marked **superseded, pending
redesign** in auth before this component existed. They describe a
cross-product billing aggregator with a single credit balance per payer,
drained chronologically.

That model is **not** what `pay` builds: this repo keeps one ledger per
product, so a payer has a balance per product. The four documents are
kept because they are the clearest statement of the road not taken, and
because whoever reopens cross-product invoicing should read them first.

## The single most useful thing in here

The httptest-over-stripe-go test harness. `stripe-go` allows replacing
the backend with one built over a custom `http.Client`, so tests point at
an `httptest.Server` returning recorded payloads. It is why auth's client
reached 100% statement coverage while replichai's sits at 57.7% with
`CreateCheckout` untested, and it is what makes a 95% floor reachable on
a vendor adapter at all. See [`../003-stripe-adapter.md`](../003-stripe-adapter.md).
