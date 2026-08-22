---
title: Billing — rollout, configuration, and pilot
status: complete
depends_on:
  - specs/.archive/024-impl-01-persistence.md
  - specs/.archive/025-impl-02-mock-service.md
  - specs/.archive/026-impl-03-user-handlers.md
  - specs/.archive/027-impl-04-internal-handlers.md
  - specs/.archive/028-impl-05-stripe-checkout-portal.md
  - specs/.archive/029-impl-06-stripe-webhook.md
  - specs/.archive/030-impl-07-meter-push-worker.md
  - specs/.archive/031-impl-08-admin-ui.md
affects:
  - cmd/latere-auth/main.go
  - deploy/
  - INTEGRATION.md
created: 2026-05-02
author: codex
---

# Rollout — config, deploy, pilot

Brings the impl tree to production. Nothing in this spec
introduces new schemas or wire shapes; it's the
config / sequencing layer.

## Configuration matrix

| Var | Required prod | Default | Owner | Meaning |
|---|---|---|---|---|
| `STRIPE_SECRET_KEY`           | yes | `""` (mock) | ops | Stripe API secret. Empty = mock mode. |
| `STRIPE_WEBHOOK_SECRET`       | yes | `""`        | ops | Webhook HMAC secret. Empty = mock mode. |
| `BILLING_CHECKOUT_SUCCESS_URL`| yes | —           | ops | Cella URL after successful Checkout. |
| `BILLING_CHECKOUT_CANCEL_URL` | yes | —           | ops | Cella URL on Checkout cancel. |
| `BILLING_PORTAL_RETURN_URL`   | yes | —           | ops | Cella URL on Portal exit. |
| `BILLING_PLAN_PRICES_JSON`    | yes | `{}`        | ops | `{plan: stripe_price_id}` map. |
| `BILLING_WORKER_INTERVAL`     | no  | `30s`       | ops | Meter-push drain tick. |
| `BILLING_ADMIN_DEEPLINK_MODE` | no  | derived     | ops | `test` vs `live`; auto-detected from `STRIPE_SECRET_KEY` prefix. |

`BILLING_PLAN_PRICES_JSON` maps must match the plan-name strings
in cella's catalog (`migrations/000012_plans.up.sql` in
sandbox). Mismatch surfaces as `400 unknown_plan` at Checkout —
add a startup-time validation that warns (not fatal) if the env
keys don't match a curated allowlist.

## Cella service-account credential

Cella's deploy needs a JWT for the `cella-billing` service
account (impl-04). Same provisioning path used today for the
`sandboxd` service account.

Add to cella's deploy env:

- `BILLING_AUTH_BASE_URL` — auth's URL (e.g.
  `https://auth.latere.ai`).
- `BILLING_AUTH_TOKEN` — the JWT (or the OIDC client_credentials
  pair if cella refreshes its own).

## Webhook routing

Stripe Dashboard → Developers → Webhooks → add endpoint:

- URL: `https://auth.latere.ai/webhooks/stripe`
- Events:
  - `customer.subscription.created`
  - `customer.subscription.updated`
  - `customer.subscription.deleted`
  - `customer.updated`
  - `payment_method.attached`
  - `payment_method.detached`
  - `invoice.payment_failed` (logged only)

Copy the signing secret into `STRIPE_WEBHOOK_SECRET`.

## Single-replica constraint (v1)

The meter-push worker (impl-07) runs unguarded inside
`cmd/latere-auth/main.go`. v1 auth deploys as a single replica.
If/when ops scales horizontally, gate the worker with a
leader-lease (port the cella `internal/leader` package or use
`postgres-lock`).

This is a documented constraint, not a TODO. Cella's reporter
already does the right thing on its side.

## Rollout sequence

1. **Mock-mode prod deploy.** Ship impls 01-04 + 06 (mock-mode
   webhook ingest) + 08 (admin UI). `STRIPE_SECRET_KEY` and
   `STRIPE_WEBHOOK_SECRET` empty. Cella points at this. Verify
   the existing free-tier flow is unchanged. Verify admin UI
   renders empty tables.
2. **Stripe Test Mode pilot.** Ship impls 05 + 07. Set
   `STRIPE_SECRET_KEY=sk_test_...`,
   `STRIPE_WEBHOOK_SECRET=whsec_...`. Run the pilot org
   end-to-end (impl-05 "Pilot" section). Observe meter pushes
   in Stripe Dashboard.
3. **Pilot validation gate.** At least one full month of pilot-
   org usage rolls over correctly: cella reporter sends final
   delta of month N, then starts month N+1 from zero, and
   Stripe Invoice for month N matches cella's expected total.
4. **Live-mode cutover.** Swap to `sk_live_...` + matching
   webhook secret + live Price IDs. Move pilot org to live
   mode by recreating the subscription via Checkout.
5. **General availability.** Remove the
   `--billing-allow-unenforced-free-tier` cella override (if it
   ever shipped); cella refuses to start without a Usage reader.
6. **Decommission free-only mode** (optional, later): cella's
   billing entitlement becomes mandatory for new orgs. Existing
   free orgs are unaffected.

## Documentation

Update `auth/INTEGRATION.md` with the cella ↔ auth billing
contract, mirroring the impl-03/impl-04 wire shapes and the
service-account requirement.

Document the two operator workflows the admin UI exposes in
`auth/docs/stripe-setup.md`: "retry stuck webhook" and "retry
permanent meter-push error."

## Acceptance

- Auth boots in mock mode, Stripe Test Mode, and Stripe Live
  Mode without code changes — only env.
- Pilot org completes the Checkout → webhook → cella account
  lookup → sandbox create → reporter delta → meter-push →
  Stripe Invoice loop end-to-end.
- Operators can resolve at least one stuck webhook and one
  permanent-error meter push from the admin UI.
- Cella spec 60's "Acceptance" section passes against this
  deployment.
