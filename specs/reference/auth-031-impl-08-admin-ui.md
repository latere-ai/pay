---
title: Billing — admin UI tab
status: complete
depends_on:
  - specs/.archive/029-impl-06-stripe-webhook.md
  - specs/.archive/030-impl-07-meter-push-worker.md
  - specs/.archive/021-admin-ui.md
affects:
  - frontend/src/views/AdminView.vue
  - frontend/src/components/AdminBilling.vue (new)
  - internal/handler/admin_billing.go
created: 2026-05-02
updated: 2026-05-08
author: codex
---

# Admin UI billing tab

Operator visibility for billing state. Lives inside the existing
admin UI (specs/.archive/021-admin-ui.md), session-cookie + superadmin gate.

> **Stale on the UI side (noted 2026-05-08).** Original `affects`
> listed `ui/templates/admin.html` and `ui/static/admin.js`,
> both deleted in the Vue SPA migration
> (specs/.archive/033-frontend-spa.md). The Go endpoints below apply
> unchanged. The UI surface is now a new Vue component
> (`frontend/src/components/AdminBilling.vue`) registered as a
> tab inside `frontend/src/views/AdminView.vue`'s switch,
> mirroring how Users / Orgs / Sessions tabs work today. The
> `affects` list above already reflects this.

## Routes (admin-only)

```go
mux.HandleFunc("GET    /admin/billing/orgs",                 superadmin(h.handleAdminListBillingOrgs))
mux.HandleFunc("GET    /admin/billing/orgs/{org_id}",        superadmin(h.handleAdminGetBillingOrg))
mux.HandleFunc("GET    /admin/billing/webhooks",             superadmin(h.handleAdminListWebhooks))
mux.HandleFunc("POST   /admin/billing/webhooks/{id}/retry",  superadmin(h.handleAdminRetryWebhook))
mux.HandleFunc("GET    /admin/billing/meter-pushes",         superadmin(h.handleAdminListMeterPushes))
mux.HandleFunc("POST   /admin/billing/meter-pushes/{key}/retry", superadmin(h.handleAdminRetryMeterPush))
```

All session-cookie + superadmin (same pattern as
`/admin/orgs`).

## Views

### `GET /admin/billing/orgs`

Filterable list joining `organizations`, `billing_customers`,
`billing_subscriptions`. Filters by `status`, `plan`, search by
org name or `stripe_customer_id`.

Response columns:

- Org name, ID
- Plan (string)
- Status
- Period start / end
- `cancel_at_period_end`
- `payment_method_attached`
- Stripe Customer ID (with deep-link to Stripe Dashboard:
  `https://dashboard.stripe.com/customers/<cust>`)
- Last meter-push: time + status (ok / error / pending)

### `GET /admin/billing/orgs/{org_id}`

Detail page: full subscription state, last 20 webhook events
for this org's customer, last 20 meter pushes for this org.

### `GET /admin/billing/webhooks`

Recent events. Filter by `processed_at IS NULL`, `error IS NOT
NULL`, type. Show raw payload on click.

### `POST /admin/billing/webhooks/{id}/retry`

Clears `processed_at` and `error`, then synchronously calls the
dispatcher (impl-06) for that event. Returns the new state.

### `POST /admin/billing/meter-pushes/{key}/retry`

Only valid for rows with `error LIKE 'permanent:%'`. Clears
`pushed_at` and `error`; the worker picks it up next tick.
Permanent-error retry exists for "ops fixed the Stripe meter
config" recovery.

## Stripe dashboard deep-links

Format strings (mode-aware):

- Test mode customer: `https://dashboard.stripe.com/test/customers/<id>`
- Live mode customer: `https://dashboard.stripe.com/customers/<id>`
- Subscription: `.../subscriptions/<id>`

Pull mode from `STRIPE_SECRET_KEY` prefix (`sk_test_` vs `sk_live_`).

## UI

Extend `ui/templates/admin.html`'s sidebar with a "Billing"
entry. New JS module `ui/static/admin/billing.js` calls the
admin endpoints above. No build step (matches admin-ui.md).

## Mock mode

In mock mode, deep-links point to a placeholder
(`https://billing.mock/customer/<id>`); the rest of the UI
works the same.

## Tests

`internal/handler/admin_billing_test.go`:

- Non-superadmin → 403.
- List filters (status=active, plan=pro) return correct rows.
- Detail page joins all three tables.
- Retry webhook re-dispatches; idempotent if already processed.
- Retry meter-push only works for permanent-error rows.

## Acceptance

- An operator can find a paid org and verify subscription state
  against Stripe via deep-link.
- Failed webhooks and stuck meter pushes are visible and
  retryable from the UI.
- The billing tab is gated to superadmins (same gate as the
  rest of `/admin`).
