---
title: Running the Stripe account
status: drafted
repo: latere-ai/pay
effort: small
created: 2026-08-22
updated: 2026-08-22
author: changkun
trigger: The adapter is code; a working integration is also an account configured correctly. Getting the webhook event list, the presentment settings or the test-mode loop wrong produces silent undercharging or a wallet that never credits, and neither shows up in a unit test.
---

# Running the Stripe account

What an operator sets up, and what a developer runs locally. Written for
this component's surface: **one-off top-ups, refunds, and a saved method
for auto-recharge.** Not subscriptions.

## One account, several endpoints

`pay` is a library each product embeds, so several services take payment
against **one Stripe account**. Stripe allows many webhook endpoints, so
each product registers its own with its own signing secret. A product's
secret must never be shared with another: a leaked secret then forges
deliveries only for the service it belongs to.

## Products and prices

There are none, and that is deliberate. A credit top-up is an arbitrary
amount a person chooses, so checkout is created with inline `price_data`
carrying the amount, not a pre-created Price. Pre-created Prices exist
for fixed-value SKUs; a wallet has no SKU.

The consequence for the dashboard: nothing to configure, and nothing that
drifts out of sync with the code.

## Webhook events

Subscribe to exactly these. Anything else is noise that the adapter
reduces to `KindIgnored` and the handler acknowledges.

| Event | Why |
|---|---|
| `checkout.session.completed` | The synchronous card path. Credit only when `payment_status == paid` |
| `checkout.session.async_payment_succeeded` | SEPA, iDEAL, Bancontact. `completed` arrives unpaid and this confirms later |
| `charge.refunded` | Reverse the credit |
| `charge.dispute.created` | Reverse the credit |
| `payment_intent.payment_failed` | Auto-recharge telemetry; not a ledger write |

The first two are the pair that make an async purchase credit **once**:
`completed` for a SEPA payment is not `paid`, so it is ignored, and the
`async_payment_succeeded` that follows carries the same payment intent
the ledger dedupes on. Subscribing to only one of them is a live bug in
either direction: drop the first and a card purchase never credits; drop
the second and an EU bank transfer never does.

## Account settings that change what a customer is charged

Two, both default-on for new accounts, both able to make the amount
charged differ from the amount quoted:

- **Managed Payments.** Must be off. It requires a product tax code and
  adds tax on top of the total, so the customer pays more than the app
  said. The adapter disables it per session
  (`managed_payments[enabled]=false`) rather than relying on the account
  setting, because an account default can be changed in the dashboard by
  someone who does not know what it breaks.
- **Adaptive Pricing.** Leave on. It is how a EUR customer sees euros
  while the charge is created in USD, and it is why the ledger never
  holds a non-USD amount. The USD a EUR charge is actually worth comes
  from the charge's balance transaction, not from a rate we look up.

Automatic Tax stays **off** until a VAT spec says otherwise. When it is
turned on, `TaxMode` on the checkout params is what carries the change,
and the tax figures arrive on the `Event`.

## The local loop

Stripe cannot reach `localhost`, so the CLI forwards deliveries:

```bash
stripe login
stripe listen --forward-to localhost:8080/webhooks/stripe
```

The first line of output is a **CLI-only signing secret** (`whsec_...`),
separate from any dashboard endpoint's. Use it as the local webhook
secret. No dashboard endpoint is needed for local work at all.

Synthetic events exercise the real code path, HMAC and all, without
paying:

```bash
stripe trigger checkout.session.completed
stripe trigger charge.refunded
```

## Test cards

Test mode only; they never move real money.

| Card | Behaviour |
|---|---|
| `4242 4242 4242 4242` | Succeeds immediately |
| `4000 0025 0000 3155` | 3D Secure challenge, which is the `requires_action` path |
| `4000 0000 0000 0002` | Declined, which must map to `ErrDeclined` and never retry |
| `4100 0000 0000 0019` | Radar fraud block |

Any future expiry, any CVC. The 3DS card is the one worth wiring into an
end-to-end test: `ChargeSaved` returning `ChargePending` is the branch
most likely to be written wrong and never exercised.

## Rollout

1. **Unconfigured.** No keys. Every operation returns `ErrUnconfigured`,
   the service boots, and nothing sells. This is the default and it is
   what every test that is not about payment runs against.
2. **Test mode.** Test keys, CLI forwarding, buy credit with `4242`,
   assert the balance moved by exactly `Spread.Credited(gross)`.
3. **Live mode.** New keys, a new webhook endpoint, a new signing secret.
   Test-mode and live-mode secrets are different; promoting by changing
   only the API key leaves webhooks failing signature verification, which
   presents as purchases that charge the customer and never credit.

Before live: confirm the ledger's outstanding total matches what the
Stripe account holds. That reconciliation is the whole point of
`TotalOutstanding`.

## Operator recovery

- **A delivery failed.** Stripe retries on its own schedule. The handler
  returns 500 only for errors worth retrying and 200 for anything it
  cannot act on, so a poison event is acknowledged rather than retried
  forever.
- **A purchase charged but never credited.** Look up the payment intent
  in the ledger by `ref`. If absent, replay the delivery from the Stripe
  dashboard; idempotency makes a duplicate replay harmless.
- **Never** credit a wallet by hand to fix a webhook problem. The ledger's
  idempotency is keyed on the processor's reference; a manual entry has
  none, so the real delivery will credit again when it lands.

## Secrets

Two per product: the API key and that product's webhook signing secret.
There is no publishable key to deploy, because checkout is hosted and the
browser never talks to Stripe directly.

Terraform manages no Stripe resource anywhere in the family today, and
both existing secrets are provisioned out of band into k8s Secrets named
in no manifest. `pay` should not inherit that silence: either a terraform
resource, or a written procedure. Not neither.
