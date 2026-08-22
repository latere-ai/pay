# Stripe Setup Guide

Step-by-step from a fresh Stripe account to a working end-to-end
billing flow against the auth service. Aimed at developers wiring
this up for the first time.

This is the single source of truth for billing setup and operations:
Stripe configuration, remote Kubernetes env, rollout, smoke tests,
and recovery workflows. For the design rationale and wire contract see
[`../specs/.archive/023-billing.md`](../specs/.archive/023-billing.md).
Stripe's current docs for the moving parts are:
[products and prices](https://docs.stripe.com/products-prices/manage-prices),
[webhook endpoints](https://docs.stripe.com/development/dashboard/webhooks),
[Customer Portal](https://docs.stripe.com/customer-management/configure-portal),
[meter events](https://docs.stripe.com/api/billing/meter-event/create),
and [API keys](https://docs.stripe.com/keys).

## Why you need this

Out of the box, auth runs in **mock mode**: `STRIPE_SECRET_KEY`
is empty, the mock service mints fake `https://billing.mock/...`
URLs, and no real network calls happen. That's fine for unit
tests and cella's CI, but you can't actually take money or
exercise real Checkout / PayPal / webhook flows.

To do that you need a Stripe account. The good news: Stripe Test
Mode is **free, immediately usable, and does not require submitting
any business information**. You can have a working test flow in
about 15 minutes.

## Stripe-side checklist

In Stripe, you need to configure exactly five things before auth can
leave mock mode:

1. **Products + Prices** for every plan name auth accepts, e.g.
   `pro` → `price_...`.
2. **Billing Meters** for usage-based charges, with `event_name`
   values matching what cella sends as `meter_id` in
   `/internal/billing/usage`.
3. **Webhook endpoint** pointing at auth:
   `https://auth.latere.ai/webhooks/stripe`.
4. **Customer Portal** settings so users can manage payment methods,
   subscriptions, billing address, and invoice history.
5. **API keys**: the Stripe secret key (`sk_test_...` first), plus
   the webhook signing secret (`whsec_...`) from the endpoint page.

Do not create pilot subscriptions manually in Stripe. Use the
Latere billing UI / auth Checkout flow so auth creates the Stripe
Customer, stores the `billing_customers` row, and receives the
subscription webhook back into `billing_subscriptions`.

---

## 1. Create a Stripe account

1. Go to <https://stripe.com> → **Start now**.
2. Email + password is enough. Pick a country (any).
3. You land in the dashboard. The toggle in the top-right reads
   **"Test mode"** with an orange indicator. Stay here for
   everything below — do not switch to live until you're ready
   to take real money.

You don't need to submit business details, link a bank account,
or activate the account for test mode. Live mode requires those;
test mode does not.

---

## 2. Create the Products and Prices

Each plan name in `BILLING_PLAN_PRICES_JSON` must map to a Stripe
**Price ID**. A Price belongs to a Product. Create one Product per
plan, then one Price per Product.

For a basic recurring "Pro" plan:

1. Dashboard → **Product catalog** → **+ Add product**.
2. Name: `Pro`. Optional: description, image.
3. **Pricing**: Recurring, monthly (or yearly), pick an amount.
4. Save → opens the Product page. Click the price row → copy the
   `price_xxx` ID at the top of the side panel.

Repeat for any other plans. You'll plug them into the env var
later as a JSON map:

```json
{"pro":"price_AAAA...","enterprise":"price_BBBB..."}
```

For usage-based billing, also create a **Billing Meter** and a
usage-based recurring Price. The meter's `event_name` is what auth
pushes to Stripe as the meter event name; in our wire contract,
cella sends that value as `meter_id` in
`POST /internal/billing/usage`.

Auth's meter-event payload uses Stripe's default meter payload keys:

```json
{
  "stripe_customer_id": "cus_...",
  "value": "1234"
}
```

So leave the meter customer mapping at `stripe_customer_id` and the
value mapping at `value`, unless you also change
`internal/billing/stripeapi/client.go`. v1 attaches the relevant
metered Prices to the Product as default subscription items, so
Checkout picks them up automatically — no extra wiring per Checkout
call.

---

## 3. Create the Webhook endpoint

The webhook is how subscription state actually lands in our DB.
Without it, `customer.subscription.created` events never reach
auth and `billing_subscriptions` stays empty after Checkout.

1. Dashboard → Developers / Workbench → **Webhooks** →
   **Create event destination** / **+ Add endpoint**.
2. Endpoint URL: `https://auth.<your-domain>/webhooks/stripe`.
   For local development see [§7 Stripe CLI](#7-local-development-with-the-stripe-cli)
   below — don't use `localhost` here.
   For production today, use:

   ```text
   https://auth.latere.ai/webhooks/stripe
   ```

3. Select events to listen to:
   - `customer.subscription.created`
   - `customer.subscription.updated`
   - `customer.subscription.deleted`
   - `customer.updated`
   - `payment_method.attached`
   - `payment_method.detached`
   - `invoice.payment_failed`
4. **Add endpoint** → opens the endpoint page.
5. Click **Signing secret** → reveal → copy. Starts with `whsec_...`.
   This is `STRIPE_WEBHOOK_SECRET`.

Auth verifies every webhook against this secret. Empty/wrong
secret → 400 `bad_signature` → Stripe retries (visible in the
Webhook attempts log).

---

## 4. Configure the Customer Portal

Customers manage their own subscription/payment method via Stripe
Customer Portal (`POST /me/billing/portal` returns the URL). You
control what they can do.

1. Dashboard → Settings → **Billing** → **Customer portal**.
2. Recommended toggles:
   - ✅ Customers can update their payment method.
   - ✅ Customers can cancel subscriptions (cancel at period end).
   - ✅ Customers can update billing address.
   - ✅ Customers can view billing history.
3. Set a **Privacy policy** and **Terms of service** URL (Stripe
   shows them in the portal footer).
4. Save.

---

## 5. Get your secret key

1. Dashboard → Developers → **API keys**.
2. **Secret key** row → **Reveal test key**. Starts with `sk_test_...`.
   This is `STRIPE_SECRET_KEY`.

The publishable key (`pk_test_...`) is for client-side Stripe.js
work; we don't use it because Checkout / Portal are hosted by
Stripe (you redirect there, you don't embed forms). If you later
move to Stripe Elements for first-party card UX, the publishable
key gets used then.

---

## 6. Set the auth env vars and restart

Required for Stripe Test mode:

```bash
STRIPE_SECRET_KEY=sk_test_...
STRIPE_WEBHOOK_SECRET=whsec_...
BILLING_PLAN_PRICES_JSON='{"pro":"price_AAAA...","enterprise":"price_BBBB..."}'
BILLING_CHECKOUT_SUCCESS_URL=https://cella.<your-domain>/billing/success
BILLING_CHECKOUT_CANCEL_URL=https://cella.<your-domain>/billing/cancel
BILLING_PORTAL_RETURN_URL=https://<your-domain>/me/billing
```

Optional (defaults are fine for most setups):

```bash
BILLING_WORKER_INTERVAL=30s             # meter-push drain tick
CELLA_BILLING_CLIENT_SECRET=...         # shared with cella's billing client
```

For the remote `latere` namespace, auth reads most runtime config
from the `auth-config` Secret. Patch only `stringData`; Kubernetes
will encode it for you:

```bash
kubectl -n latere patch secret auth-config --type merge -p '{
  "stringData": {
    "STRIPE_SECRET_KEY": "sk_test_...",
    "STRIPE_WEBHOOK_SECRET": "whsec_...",
    "BILLING_PLAN_PRICES_JSON": "{\"pro\":\"price_...\"}",
    "BILLING_CHECKOUT_SUCCESS_URL": "https://cella.latere.ai/billing/success",
    "BILLING_CHECKOUT_CANCEL_URL": "https://cella.latere.ai/billing/cancel",
    "BILLING_PORTAL_RETURN_URL": "https://auth.latere.ai/me/billing",
    "CELLA_BILLING_CLIENT_SECRET": "..."
  }
}'

kubectl -n latere rollout restart deployment/auth
kubectl -n latere rollout status deployment/auth
```

Use a stable `CELLA_BILLING_CLIENT_SECRET` and configure the same
value on cella. If this env var is absent, auth can generate a
secret once, but that is hard to recover later and should not be the
remote production path.

Cella authenticates to `/internal/billing/*` with the
`cella-billing` OAuth client. Configure the Cella deployment with:

```bash
BILLING_AUTH_BASE_URL=https://auth.latere.ai
BILLING_AUTH_CLIENT_ID=cella-billing
BILLING_AUTH_CLIENT_SECRET=<same value as CELLA_BILLING_CLIENT_SECRET>
```

Cella exchanges those credentials with auth using the standard
`client_credentials` flow, then calls:

- `POST /internal/billing/usage` with `billing:report`.

The read-side `GET /internal/billing/account/...` endpoints and their
`billing:read` scope were retired (identity-fabric if-10); no product
ever shipped a caller. The entitlement-read contract moves to billing
phase 2 (`../specs/060-billing-redesign.md`).

Restart auth. Startup logs include validator warnings if any
required-when-Stripe-is-set var is missing — for example:

```
WARN  billing: STRIPE_SECRET_KEY is set but BILLING_PLAN_PRICES_JSON
      is empty — every Checkout will 400 unknown_plan
```

The Admin → Billing tab badge will switch from grey **MOCK MODE**
to orange **STRIPE TEST**.

Verify the remote config without printing secret values:

```bash
kubectl -n latere get secret auth-config -o go-template='{{range $k,$v := .data}}{{if or (eq $k "STRIPE_SECRET_KEY") (eq $k "STRIPE_WEBHOOK_SECRET") (eq $k "BILLING_PLAN_PRICES_JSON") (eq $k "BILLING_CHECKOUT_SUCCESS_URL") (eq $k "BILLING_CHECKOUT_CANCEL_URL") (eq $k "BILLING_PORTAL_RETURN_URL") (eq $k "CELLA_BILLING_CLIENT_SECRET")}}{{printf "%s present=true nonempty=%t\n" $k (ne $v "")}}{{end}}{{end}}'
```

Then check for startup warnings:

```bash
kubectl -n latere logs deployment/auth --since=10m | rg 'billing|Stripe|WARN|ERROR'
```

If the admin badge still says **MOCK MODE**, `STRIPE_SECRET_KEY` is
not present in the running pod.

---

## 7. Local development with the Stripe CLI

Stripe can't reach `localhost`, so for local development install
the Stripe CLI which forwards webhook events from Stripe's servers
to your local auth process.

```bash
brew install stripe/stripe-cli/stripe                   # macOS
stripe login                                            # one-time browser auth

# In a separate terminal, while auth is running:
stripe listen --forward-to localhost:8080/webhooks/stripe
```

The first line of the `listen` output is a **CLI-only signing
secret**:

```
> Ready! Your webhook signing secret is whsec_local_xxx (^C to quit)
```

Use this `whsec_local_xxx` as your local `STRIPE_WEBHOOK_SECRET`
— it's separate from the dashboard endpoint's secret. (You don't
need an endpoint configured in the dashboard at all for local
development; the CLI proxies events directly to you.)

You can also fire synthetic events without going through Checkout:

```bash
stripe trigger customer.subscription.created
stripe trigger invoice.payment_failed
stripe trigger payment_method.attached
```

These hit your local auth, get HMAC-verified, and apply via the
dispatcher — exactly the same code path as production Stripe
events. Useful for testing failure modes without manually paying.

---

## 8. Test cards (test mode only)

Stripe ships fixed test card numbers. They never charge anything
real and only work in test mode.

| Card number | Behavior |
|---|---|
| `4242 4242 4242 4242` | Succeeds immediately. |
| `4000 0025 0000 3155` | Triggers a 3D Secure challenge. |
| `4000 0000 0000 9995` | Succeeds, then **fails on the next renewal** — good for testing `invoice.payment_failed` → `past_due`. |
| `4000 0000 0000 0002` | Declined immediately. |
| `4100 0000 0000 0019` | Triggers Stripe Radar fraud block. |

Any future expiry date, any CVC, any zip code. Full list at
<https://stripe.com/docs/testing>.

For PayPal: clicking the PayPal button in test-mode Checkout opens
the PayPal sandbox flow — no real PayPal account needed. Stripe
auto-creates a sandbox payer for you.

---

## 9. End-to-end smoke test

After steps 1–6 (and optionally 7 for local), you should be able
to:

1. Sign in as any test user that's a member of an org.
2. Open `/me/billing` (or `/orgs/:id/billing` for org-payer).
3. Click **+ Add payment method** (calls `POST /me/billing/setup`).
   You're redirected to Stripe Checkout in setup mode.
4. Pick **Card**, enter `4242 4242 4242 4242` + any future expiry
   + any CVC + any zip. Submit.
5. Stripe redirects to your `BILLING_CHECKOUT_SUCCESS_URL`.
6. Within a second, the `payment_method.attached` webhook fires;
   auth's `billing_customers.payment_method_attached` flips to
   `true`. The PaymentView's "ON FILE" badge should appear after
   a refresh.
7. Click **Subscribe to Pro** (calls `POST /me/billing/checkout`).
   Pay again with the test card. After redirect, the
   `customer.subscription.created` webhook lands and PaymentView
   shows the active subscription block.
8. Open Admin → **Billing** tab. The org row shows status `active`
   with a deep-link to the Stripe Dashboard customer.
9. Click **Manage on Stripe** in PaymentView → opens Customer
   Portal in a new tab → cancel the subscription → return →
   `customer.subscription.deleted` event lands → row preserved
   with `status="canceled"` (admin trail intact).

If any step doesn't work, the Admin → Billing tab's **Failed
webhooks** section tells you what went wrong; the **Retry** button
re-applies after you fix the root cause.

---

## 10. Rollout sequence

### Phase 1 — Mock mode

Keep `STRIPE_SECRET_KEY` and `STRIPE_WEBHOOK_SECRET` empty. Auth
uses the in-process mock service, Cella sees no subscription row,
and product policy should treat every org as free.

Verify:

- `/api/me/billing` returns `subscription:null` for users with no
  billing customer.
- Admin → Billing shows **MOCK MODE**.
- Cella's mock/e2e harness passes.

### Phase 2 — Stripe test-mode pilot

Set `STRIPE_SECRET_KEY=sk_test_...`, `STRIPE_WEBHOOK_SECRET`,
checkout/portal URLs, and `BILLING_PLAN_PRICES_JSON`.

Run one pilot org end-to-end:

1. Create or choose a pilot org.
2. Subscribe through Latere's billing UI, not the Stripe Dashboard.
3. Pay with a Stripe test card.
4. Confirm `customer.subscription.created` lands and the org shows
   `status="active"` in Admin → Billing.
5. Run a Cella sandbox so Cella posts usage to auth.
6. Confirm the auth worker forwards a Stripe Meter event.

### Phase 3 — Pilot validation gate

Before live mode, validate at least one complete billing period:

- Cella sends the final delta for period N.
- Cella starts period N+1 from zero.
- Stripe's invoice for period N matches Cella's expected usage.

### Phase 4 — Live-mode cutover

Live mode is a separate Stripe environment. Recreate products,
prices, meters, webhook endpoint, and Customer Portal settings in
live mode before swapping env vars.

---

## 11. Operator workflows

The Admin → Billing tab exposes the recovery controls operators
should normally need.

### Stuck webhook → retry

Symptoms: Stripe shows a subscription/payment event, but the local
`billing_subscriptions` row did not appear or did not update.

1. Open Admin → Billing.
2. Check **Failed webhooks**.
3. Read the error text.
4. Fix the root cause.
5. Click **Retry**.

Common root causes:

- The webhook arrived before auth had stored the matching
  `billing_customers` row. Retry after the customer row exists.
- Stripe sent a shape the dispatcher does not understand. File a bug;
  the dispatcher ignores API-version mismatches, but code can still
  miss fields.
- `STRIPE_WEBHOOK_SECRET` is wrong. Fix the env var and restart auth;
  Stripe will retry failed deliveries.

### Permanent meter-push error → retry

Symptoms: `billing_meter_pushes.pushed_at` is set and `error` is
non-empty. The worker considered the error permanent and stopped
retrying automatically.

1. Open Admin → Billing → **Stuck meter pushes**.
2. Read the error.
3. Fix Stripe or the auth/customer state.
4. Click **Retry**.

Common root causes:

- Unknown meter/event name: create the matching Stripe Billing Meter,
  or fix Cella's `meter_id`.
- Missing Stripe customer: usage was reported for an org that has not
  gone through Checkout. Have the org owner start Checkout, or scrub
  the invalid usage row.

Rows with `pushed_at = NULL` are already pending and should not be
manually retried; the worker retries them on its normal interval.

### Worker concurrency

The meter-push worker runs inside `cmd/auth` on every replica, with no
leader election, and auth deploys two replicas
(`deploy/base/deployment.yaml`). Stripe's meter-event `identifier`
dedupe and auth's `billing_meter_pushes.idempotency_key` primary key
protect billing correctness, so duplicate pushes cannot double-bill.
What the replicas do cost is redundant Stripe API quota and noisier
failures. Adding a leader lease or DB lock around the worker would
remove that duplication.

---

## 12. Promoting to live mode

Don't do this until the test-mode smoke test above runs cleanly
and you've validated at least one full subscription period
(typically a month for monthly plans).

When you're ready:

1. Stripe Dashboard → toggle to **Live mode** (top-right; turns
   green/blue). You'll be prompted to submit business details +
   bank account if you haven't already — required for receiving
   real money.
2. Re-do steps 2 (Products/Prices), 3 (Webhook endpoint), 4
   (Customer Portal). Live mode is a separate environment; nothing
   carries over from test mode.
3. Get a **live secret key** and **live webhook signing secret**.
4. Update auth's env vars: `sk_test_...` → `sk_live_...`,
   `whsec_test...` → `whsec_live...`, plan map to live
   `price_...` IDs.
5. Restart auth. Admin → Billing tab badge will read **LIVE**
   (red).
6. Existing pilot org subscriptions don't migrate automatically.
   The pilot user re-subscribes via Checkout in live mode.

The mode (mock / test / live) is auto-detected from the
`STRIPE_SECRET_KEY` prefix. No separate config flag.

---

## Appendix: env var reference

| Env var | Required for live | Default | Notes |
|---|---|---|---|
| `STRIPE_SECRET_KEY`             | yes | `""` (mock) | `sk_test_*` or `sk_live_*`. |
| `STRIPE_WEBHOOK_SECRET`         | yes | `""`        | Empty disables HMAC checks (mock mode only). |
| `BILLING_PLAN_PRICES_JSON`      | yes | `""`        | `{"plan":"price_..."}`. |
| `BILLING_CHECKOUT_SUCCESS_URL`  | yes | `""`        | Redirect after successful Checkout. |
| `BILLING_CHECKOUT_CANCEL_URL`   | yes | `""`        | Redirect after Checkout cancel. |
| `BILLING_PORTAL_RETURN_URL`     | yes | `""`        | Redirect after Customer Portal exit. |
| `BILLING_WORKER_INTERVAL`       | no  | `30s`       | Meter-push drain tick. `0` disables. |
| `BILLING_MOCK_BASE_URL`         | no  | `https://billing.mock` | Mock-mode URL host. |
| `CELLA_BILLING_CLIENT_SECRET`   | no  | generated   | cella ← auth service-account secret. |
| `CELLA_BILLING_SECRET_FILE`     | no  | `""`        | Where to write the auto-generated secret. |
