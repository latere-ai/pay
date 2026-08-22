---
title: Billing — internal handlers + service-account scopes
status: complete
depends_on:
  - specs/.archive/025-impl-02-mock-service.md
  - specs/.archive/008-impl-06-handlers.md
affects:
  - internal/handler/billing_internal.go
  - internal/handler/router.go
  - internal/bootstrap/seed.go (or wherever service-account seed lives)
created: 2026-05-02
author: codex
---

# `/internal/billing/*` handlers + cella service account

Service-token routes that cella consumes. Mirrors the
`/internal/sandbox-tokens` pattern from spec 11.

> **Delta (2026-05-08):** add `GET /internal/billing/account/principal/{principal_id}`
> alongside the org-scoped variant, per `billing.md` §"Spec deltas"
> (two payer paths). Cella does not call this in Slice A — it
> exists so admin tooling and the `/me/billing` handler share one
> backing query. Permission: `billing:read` (any caller with the
> scope) plus a sub-match check for non-superadmin callers.

## Routes

```go
mux.HandleFunc("GET  /internal/billing/account/{org_id}",                    jwtMW(h.handleGetBillingAccount))
mux.HandleFunc("GET  /internal/billing/account/principal/{principal_id}",    jwtMW(h.handleGetBillingAccountPrincipal))
mux.HandleFunc("POST /internal/billing/usage",                                jwtMW(h.handleStageBillingUsage))
```

All three gated by JWT with the appropriate scope (see below).
Both `account` handlers share a single internal helper that takes
a `Payer` and produces the wire `Account` shape.

## Scopes

Add to the scope vocabulary used by `jwtMW`/`hasScope`:

- `billing:read` — `GET /internal/billing/account/{org_id}`.
- `billing:report` — `POST /internal/billing/usage`.

Existing scopes like `sandboxd:mint-token` set the precedent.

## Service-account seed

Add a `cella-billing` service account row in the bootstrap seed
(adjust path to wherever the existing `sandboxd` service account
is seeded):

```sql
INSERT INTO service_accounts (id, name, scopes, created_at)
VALUES (
  '00000000-0000-0000-0000-00000000B111',
  'cella-billing',
  ARRAY['billing:read','billing:report'],
  now()
);
```

Cella's deploy fetches a JWT for this account through whatever
mechanism the `sandboxd` service account uses today. Cella's
existing `BillingAuth.Token` config receives that JWT.

`/me/billing/*` handlers (impl-03) reject any JWT with
`principal_type == "service_account"` regardless of scopes, so a
leaked `billing:read` token cannot be repurposed as a Checkout-
session creator.

## `handleGetBillingAccount`

Path: `GET /internal/billing/account/{org_id}`. Scope:
`billing:read`. Validates `org_id` is a UUID; calls
`billing.GetAccount(orgID)`; returns the **flat** wire shape
spec'd in `auth/specs/.archive/billing.md`. `404` if no subscription row.

```go
type accountResp struct {
    OrgID                 string    `json:"org_id"`
    Plan                  string    `json:"plan"`
    Status                string    `json:"status"`
    PaymentMethodAttached bool      `json:"payment_method_attached"`
    PeriodStart           time.Time `json:"period_start"`
    PeriodEnd             time.Time `json:"period_end"`
    CheckoutURL           string    `json:"checkout_url,omitempty"`
    PortalURL             string    `json:"portal_url,omitempty"`
}
```

`CheckoutURL`/`PortalURL` are **only set when the active org has
no payment method attached** (so the dashboard can prompt
upgrade). For active paid subs they are empty strings; cella
synthesizes the link via `/me/billing/portal` on the user's own
session.

## `handleStageBillingUsage`

Path: `POST /internal/billing/usage`. Scope: `billing:report`.

Body:

```json
{
  "org_id": "uuid",
  "kind": "sandbox.seconds",
  "meter_id": "mtr_...",
  "quantity": 1234,
  "period_start": "RFC3339",
  "idempotency_key": "<org>:<kind>:<period>:<seq>"
}
```

Validation:

- `org_id` parses; otherwise `400 bad_request`.
- `meter_id` non-empty; otherwise `400 missing_meter`.
- `quantity` is a non-negative integer; otherwise
  `400 invalid_quantity`. (Cella multiplies floats up by the
  meter's resolution before sending.)
- `period_start` is UTC; we don't enforce alignment to month
  boundaries (Stripe Meter periods may not match calendar
  months in some plan configs).
- `idempotency_key` length ≤ 200 chars and matches a permissive
  regex (no whitespace).

Behavior:

- `StageMeterPush` returns `(created bool, existing MeterPush, err)`.
- `created==true` → `202 Accepted {"queued": true}`.
- `created==false` → `200 OK {"queued": false, "already_pushed_at": "..."}`
  (`already_pushed_at` is `null` if not yet drained).

The push worker (impl-07) drains `WHERE pushed_at IS NULL`. This
handler does **not** call Stripe synchronously.

## Tests

`internal/handler/billing_internal_test.go`:

- `GET /internal/billing/account/{org_id}`:
  - 200 with row.
  - 404 with no row.
  - 403 without `billing:read` scope.
  - 400 on bad UUID.
- `POST /internal/billing/usage`:
  - 202 on first call; 200 on replay.
  - 400 on missing/invalid fields.
  - 403 without `billing:report` scope.
  - Non-`service_account` principals are rejected (defence in
    depth).

## Acceptance

- Cella's `BillingAuth.Account` round-trips against this handler.
- Cella's `BillingAuth.ReportUsage` round-trips, with the new
  delta-shaped idempotency key from sandbox spec 60.
- `cella-billing` service account exists in seed and the integration
  test suite uses it.
